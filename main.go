//go:build windows
// +build windows

package main

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/getlantern/systray"
	"github.com/pkg/browser"
	_ "modernc.org/sqlite"

	"github.com/stevecastle/shrike/jobqueue"
	"github.com/stevecastle/shrike/media"
	"github.com/stevecastle/shrike/renderer"
	"github.com/stevecastle/shrike/runners"
	"github.com/stevecastle/shrike/stream"
)

// -----------------------------------------------------------------------------
// Embedded tray-icon (.ico) file – place your icon at assets/logo.ico.
// -----------------------------------------------------------------------------

//go:embed assets/logo.ico
var iconData []byte

// -----------------------------------------------------------------------------
// Embed static assets under client/static; ** must recurse all sub-paths.
// -----------------------------------------------------------------------------

//go:embed client/static/**
var embeddedStatic embed.FS

// staticFS is the embedded filesystem rooted at client/static/.
var staticFS fs.FS

// -----------------------------------------------------------------------------
// http server so we can shut it down cleanly from onExit.
// -----------------------------------------------------------------------------
var srv *http.Server

// Global dependencies variable so we can access it from onExit
var deps *Dependencies

// -----------------------------------------------------------------------------
// Dependencies struct to hold shared dependencies
// -----------------------------------------------------------------------------
type Dependencies struct {
	Queue *jobqueue.Queue
	DB    *sql.DB
}

// -----------------------------------------------------------------------------
// Utility – run from the folder that contains the executable so the templates
// and static files are found even when launched from elsewhere (during dev
// this still helps, but isn't strictly required for embedded files).
// -----------------------------------------------------------------------------
func init() {
	if exe, err := os.Executable(); err == nil {
		_ = os.Chdir(filepath.Dir(exe))
	}

	// Carve out the client/static subtree of the embedded FS so that
	// "/static/foo.js" maps directly to "foo.js".
	var err error
	staticFS, err = fs.Sub(embeddedStatic, "client/static")
	if err != nil {
		panic("shrike: fs.Sub failed: " + err.Error())
	}
}

// -----------------------------------------------------------------------------
// Database initialization
// -----------------------------------------------------------------------------

// Config represents the structure of the configuration file
type Config struct {
	DBPath string `json:"dbPath"`
	// Add other fields as needed, but we only need dbPath for now
}

func initDB() (*sql.DB, error) {
	// Get the AppData directory dynamically for the current Windows user
	appDataDir := os.Getenv("APPDATA")
	if appDataDir == "" {
		return nil, fmt.Errorf("APPDATA environment variable not found")
	}

	// Construct the path to the config file
	configPath := filepath.Join(appDataDir, "Lowkey Media Viewer", "config.json")

	// Read the config file
	configData, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file at %s: %v", configPath, err)
	}

	// Parse the JSON config
	var config Config
	if err := json.Unmarshal(configData, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config JSON: %v", err)
	}

	// Validate that dbPath is set
	if config.DBPath == "" {
		return nil, fmt.Errorf("dbPath not found in config file")
	}

	dbPath := config.DBPath
	log.Printf("Using database path from config: %s", dbPath)

	// Ensure the directory exists
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, fmt.Errorf("failed to create database directory: %v", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %v", err)
	}

	// Test the connection
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping database: %v", err)
	}

	log.Printf("Connected to SQLite database at: %s", dbPath)
	return db, nil
}

// -----------------------------------------------------------------------------
// Web-handler helpers
// -----------------------------------------------------------------------------

type ListTemplateData struct{ Jobs []jobqueue.Job }
type DetailTemplateData struct{ Job *jobqueue.Job }

type Command struct {
	Command   string
	Arguments []string
}

func homeHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// POST = legacy JSON workflow launch
		if r.Method == http.MethodPost {
			var c Command
			if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
				http.Error(w, "bad json", http.StatusBadRequest)
				return
			}
			workflow := jobqueue.Workflow{
				Command:   c.Command,
				Arguments: c.Arguments[:len(c.Arguments)-1],
				Input:     c.Arguments[len(c.Arguments)-1],
			}
			id, err := deps.Queue.AddWorkflow(workflow)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}

			// Send successful response for legacy POST
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
			return
		}

		// GET – render job list
		data := ListTemplateData{Jobs: deps.Queue.GetJobs()}
		if err := renderer.Templates().ExecuteTemplate(w, "home", data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

func detailHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		job := deps.Queue.GetJob(id)
		if job == nil {
			http.NotFound(w, r)
			return
		}
		data := DetailTemplateData{Job: job}
		if err := renderer.Templates().ExecuteTemplate(w, "detail", data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

type CreateJobHandlerRequest struct {
	Input string `json:"input"`
}

func createJobHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Use POST", http.StatusMethodNotAllowed)
			return
		}

		var req CreateJobHandlerRequest
		if err := readJSONBody(r, &req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}

		args := ParseCommand(req.Input)
		if len(args) == 0 {
			http.Error(w, "Invalid input", http.StatusBadRequest)
			return
		}

		cmd, input := args[0], ""
		if len(args) > 1 {
			input = args[len(args)-1]
			args = args[1 : len(args)-1]
		} else {
			args = nil
		}

		id, err := deps.Queue.AddWorkflow(jobqueue.Workflow{
			Command:   cmd,
			Arguments: args,
			Input:     input,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
	}
}

func cancelHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Use POST", http.StatusMethodNotAllowed)
			return
		}
		deps.Queue.CancelJob(r.PathValue("id"))

		// Send successful response
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Job cancelled successfully"))
	}
}

func copyHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Use POST", http.StatusMethodNotAllowed)
			return
		}
		newID, err := deps.Queue.CopyJob(r.PathValue("id"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// Send successful response with new job ID
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": newID, "message": "Job copied successfully"})
	}
}

func removeHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Use POST", http.StatusMethodNotAllowed)
			return
		}
		if err := deps.Queue.RemoveJob(r.PathValue("id")); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// Send successful response
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Job removed successfully"))
	}
}

func clearNonRunningJobsHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Use POST", http.StatusMethodNotAllowed)
			return
		}

		clearedCount, err := deps.Queue.ClearNonRunningJobs()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"cleared_count": clearedCount,
			"message":       fmt.Sprintf("Cleared %d non-running jobs", clearedCount),
		})
	}
}

func readJSONBody(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

// healthHandler provides system health information including stream connections
func healthHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Set fully permissive CORS headers
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Max-Age", "86400")

		// Handle preflight OPTIONS request
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		if r.Method != http.MethodGet {
			http.Error(w, "Use GET", http.StatusMethodNotAllowed)
			return
		}

		// Get stream connection statistics
		streamStats := stream.GetConnectionStats()

		// Get job queue statistics
		jobs := deps.Queue.GetJobs()
		jobStats := map[string]int{
			"total":       len(jobs),
			"pending":     0,
			"in_progress": 0,
			"completed":   0,
			"cancelled":   0,
			"error":       0,
		}

		for _, job := range jobs {
			switch job.State {
			case 0: // StatePending
				jobStats["pending"]++
			case 1: // StateInProgress
				jobStats["in_progress"]++
			case 2: // StateCompleted
				jobStats["completed"]++
			case 3: // StateCancelled
				jobStats["cancelled"]++
			case 4: // StateError
				jobStats["error"]++
			}
		}

		health := map[string]interface{}{
			"status":    "healthy",
			"timestamp": time.Now().Unix(),
			"stream":    streamStats,
			"jobs":      jobStats,
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(health); err != nil {
			log.Printf("Error encoding health response: %v", err)
		}
	}
}

