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
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/getlantern/systray"
	"github.com/pkg/browser"
	_ "modernc.org/sqlite"

	"github.com/stevecastle/shrike/appconfig"
	"github.com/stevecastle/shrike/jobqueue"
	"github.com/stevecastle/shrike/media"
	"github.com/stevecastle/shrike/renderer"
	"github.com/stevecastle/shrike/runners"
	"github.com/stevecastle/shrike/stream"
	"github.com/stevecastle/shrike/tasks"
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

// Keep a copy of the currently loaded config in memory
var currentConfig appconfig.Config

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

// switchDatabase switches the application's active database and queue to the provided path
func switchDatabase(newDBPath string) error {
	if newDBPath == "" {
		return fmt.Errorf("newDBPath cannot be empty")
	}

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(newDBPath), 0755); err != nil {
		return fmt.Errorf("failed to create database directory: %v", err)
	}

	// Open and ping the new DB first to validate
	newDB, err := sql.Open("sqlite", newDBPath)
	if err != nil {
		return fmt.Errorf("failed to open new database: %v", err)
	}
	if err := newDB.Ping(); err != nil {
		newDB.Close()
		return fmt.Errorf("failed to ping new database: %v", err)
	}

	// Prepare a new queue backed by the new DB
	newQueue := jobqueue.NewQueueWithDB(newDB)

	// Swap dependencies
	oldDB := deps.DB
	deps.DB = newDB
	deps.Queue = newQueue

	// Start runners for the new queue
	runners.New(newQueue, 1)

	// Close the old DB last
	if oldDB != nil {
		_ = oldDB.Close()
	}
	return nil
}

func initDB() (*sql.DB, error) {
	// Load config
	cfg, _, err := appconfig.Load()
	if err != nil {
		return nil, err
	}
	currentConfig = cfg
	dbPath := cfg.DBPath
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

	// Best-effort: ensure helpful indexes exist
	if err := ensureIndexes(db); err != nil {
		log.Printf("warning: failed to ensure indexes: %v", err)
	}

	log.Printf("Connected to SQLite database at: %s", dbPath)
	return db, nil
}

// ensureIndexes creates recommended indexes if the related tables exist.
func ensureIndexes(db *sql.DB) error {
	// Helper to detect if a table exists
	tableExists := func(name string) bool {
		var cnt int
		_ = db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&cnt)
		return cnt > 0
	}

	// Indexes for media_tag_by_category
	if tableExists("media_tag_by_category") {
		stmts := []string{
			"CREATE INDEX IF NOT EXISTS idx_mtbc_media_path ON media_tag_by_category(media_path)",
			"CREATE INDEX IF NOT EXISTS idx_mtbc_tag_label ON media_tag_by_category(tag_label)",
			"CREATE INDEX IF NOT EXISTS idx_mtbc_category_label ON media_tag_by_category(category_label)",
			"CREATE INDEX IF NOT EXISTS idx_mtbc_tag_category ON media_tag_by_category(tag_label, category_label)",
		}
		for _, s := range stmts {
			if _, err := db.Exec(s); err != nil {
				return fmt.Errorf("creating index failed: %w", err)
			}
		}
	}

	// Indexes for media
	if tableExists("media") {
		stmts := []string{
			"CREATE INDEX IF NOT EXISTS idx_media_path ON media(path)",
			"CREATE INDEX IF NOT EXISTS idx_media_has_description ON media(description) WHERE description IS NOT NULL AND description <> ''",
			"CREATE INDEX IF NOT EXISTS idx_media_has_hash ON media(hash) WHERE hash IS NOT NULL AND hash <> ''",
			"CREATE INDEX IF NOT EXISTS idx_media_has_size ON media(size) WHERE size IS NOT NULL",
		}
		for _, s := range stmts {
			if _, err := db.Exec(s); err != nil {
				return fmt.Errorf("creating index failed: %w", err)
			}
		}
	}

	return nil
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

		// GET – render quick jobs launcher
		if err := renderer.Templates().ExecuteTemplate(w, "home", nil); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

func jobsHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Use GET", http.StatusMethodNotAllowed)
			return
		}
		data := ListTemplateData{Jobs: deps.Queue.GetJobs()}
		if err := renderer.Templates().ExecuteTemplate(w, "jobs", data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

func jobsListHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Use GET", http.StatusMethodNotAllowed)
			return
		}
		jobs := deps.Queue.GetJobs()
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(jobs); err != nil {
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
			MediaItems:         items,
			Offset:             len(items),
			HasMore:            hasMore,
			SearchQuery:        searchQuery,
			DefaultOllamaModel: currentConfig.OllamaModel,
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
		singleStr := r.URL.Query().Get("single")

		// For the path parameter, use robust decoding to handle unicode characters
		var pathQuery string
		if rawPath := getRawQueryParam(r.URL.RawQuery, "path"); rawPath != "" {
			decoded, err := url.PathUnescape(rawPath)
			if err != nil {
				log.Printf("Error decoding path parameter: %v", err)
				http.Error(w, "Invalid path encoding", http.StatusBadRequest)
				return
			}
			pathQuery = decoded
		}

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

// mediaSuggestHandler serves suggestion data for typeahead search
func mediaSuggestHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Use GET", http.StatusMethodNotAllowed)
			return
		}

		kind := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("kind")))
		prefix := r.URL.Query().Get("prefix")
		limitStr := r.URL.Query().Get("limit")
		limit := 25
		if limitStr != "" {
			if parsed, err := strconv.Atoi(limitStr); err == nil && parsed > 0 && parsed <= 200 {
				limit = parsed
			}
		}

		type resp struct {
			Suggestions []string `json:"suggestions"`
		}

		var suggestions []string
		var err error

		switch kind {
		case "filters":
			suggestions = media.SuggestFilters()
		case "tag":
			suggestions, err = media.SuggestTagLabels(deps.DB, prefix, limit)
		case "category":
			suggestions, err = media.SuggestCategoryLabels(deps.DB, prefix, limit)
		case "path":
			suggestions, err = media.SuggestPaths(deps.DB, prefix, limit)
		case "pathdir":
			suggestions, err = media.SuggestPathDirs(deps.DB, prefix, limit)
		default:
			http.Error(w, "unknown kind", http.StatusBadRequest)
			return
		}

		if err != nil {
			log.Printf("suggest error kind=%s prefix=%q: %v", kind, prefix, err)
			http.Error(w, "suggest error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp{Suggestions: suggestions})
	}
}

// -----------------------------------------------------------------------------
// Tasks handler – lists all registered tasks/commands
// -----------------------------------------------------------------------------

// TaskInfo represents a task for the API response
type TaskInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func tasksHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Use GET", http.StatusMethodNotAllowed)
			return
		}

		taskMap := tasks.GetTasks()
		taskList := make([]TaskInfo, 0, len(taskMap))

		for _, t := range taskMap {
			taskList = append(taskList, TaskInfo{
				ID:   t.ID,
				Name: t.Name,
			})
		}

		// Sort by ID for consistent ordering
		sort.Slice(taskList, func(i, j int) bool {
			return taskList[i].ID < taskList[j].ID
		})

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"tasks": taskList})
	}
}

// -----------------------------------------------------------------------------
// Ollama models handler – lists available models via `ollama ls`
// -----------------------------------------------------------------------------

func ollamaModelsHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Use GET", http.StatusMethodNotAllowed)
			return
		}

		// Run `ollama ls --quiet` to get just model names, one per line.
		// Fallback to `ollama ls` parsing if --quiet is unavailable.
		var models []string

		run := func(args ...string) ([]byte, error) {
			cmd := exec.Command("ollama", args...)
			// Best-effort timeout via context is not critical here; rely on default.
			return cmd.Output()
		}

		// Try quiet first
		out, err := run("ls", "--quiet")
		if err != nil || len(out) == 0 {
			// Fallback to regular `ollama ls` and parse first column
			out, err = run("ls")
		}
		if err != nil {
			log.Printf("ollama ls error: %v", err)
			http.Error(w, "failed to list ollama models", http.StatusInternalServerError)
			return
		}

		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			// When not quiet, lines are like: "llama3:latest  4.7 GB  2 weeks ago"
			// Take the first whitespace-separated token.
			if strings.Contains(line, " ") {
				line = strings.Fields(line)[0]
			}
			// Some outputs include tags like name:tag – keep as-is so user can choose full ref.
			models = append(models, line)
		}

		// Deduplicate while preserving order
		seen := map[string]struct{}{}
		unique := make([]string, 0, len(models))
		for _, m := range models {
			if _, ok := seen[m]; ok {
				continue
			}
			seen[m] = struct{}{}
			unique = append(unique, m)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"models": unique})
	}
}

// -----------------------------------------------------------------------------
// Stats page handler
// -----------------------------------------------------------------------------

type statsTemplateData struct {
	TotalMedia         int
	WithDescription    int
	WithHash           int
	WithSize           int
	WithTags           int
	WithoutDescription int
	WithoutHash        int
	WithoutSize        int
	WithoutTags        int
}

func statsHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Use GET", http.StatusMethodNotAllowed)
			return
		}

		db := deps.DB

		var total, withDesc, withHash, withSize, withTags int

		// Single round-trip to fetch all counts; avoids JOIN for with-tags
		err := db.QueryRow(`
            SELECT
                (SELECT COUNT(*) FROM media) AS total,
                (SELECT COUNT(*) FROM media WHERE description IS NOT NULL AND TRIM(description) <> '') AS with_desc,
                (SELECT COUNT(*) FROM media WHERE hash IS NOT NULL AND TRIM(hash) <> '') AS with_hash,
                (SELECT COUNT(*) FROM media WHERE size IS NOT NULL) AS with_size,
                (SELECT COUNT(DISTINCT media_path) FROM media_tag_by_category) AS with_tags
        `).Scan(&total, &withDesc, &withHash, &withSize, &withTags)
		if err != nil {
			log.Printf("stats counts error: %v", err)
		}

		data := statsTemplateData{
			TotalMedia:         total,
			WithDescription:    withDesc,
			WithHash:           withHash,
			WithSize:           withSize,
			WithTags:           withTags,
			WithoutDescription: total - withDesc,
			WithoutHash:        total - withHash,
			WithoutSize:        total - withSize,
			WithoutTags:        total - withTags,
		}

		if err := renderer.Templates().ExecuteTemplate(w, "stats", data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// -----------------------------------------------------------------------------
// Config page handlers
// -----------------------------------------------------------------------------

type configTemplateData struct {
	Config       appconfig.Config
	ConfigPath   string
	ActiveDBPath string
}

type updateConfigRequest struct {
	DBPath                 string  `json:"dbPath"`
	DownloadPath           string  `json:"downloadPath"`
	OllamaBaseURL          string  `json:"ollamaBaseUrl"`
	OllamaModel            string  `json:"ollamaModel"`
	DescribePrompt         string  `json:"describePrompt"`
	AutotagPrompt          string  `json:"autotagPrompt"`
	OnnxModelPath          string  `json:"onnxModelPath"`
	OnnxLabelsPath         string  `json:"onnxLabelsPath"`
	OnnxConfigPath         string  `json:"onnxConfigPath"`
	OnnxORTSharedLibPath   string  `json:"onnxOrtSharedLibPath"`
	OnnxGeneralThreshold   float64 `json:"onnxGeneralThreshold"`
	OnnxCharacterThreshold float64 `json:"onnxCharacterThreshold"`
	FasterWhisperPath      string  `json:"fasterWhisperPath"`
}

func configHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			cfg, cfgPath, err := appconfig.Load()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			currentConfig = cfg
			data := configTemplateData{
				Config:       cfg,
				ConfigPath:   cfgPath,
				ActiveDBPath: cfg.DBPath,
			}
			if err := renderer.Templates().ExecuteTemplate(w, "config", data); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
		case http.MethodPost:
			var req updateConfigRequest
			if err := readJSONBody(r, &req); err != nil {
				http.Error(w, "bad json", http.StatusBadRequest)
				return
			}
			req.DBPath = strings.TrimSpace(req.DBPath)
			if req.DBPath == "" {
				http.Error(w, "dbPath cannot be empty", http.StatusBadRequest)
				return
			}

			oldCfg := currentConfig
			oldDBPath := currentConfig.DBPath
			newCfg := currentConfig
			newCfg.DBPath = req.DBPath
			if strings.TrimSpace(req.DownloadPath) != "" {
				newCfg.DownloadPath = strings.TrimSpace(req.DownloadPath)
			}
			if strings.TrimSpace(req.OllamaBaseURL) != "" {
				newCfg.OllamaBaseURL = strings.TrimSpace(req.OllamaBaseURL)
			}
			if strings.TrimSpace(req.OllamaModel) != "" {
				newCfg.OllamaModel = strings.TrimSpace(req.OllamaModel)
			}
			if req.DescribePrompt != "" {
				newCfg.DescribePrompt = req.DescribePrompt
			}
			if req.AutotagPrompt != "" {
				newCfg.AutotagPrompt = req.AutotagPrompt
			}
			newCfg.OnnxTagger.ModelPath = strings.TrimSpace(req.OnnxModelPath)
			newCfg.OnnxTagger.LabelsPath = strings.TrimSpace(req.OnnxLabelsPath)
			newCfg.OnnxTagger.ConfigPath = strings.TrimSpace(req.OnnxConfigPath)
			newCfg.OnnxTagger.ORTSharedLibraryPath = strings.TrimSpace(req.OnnxORTSharedLibPath)
			if req.OnnxGeneralThreshold > 0 {
				newCfg.OnnxTagger.GeneralThreshold = req.OnnxGeneralThreshold
			}
			if req.OnnxCharacterThreshold > 0 {
				newCfg.OnnxTagger.CharacterThreshold = req.OnnxCharacterThreshold
			}
			if strings.TrimSpace(req.FasterWhisperPath) != "" {
				newCfg.FasterWhisperPath = strings.TrimSpace(req.FasterWhisperPath)
			}
			cfgPath, err := appconfig.Save(newCfg)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}

			dbChanged := req.DBPath != oldDBPath
			if dbChanged {
				if err := switchDatabase(req.DBPath); err != nil {
					http.Error(w, "failed to switch database: "+err.Error(), http.StatusInternalServerError)
					return
				}
			}
			currentConfig = newCfg

			// Determine if any config field actually changed
			changed := !reflect.DeepEqual(oldCfg, newCfg)

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":       "ok",
				"configPath":   cfgPath,
				"activeDBPath": currentConfig.DBPath,
				"changed":      changed,
				"dbChanged":    dbChanged,
			})
		default:
			http.Error(w, "Use GET or POST", http.StatusMethodNotAllowed)
		}
	}
}