// mediaHandler serves the main media browsing page
func mediaHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		const initialLimit = 25

		// Get search query from URL parameter
		searchQuery := r.URL.Query().Get("q")

		items, hasMore, err := media.GetItems(deps.DB, 0, initialLimit, searchQuery)
		if err != nil {
			log.Printf("Error fetching media items: %v", err)
			http.Error(w, "Error fetching media items", http.StatusInternalServerError)
			return
		}

		data := media.TemplateData{
			MediaItems:  items,
			Offset:      len(items),
			HasMore:     hasMore,
			SearchQuery: searchQuery,
		}

		if err := renderer.Templates().ExecuteTemplate(w, "media", data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// mediaAPIHandler serves the JSON API for infinite scroll
func mediaAPIHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Use GET", http.StatusMethodNotAllowed)
			return
		}

		// Parse query parameters
		offsetStr := r.URL.Query().Get("offset")
		limitStr := r.URL.Query().Get("limit")
		searchQuery := r.URL.Query().Get("q")
		pathQuery := r.URL.Query().Get("path")
		singleStr := r.URL.Query().Get("single")

		// Check if this is a single item request by path
		if pathQuery != "" && singleStr == "true" {
			// Handle single item lookup by path
			item, err := media.GetItemByPath(deps.DB, pathQuery)
			if err != nil {
				log.Printf("Error fetching media item by path '%s': %v", pathQuery, err)
				http.Error(w, "Error fetching media item", http.StatusInternalServerError)
				return
			}

			var items []media.MediaItem
			if item != nil {
				items = append(items, *item)
			}

			response := media.APIResponse{
				Items:   items,
				HasMore: false,
			}

			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(response); err != nil {
				log.Printf("Error encoding JSON response: %v", err)
			}
			return
		}

		// Handle regular pagination requests
		offset := 0
		limit := 25

		if offsetStr != "" {
			if parsed, err := strconv.Atoi(offsetStr); err == nil {
				offset = parsed
			}
		}

		if limitStr != "" {
			if parsed, err := strconv.Atoi(limitStr); err == nil && parsed > 0 && parsed <= 100 {
				limit = parsed
			}
		}

		items, hasMore, err := media.GetItems(deps.DB, offset, limit, searchQuery)
		if err != nil {
			log.Printf("Error fetching media items: %v", err)
			http.Error(w, "Error fetching media items", http.StatusInternalServerError)
			return
		}

		response := media.APIResponse{
			Items:   items,
			HasMore: hasMore,
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			log.Printf("Error encoding JSON response: %v", err)
		}
	}
}

// mediaFileHandler serves individual media files for preview
func mediaFileHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Use GET", http.StatusMethodNotAllowed)
			return
		}

		// Get the file path from query parameter
		encodedPath := r.URL.Query().Get("path")
		if encodedPath == "" {
			http.Error(w, "Missing path parameter", http.StatusBadRequest)
			return
		}

		// URL decode the path
		filePath, err := url.QueryUnescape(encodedPath)
		if err != nil {
			log.Printf("Error decoding path '%s': %v", encodedPath, err)
			http.Error(w, "Invalid path encoding", http.StatusBadRequest)
			return
		}

		// Validate and sanitize the file path
		filePath = strings.TrimSpace(filePath)
		if filePath == "" {
			http.Error(w, "Empty file path", http.StatusBadRequest)
			return
		}

		// Check path length (prevent extremely long paths)
		if len(filePath) > 1000 {
			http.Error(w, "Path too long", http.StatusBadRequest)
			return
		}

		// Basic security: prevent directory traversal attacks
		// Note: This is a basic check - for production, consider more robust validation
		if strings.Contains(filePath, "..") {
			log.Printf("Potential directory traversal attempt: %s", filePath)
			http.Error(w, "Invalid path", http.StatusBadRequest)
			return
		}

		// For remote URLs, proxy the request
		if strings.HasPrefix(filePath, "http://") || strings.HasPrefix(filePath, "https://") {
			proxyRemoteMedia(w, r, filePath)
			return
		}

		// Handle local files
		// Clean the path for consistency
		filePath = filepath.Clean(filePath)

		// Check if file exists
		if !media.CheckFileExists(filePath) {
			log.Printf("File not found: %s", filePath)
			http.Error(w, "File not found", http.StatusNotFound)
			return
		}

		// Get file info for additional validation
		fileInfo, err := os.Stat(filePath)
		if err != nil {
			log.Printf("Error getting file info for '%s': %v", filePath, err)
			http.Error(w, "Cannot access file", http.StatusInternalServerError)
			return
		}

		// Check if it's actually a file (not a directory)
		if fileInfo.IsDir() {
			http.Error(w, "Path is a directory", http.StatusBadRequest)
			return
		}

		// Check file size (prevent serving extremely large files for preview)
		const maxFileSize = 100 * 1024 * 1024 // 100MB limit
		if fileInfo.Size() > maxFileSize {
			http.Error(w, "File too large for preview", http.StatusRequestEntityTooLarge)
			return
		}

		// Set appropriate content type based on file extension
		ext := strings.ToLower(filepath.Ext(filePath))
		contentType := getContentType(ext)
		w.Header().Set("Content-Type", contentType)

		// Set cache headers for better performance
		w.Header().Set("Cache-Control", "public, max-age=3600") // Cache for 1 hour
		etag := fmt.Sprintf(`"%s-%d-%d"`, filepath.Base(filePath), fileInfo.Size(), fileInfo.ModTime().Unix())
		w.Header().Set("ETag", etag)

		// Check If-None-Match header for caching
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}

		// Set content length
		w.Header().Set("Content-Length", strconv.FormatInt(fileInfo.Size(), 10))

		// Serve the file
		http.ServeFile(w, r, filePath)
	}
}

// proxyRemoteMedia proxies remote media files with timeout and size limits
func proxyRemoteMedia(w http.ResponseWriter, r *http.Request, remoteURL string) {
	// Create a client with timeout
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	// Create request to remote URL
	req, err := http.NewRequest("GET", remoteURL, nil)
	if err != nil {
		log.Printf("Error creating request for remote URL '%s': %v", remoteURL, err)
		http.Error(w, "Invalid remote URL", http.StatusBadRequest)
		return
	}

	// Set User-Agent to identify our requests
	req.Header.Set("User-Agent", "Shrike-Media-Browser/1.0")

	// Make the request
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Error fetching remote media '%s': %v", remoteURL, err)
		http.Error(w, "Failed to fetch remote media", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Check response status
	if resp.StatusCode != http.StatusOK {
		log.Printf("Remote media returned status %d for URL: %s", resp.StatusCode, remoteURL)
		http.Error(w, "Remote media not accessible", resp.StatusCode)
		return
	}

	// Check content length if provided
	if contentLengthStr := resp.Header.Get("Content-Length"); contentLengthStr != "" {
		if contentLength, err := strconv.ParseInt(contentLengthStr, 10, 64); err == nil {
			const maxFileSize = 100 * 1024 * 1024 // 100MB limit
			if contentLength > maxFileSize {
				http.Error(w, "Remote file too large for preview", http.StatusRequestEntityTooLarge)
				return
			}
		}
	}

	// Copy headers from remote response
	for key, values := range resp.Header {
		if key == "Content-Type" || key == "Content-Length" || key == "Cache-Control" || key == "ETag" {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
	}

	// If no cache headers from remote, set our own
	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "public, max-age=1800") // 30 minutes for remote files
	}

	// Copy the response body with size limit
	const maxFileSize = 100 * 1024 * 1024 // 100MB limit
	limitedReader := &io.LimitedReader{R: resp.Body, N: maxFileSize}

	written, err := io.Copy(w, limitedReader)
	if err != nil {
		log.Printf("Error copying remote media response: %v", err)
		return
	}

	// Check if we hit the size limit
	if limitedReader.N <= 0 && written == maxFileSize {
		log.Printf("Remote file too large, truncated at %d bytes: %s", maxFileSize, remoteURL)
	}
}

// getContentType returns the appropriate MIME type for a file extension
func getContentType(ext string) string {
	switch ext {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	case ".svg":
		return "image/svg+xml"
	case ".tiff":
		return "image/tiff"
	case ".ico":
		return "image/x-icon"
	case ".mp4":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".ogg":
		return "video/ogg"
	case ".avi":
		return "video/x-msvideo"
	case ".mov":
		return "video/quicktime"
	case ".wmv":
		return "video/x-ms-wmv"
	case ".flv":
		return "video/x-flv"
	case ".mkv":
		return "video/x-matroska"
	case ".m4v":
		return "video/x-m4v"
	default:
		return "application/octet-stream"
	}
}