// getRawQueryParam extracts a parameter value from a raw query string without decoding.
// This allows us to use url.PathUnescape instead of url.QueryUnescape, which:
// 1. Properly handles complex unicode characters
// 2. Does not treat '+' as space (important for file paths that may contain '+')
// 3. Handles all valid percent-encoded sequences
func getRawQueryParam(rawQuery, key string) string {
	keyPrefix := key + "="
	for _, param := range strings.Split(rawQuery, "&") {
		if strings.HasPrefix(param, keyPrefix) {
			return strings.TrimPrefix(param, keyPrefix)
		}
	}
	return ""
}

// mediaFileHandler serves individual media files for preview
func mediaFileHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Use GET", http.StatusMethodNotAllowed)
			return
		}

		// Get the file path from query parameter using robust decoding
		// We use getRawQueryParam + PathUnescape to properly handle:
		// 1. Complex unicode characters (properly decoded from percent-encoded UTF-8)
		// 2. Plus signs in paths (not treated as spaces, unlike QueryUnescape)
		// 3. All valid path characters
		rawPath := getRawQueryParam(r.URL.RawQuery, "path")
		if rawPath == "" {
			http.Error(w, "Missing path parameter", http.StatusBadRequest)
			return
		}

		filePath, err := url.PathUnescape(rawPath)
		if err != nil {
			log.Printf("Error decoding path parameter: %v (raw: %s)", err, rawPath)
			http.Error(w, "Invalid path encoding", http.StatusBadRequest)
			return
		}

		// Validate and sanitize the file path
		filePath = strings.TrimSpace(filePath)
		if filePath == "" {
			http.Error(w, "Empty file path", http.StatusBadRequest)
			return
		}

		// If local path, enforce absolute path to avoid traversal via relative inputs
		if !strings.HasPrefix(filePath, "http://") && !strings.HasPrefix(filePath, "https://") {
			if !filepath.IsAbs(filePath) {
				http.Error(w, "Path must be absolute", http.StatusBadRequest)
				return
			}
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
		// For localhost serving of large videos, we allow up to 2GB
		const maxFileSize = 2 * 1024 * 1024 * 1024 // 2GB limit
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
	runners.New(queue, 1)

	// ––– create dependencies struct –––
	deps = &Dependencies{
		Queue: queue,
		DB:    db,
	}

	// ––– routes –––
	mux := http.NewServeMux()
	mux.HandleFunc("/", renderer.ApplyMiddlewares(homeHandler(deps)))
	mux.HandleFunc("/jobs", renderer.ApplyMiddlewares(jobsHandler(deps)))
	mux.HandleFunc("/jobs/list", renderer.ApplyMiddlewares(jobsListHandler(deps)))
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
	mux.HandleFunc("/media/suggest", renderer.ApplyMiddlewares(mediaSuggestHandler(deps)))
	mux.HandleFunc("/config", renderer.ApplyMiddlewares(configHandler(deps)))
	mux.HandleFunc("/stats", renderer.ApplyMiddlewares(statsHandler(deps)))
	mux.HandleFunc("/ollama/models", renderer.ApplyMiddlewares(ollamaModelsHandler(deps)))
	mux.HandleFunc("/tasks", renderer.ApplyMiddlewares(tasksHandler(deps)))

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