func ParseCommand(input string) []string {
	var (
		result   []string
		current  strings.Builder
		inQuotes bool
	)
	for i := 0; i < len(input); i++ {
		c := input[i]
		switch c {
		case '"':
			inQuotes = !inQuotes
		case ' ':
			if inQuotes {
				current.WriteByte(c)
			} else if current.Len() > 0 {
				result = append(result, current.String())
				current.Reset()
			}
		default:
			current.WriteByte(c)
		}
	}
	if current.Len() > 0 {
		result = append(result, current.String())
	}
	return result
}

// -----------------------------------------------------------------------------
// main – start server then hand control to the system-tray UI.
// -----------------------------------------------------------------------------

func main() {
	// ––– initialize database –––
	db, err := initDB()
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer db.Close()

	// ––– job queue and runners –––
	log.Println("Initializing job queue with database persistence...")
	queue := jobqueue.NewQueueWithDB(db)
	log.Printf("Job queue initialized. Current jobs: %d", len(queue.GetJobs()))
	runners.New(queue, 2)

	// ––– create dependencies struct –––
	deps = &Dependencies{
		Queue: queue,
		DB:    db,
	}

	// ––– routes –––
	mux := http.NewServeMux()
	mux.HandleFunc("/", renderer.ApplyMiddlewares(homeHandler(deps)))
	mux.HandleFunc("/job/{id}", renderer.ApplyMiddlewares(detailHandler(deps)))
	mux.HandleFunc("/job/{id}/cancel", renderer.ApplyMiddlewares(cancelHandler(deps)))
	mux.HandleFunc("/job/{id}/copy", renderer.ApplyMiddlewares(copyHandler(deps)))
	mux.HandleFunc("/job/{id}/remove", renderer.ApplyMiddlewares(removeHandler(deps)))
	mux.HandleFunc("/jobs/clear", renderer.ApplyMiddlewares(clearNonRunningJobsHandler(deps)))
	mux.HandleFunc("/stream", stream.StreamHandler)
	mux.HandleFunc("/health", healthHandler(deps))
	mux.HandleFunc("/create", renderer.ApplyMiddlewares(createJobHandler(deps)))
	mux.HandleFunc("/media", renderer.ApplyMiddlewares(mediaHandler(deps)))
	mux.HandleFunc("/media/api", renderer.ApplyMiddlewares(mediaAPIHandler(deps)))
	mux.HandleFunc("/media/file", renderer.ApplyMiddlewares(mediaFileHandler(deps)))

	// Serve embedded static files
	mux.Handle("/static/",
		http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	srv = &http.Server{
		Addr:    ":8090",
		Handler: mux,
	}

	// start HTTP server in background
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("shrike-server: %v", err)
		}
	}()

	// run tray icon (blocks until Quit)
	systray.Run(onReady, onExit)
}

// -----------------------------------------------------------------------------
// systray lifecycle hooks
// -----------------------------------------------------------------------------

func onReady() {
	systray.SetTemplateIcon(iconData, iconData)
	systray.SetTitle("Shrike Job Server")
	systray.SetTooltip("Shrike – click to open UI")

	openItem := systray.AddMenuItem("Open Web UI", "Launch the browser")
	systray.AddSeparator()
	quitItem := systray.AddMenuItem("Quit", "Shut down Shrike")

	// open UI once at startup
	_ = browser.OpenURL("http://localhost:8090/")

	// event loop
	for {
		select {
		case <-openItem.ClickedCh:
			_ = browser.OpenURL("http://localhost:8090/")
		case <-quitItem.ClickedCh:
			systray.Quit()
			return
		}
	}
}

func onExit() {
	log.Println("Shutting down Shrike server...")

	// Shutdown stream connections first
	log.Println("Shutting down stream connections...")
	stream.Shutdown()

	// Save all jobs to database before shutting down
	if deps != nil && deps.Queue != nil {
		log.Println("Saving job queue to database...")
		if err := deps.Queue.SaveAllJobsToDB(); err != nil {
			log.Printf("Error saving jobs to database: %v", err)
		} else {
			log.Println("Job queue saved successfully")
		}
	}

	// Shutdown HTTP server
	log.Println("Shutting down HTTP server...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	} else {
		log.Println("HTTP server shutdown complete")
	}

	log.Println("Shrike server shutdown complete")
}
