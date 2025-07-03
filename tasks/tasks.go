package tasks

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	"image/png"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "golang.org/x/image/webp"

	"github.com/stevecastle/shrike/embedexec"
	"github.com/stevecastle/shrike/jobqueue"
	"github.com/stevecastle/shrike/media"
)

type Task struct {
	ID   string                                                        `json:"id"`
	Name string                                                        `json:"name"`
	Fn   func(j *jobqueue.Job, q *jobqueue.Queue, r *sync.Mutex) error `json:"-"`
}

type TaskMap map[string]Task

var tasks TaskMap

func init() {
	tasks = make(TaskMap)
	// Register a task that waits 5 seconds then completes
	RegisterTask("wait", "Wait", waitFn)
	RegisterTask("gallery-dl", "gallery-dl", executeCommand)
	RegisterTask("dce", "dce", executeCommand)
	RegisterTask("yt-dlp", "yt-dlp", executeCommand)
	RegisterTask("ffmpeg", "ffmpeg", executeCommand)
	RegisterTask("remove", "Remove Media", removeFromDB)
	RegisterTask("cleanup", "CleanUp", cleanUpFn)
	RegisterTask("ingest", "Ingest Media Files", ingestTask)
	RegisterTask("metadata", "Generate Metadata", metadataTask)
	RegisterTask("move", "Move Media Files", moveTask)
}

func RegisterTask(id, name string, fn func(j *jobqueue.Job, q *jobqueue.Queue, mu *sync.Mutex) error) {
	tasks[id] = Task{
		ID:   id,
		Name: name,
		Fn:   fn,
	}
}

func GetTasks() TaskMap {

	return tasks
}

func normalizeArg(a string) string {
	// More conservative heuristic for path detection
	// Only treat as a path if it has clear path indicators

	// If it contains path separators, it's likely a path
	if strings.ContainsAny(a, `/\`) {
		return normalizePath(a)
	}

	// If it starts with common path indicators, treat as path
	if strings.HasPrefix(a, ".") || strings.HasPrefix(a, "~") {
		return normalizePath(a)
	}

	// Windows drive letter patterns (C:, D:\, etc.)
	if len(a) >= 2 && a[1] == ':' && ((a[0] >= 'A' && a[0] <= 'Z') || (a[0] >= 'a' && a[0] <= 'z')) {
		return normalizePath(a)
	}

	// If it has a file extension AND looks like a filename (not a flag or setting)
	// This catches cases like "video.mp4" but not "--quality=0.95" or "--format=mp4"
	if strings.Contains(a, ".") && !strings.Contains(a, "=") && !strings.HasPrefix(a, "-") {
		// Additional check: does it look like a filename with a reasonable extension?
		ext := filepath.Ext(a)
		if len(ext) >= 2 && len(ext) <= 5 { // reasonable extension length (.mp4, .jpeg, etc.)
			// Check if base name isn't empty and doesn't look like a domain/version
			base := strings.TrimSuffix(a, ext)
			if base != "" && !strings.Contains(base, ".") {
				return normalizePath(a)
			}
		}
	}

	// For everything else (flags, options, domains, version numbers, etc.), return as-is
	return a
}

func normalizePath(a string) string {
	clean := filepath.Clean(a)

	// Convert to absolute when possible (fail-safe if $PWD disappears).
	if abs, err := filepath.Abs(clean); err == nil {
		clean = abs
	}

	// Use platform-native separators so the invoked program sees the
	// right thing whether it's on Windows or POSIX.
	return filepath.FromSlash(clean)
}

func executeCommand(j *jobqueue.Job, q *jobqueue.Queue, mu *sync.Mutex) error {
	ctx := j.Ctx
	fmt.Println("Executing job:", j.ID, j.Command, j.Arguments, j.Input)

	// ------------------------------------------------------------------
	// 1. Build a *new* argument slice with normalised / escaped paths.
	// ------------------------------------------------------------------
	scriptArgs := make([]string, 0, len(j.Arguments)+1)
	for _, a := range j.Arguments {
		scriptArgs = append(scriptArgs, normalizeArg(a))
	}
	if j.Input != "" { // Input is just "the last arg"
		scriptArgs = append(scriptArgs, (j.Input))
	}

	// ------------------------------------------------------------------
	// 2. Extract + start the embedded executable.
	// ------------------------------------------------------------------
	cmd, cleanup, err := embedexec.GetExec(ctx, j.Command, scriptArgs...)
	if err != nil {
		// Push the error to job stdout and mark the job as errored.
		_ = q.PushJobStdout(j.ID, fmt.Sprintf("Error starting job: %s", err))
		_ = q.ErrorJob(j.ID)
		return fmt.Errorf("start %q: %w", j.Command, err)
	}
	if cleanup != nil {
		defer cleanup()
	}

	// Kill the child tree if the context is cancelled.
	go func() {
		<-ctx.Done()
		if cmd.Process != nil {
			if runtime.GOOS == "windows" {
				_ = exec.Command("taskkill", "/F", "/T", "/PID",
					fmt.Sprintf("%d", cmd.Process.Pid)).Run()
			} else {
				_ = cmd.Process.Kill()
			}
		}
	}()

	// ------------------------------------------------------------------
	// 3. Wire up I/O.
	// ------------------------------------------------------------------
	if j.StdIn != nil {
		cmd.Stdin = j.StdIn
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		q.PushJobStdout(j.ID, fmt.Sprintf("Error getting stdout pipe: %s", err))
		_ = q.ErrorJob(j.ID)
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		q.PushJobStdout(j.ID, fmt.Sprintf("Error getting stderr pipe: %s", err))
		_ = q.ErrorJob(j.ID)
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		q.PushJobStdout(j.ID, fmt.Sprintf("Error starting command: %s", err))
		_ = q.ErrorJob(j.ID)
		return fmt.Errorf("start: %w", err)
	}

	// ------------------------------------------------------------------
	// 4. Stream stdout & stderr back to the queue.
	// ------------------------------------------------------------------
	doneReading := make(chan struct{})
	totalReaders := 2
	doneCount := 0

	scanAndPush := func(pipe io.ReadCloser) {
		scanner := bufio.NewScanner(pipe)
		for scanner.Scan() {
			_ = q.PushJobStdout(j.ID, scanner.Text())
		}
		if err := scanner.Err(); err != nil && err != io.EOF {
			// If there was an error reading the pipe, push it to stdout
			_ = q.PushJobStdout(j.ID, fmt.Sprintf("Error reading pipe: %s", err))
			_ = q.ErrorJob(j.ID)
			fmt.Println(err)
		}
		mu.Lock()
		doneCount++
		if doneCount == totalReaders {
			close(doneReading)
		}
		mu.Unlock()
	}

	go scanAndPush(stdoutPipe)
	go scanAndPush(stderrPipe)

	// ------------------------------------------------------------------
	// 5. Wait for completion & tidy up.
	// ------------------------------------------------------------------
	err = cmd.Wait()
	<-doneReading // ensure all output consumed

	select {
	case <-ctx.Done():
		// If the context is done, we assume the job was cancelled.
		q.PushJobStdout(j.ID, "Task was canceled")
		_ = q.ErrorJob(j.ID)
		return ctx.Err()
	default:
	}

	if err != nil {
		// If there was an error waiting for the command, push it to stdout
		q.PushJobStdout(j.ID, fmt.Sprintf("Error waiting for command: %s", err))
		_ = q.ErrorJob(j.ID)
		return err
	}

	fmt.Println("Job completed:", j.ID)
	_ = q.CompleteJob(j.ID)
	return nil
}

func waitFn(j *jobqueue.Job, q *jobqueue.Queue, mu *sync.Mutex) error {
	// Get the context from the job
	ctx := j.Ctx
	for i := 0; i < 5; i++ {
		select {
		case <-ctx.Done(): // Listen for context cancellation
			q.PushJobStdout(j.ID, "Task was canceled")
			return ctx.Err() // Return the context's error (e.g., context.Canceled)
		case <-time.After(1 * time.Second): // Wait for 1 second
			q.PushJobStdout(j.ID, "Waiting in task...")
		}
	}
	// Complete the job
	q.CompleteJob(j.ID)
	return nil
}

func removeFromDB(j *jobqueue.Job, q *jobqueue.Queue, mu *sync.Mutex) error {
	ctx := j.Ctx

	// Parse comma-separated paths from input
	pathsStr := strings.TrimSpace(j.Input)
	if pathsStr == "" {
		q.PushJobStdout(j.ID, "No paths provided for removal")
		q.CompleteJob(j.ID)
		return nil
	}

	paths := strings.Split(pathsStr, ",")

	q.PushJobStdout(j.ID, fmt.Sprintf("Starting removal of media items from database"))

	// Use the abstracted media removal function
	result, err := media.RemoveItemsFromDB(ctx, q.Db, paths)
	if err != nil {
		q.PushJobStdout(j.ID, fmt.Sprintf("Error removing media items: %v", err))
		q.ErrorJob(j.ID)
		return err
	}

	// Report the results
	q.PushJobStdout(j.ID, fmt.Sprintf("Processed %d paths", len(result.ProcessedPaths)))
	q.PushJobStdout(j.ID, fmt.Sprintf("Removed %d tag associations", result.TagsRemoved))
	q.PushJobStdout(j.ID, fmt.Sprintf("Removed %d media items from database", result.MediaItemsRemoved))

	// Log summary of removal operation
	if result.MediaItemsRemoved == 0 {
		q.PushJobStdout(j.ID, "No matching media items found in database")
	} else {
		q.PushJobStdout(j.ID, fmt.Sprintf("Successfully removed %d media items and %d tag associations", result.MediaItemsRemoved, result.TagsRemoved))
	}

	// Check if context was cancelled during operation
	select {
	case <-ctx.Done():
		q.PushJobStdout(j.ID, "Task was canceled")
		q.ErrorJob(j.ID)
		return ctx.Err()
	default:
	}

	q.CompleteJob(j.ID)
	return nil
}

func cleanUpFn(j *jobqueue.Job, q *jobqueue.Queue, mu *sync.Mutex) error {
	ctx := j.Ctx

	q.PushJobStdout(j.ID, "Starting database cleanup - finding and removing media items that don't exist in file system")

	// Create a progress callback to provide updates
	progressCallback := func(found, removed int) {
		q.PushJobStdout(j.ID, fmt.Sprintf("Progress: Found %d orphaned items, removed %d so far", found, removed))
	}

	// Use the streaming cleanup function to process items in batches
	result, err := media.StreamingCleanupNonExistentItems(ctx, q.Db, progressCallback)
	if err != nil {
		q.PushJobStdout(j.ID, fmt.Sprintf("Error during cleanup: %v", err))
		q.ErrorJob(j.ID)
		return err
	}

	// Report the final results
	if result.MediaItemsRemoved == 0 {
		q.PushJobStdout(j.ID, "No orphaned media items found - database is clean!")
	} else {
		q.PushJobStdout(j.ID, fmt.Sprintf("Cleanup completed successfully:"))
		q.PushJobStdout(j.ID, fmt.Sprintf("- Processed %d orphaned media items", len(result.ProcessedPaths)))
		q.PushJobStdout(j.ID, fmt.Sprintf("- Removed %d media items from database", result.MediaItemsRemoved))
		q.PushJobStdout(j.ID, fmt.Sprintf("- Removed %d tag associations", result.TagsRemoved))
	}

	// Check for any accumulated errors
	if len(result.Errors) > 0 {
		q.PushJobStdout(j.ID, fmt.Sprintf("Note: %d errors occurred during cleanup (but cleanup continued)", len(result.Errors)))
	}

	// Final context check
	select {
	case <-ctx.Done():
		q.PushJobStdout(j.ID, "Task was canceled")
		q.ErrorJob(j.ID)
		return ctx.Err()
	default:
	}

	q.CompleteJob(j.ID)
	return nil
}

// ingestTask scans directories for media files and adds them to the database
func ingestTask(j *jobqueue.Job, q *jobqueue.Queue, mu *sync.Mutex) error {
	ctx := j.Ctx

	// Parse arguments
	var dirPath string
	var recursive bool

	// Default directory path is current directory
	if j.Input != "" {
		dirPath = strings.TrimSpace(j.Input)
	} else {
		dirPath = "."
	}

	// Check arguments for flags
	for _, arg := range j.Arguments {
		switch strings.ToLower(arg) {
		case "-r", "--recursive":
			recursive = true
		}
		// Check for directory specification in arguments
		if !strings.HasPrefix(arg, "-") && arg != "" {
			dirPath = arg
		}
	}

	// Ensure database schema is set up
	if err := ensureMediaTableSchema(q.Db); err != nil {
		q.PushJobStdout(j.ID, fmt.Sprintf("Error setting up database schema: %v", err))
		q.ErrorJob(j.ID)
		return err
	}

	q.PushJobStdout(j.ID, fmt.Sprintf("Starting media file ingestion from: %s", dirPath))
	if recursive {
		q.PushJobStdout(j.ID, "Scanning recursively...")
	}

	// Scan for media files
	mediaFiles, err := scanMediaFiles(dirPath, recursive)
	if err != nil {
		q.PushJobStdout(j.ID, fmt.Sprintf("Error scanning directory: %v", err))
		q.ErrorJob(j.ID)
		return err
	}

	q.PushJobStdout(j.ID, fmt.Sprintf("Found %d media files", len(mediaFiles)))

	if len(mediaFiles) == 0 {
		q.PushJobStdout(j.ID, "No media files found to ingest")
		q.CompleteJob(j.ID)
		return nil
	}

	// Load existing paths from database
	existingPaths, err := getExistingMediaPaths(q.Db, dirPath)
	if err != nil {
		q.PushJobStdout(j.ID, fmt.Sprintf("Error loading existing database entries: %v", err))
		q.ErrorJob(j.ID)
		return err
	}

	// Find new files to ingest
	var newFiles []string
	for _, file := range mediaFiles {
		if _, exists := existingPaths[file]; !exists {
			newFiles = append(newFiles, file)
		}
	}

	q.PushJobStdout(j.ID, fmt.Sprintf("Found %d new files to ingest", len(newFiles)))

	if len(newFiles) == 0 {
		q.PushJobStdout(j.ID, "All files already exist in database")
		q.CompleteJob(j.ID)
		return nil
	}

	// Insert new files into database
	insertedCount := 0
	for i, filePath := range newFiles {
		// Check if context was cancelled
		select {
		case <-ctx.Done():
			q.PushJobStdout(j.ID, "Task was canceled")
			q.ErrorJob(j.ID)
			return ctx.Err()
		default:
		}

		// Get file size
		var size int64
		if fi, err := os.Stat(filePath); err == nil {
			size = fi.Size()
		}

		// Insert basic record into database
		err := insertMediaRecord(q.Db, filePath, size)
		if err != nil {
			q.PushJobStdout(j.ID, fmt.Sprintf("Warning: failed to insert %s: %v", filePath, err))
			continue
		}

		insertedCount++
		if (i+1)%100 == 0 || i == len(newFiles)-1 {
			q.PushJobStdout(j.ID, fmt.Sprintf("Progress: %d/%d files ingested", i+1, len(newFiles)))
		}
	}

	q.PushJobStdout(j.ID, fmt.Sprintf("Ingestion completed: %d files added to database", insertedCount))

	// Final context check
	select {
	case <-ctx.Done():
		q.PushJobStdout(j.ID, "Task was canceled")
		q.ErrorJob(j.ID)
		return ctx.Err()
	default:
	}

	q.CompleteJob(j.ID)
	return nil
}

// metadataTask generates various metadata for media files
func metadataTask(j *jobqueue.Job, q *jobqueue.Queue, mu *sync.Mutex) error {
	ctx := j.Ctx

	// Parse arguments for metadata types and options
	var metadataTypes []string
	var overwrite bool
	var applyScope string = "new" // default to new files only
	var ollamaModel string = "llama3.2-vision"

	// Parse arguments
	for i, arg := range j.Arguments {
		switch strings.ToLower(arg) {
		case "--type", "-t":
			if i+1 < len(j.Arguments) {
				metadataTypes = strings.Split(j.Arguments[i+1], ",")
				for idx, t := range metadataTypes {
					metadataTypes[idx] = strings.TrimSpace(t)
				}
			}
		case "--overwrite", "-o":
			overwrite = true
		case "--apply", "-a":
			if i+1 < len(j.Arguments) {
				applyScope = strings.ToLower(strings.TrimSpace(j.Arguments[i+1]))
			}
		case "--model", "-m":
			if i+1 < len(j.Arguments) {
				ollamaModel = strings.TrimSpace(j.Arguments[i+1])
			}
		}
	}

	// Default metadata types if none specified
	if len(metadataTypes) == 0 {
		metadataTypes = []string{"description", "hash", "dimensions"}
	}

	// Validate metadata types
	validTypes := map[string]bool{
		"description": true,
		"transcript":  true,
		"hash":        true,
		"dimensions":  true,
	}

	for _, mType := range metadataTypes {
		if !validTypes[strings.ToLower(mType)] {
			q.PushJobStdout(j.ID, fmt.Sprintf("Warning: unknown metadata type '%s' - valid types are: description, transcript, hash, dimensions", mType))
		}
	}

	q.PushJobStdout(j.ID, fmt.Sprintf("Starting metadata generation for types: %s", strings.Join(metadataTypes, ", ")))
	q.PushJobStdout(j.ID, fmt.Sprintf("Apply scope: %s", applyScope))
	q.PushJobStdout(j.ID, fmt.Sprintf("Overwrite existing: %t", overwrite))

	// Parse input as list of file paths
	var filesToProcess []string
	var err error

	if strings.TrimSpace(j.Input) == "" {
		// If no input provided, process all files from database
		q.PushJobStdout(j.ID, "No file list provided - processing all files from database")
		filesToProcess, err = getAllMediaPaths(q.Db)
		if err != nil {
			q.PushJobStdout(j.ID, fmt.Sprintf("Error loading media paths from database: %v", err))
			q.ErrorJob(j.ID)
			return err
		}
	} else {
		// Parse input as file paths (support both comma and newline separation)
		input := strings.TrimSpace(j.Input)

		// Try newline separation first, then comma separation
		var rawPaths []string
		if strings.Contains(input, "\n") {
			rawPaths = strings.Split(input, "\n")
		} else {
			rawPaths = strings.Split(input, ",")
		}

		// Clean and validate file paths
		for _, rawPath := range rawPaths {
			cleanPath := strings.TrimSpace(rawPath)
			if cleanPath == "" {
				continue
			}

			// Convert to absolute path
			absPath, err := filepath.Abs(cleanPath)
			if err == nil {
				cleanPath = filepath.FromSlash(absPath)
			}

			// Check if file exists
			if _, err := os.Stat(cleanPath); os.IsNotExist(err) {
				q.PushJobStdout(j.ID, fmt.Sprintf("Warning: file does not exist: %s", cleanPath))
				continue
			}

			// Check if it's a media file
			if !isMediaFile(cleanPath) {
				q.PushJobStdout(j.ID, fmt.Sprintf("Warning: not a supported media file: %s", cleanPath))
				continue
			}

			filesToProcess = append(filesToProcess, cleanPath)
		}

		q.PushJobStdout(j.ID, fmt.Sprintf("Processing files from input list"))
	}

	if len(filesToProcess) == 0 {
		q.PushJobStdout(j.ID, "No valid files found to process")
		q.CompleteJob(j.ID)
		return nil
	}

	// Filter files based on apply scope
	if applyScope == "new" {
		// Only process files that don't exist in database yet
		var newFiles []string
		for _, filePath := range filesToProcess {
			exists, err := fileExistsInDatabase(q.Db, filePath)
			if err != nil {
				q.PushJobStdout(j.ID, fmt.Sprintf("Warning: error checking database for %s: %v", filePath, err))
				continue
			}
			if !exists {
				newFiles = append(newFiles, filePath)
			}
		}
		filesToProcess = newFiles
		q.PushJobStdout(j.ID, fmt.Sprintf("After filtering for new files: %d files to process", len(filesToProcess)))
	} else {
		q.PushJobStdout(j.ID, fmt.Sprintf("Processing all specified files: %d files", len(filesToProcess)))
	}

	if len(filesToProcess) == 0 {
		q.PushJobStdout(j.ID, "No files to process after filtering")
		q.CompleteJob(j.ID)
		return nil
	}

	// Process each metadata type
	for _, metadataType := range metadataTypes {
		mType := strings.ToLower(metadataType)

		q.PushJobStdout(j.ID, fmt.Sprintf("Generating %s metadata...", mType))

		switch mType {
		case "description":
			err = generateDescriptions(ctx, q, j.ID, filesToProcess, overwrite, ollamaModel)
		case "transcript":
			err = generateTranscripts(ctx, q, j.ID, filesToProcess, overwrite)
		case "hash":
			err = generateHashes(ctx, q, j.ID, filesToProcess, overwrite)
		case "dimensions":
			err = generateDimensions(ctx, q, j.ID, filesToProcess, overwrite)
		default:
			q.PushJobStdout(j.ID, fmt.Sprintf("Skipping unknown metadata type: %s", mType))
			continue
		}

		if err != nil {
			q.PushJobStdout(j.ID, fmt.Sprintf("Error generating %s: %v", mType, err))
			q.ErrorJob(j.ID)
			return err
		}

		// Check if context was cancelled between metadata types
		select {
		case <-ctx.Done():
			q.PushJobStdout(j.ID, "Task was canceled")
			q.ErrorJob(j.ID)
			return ctx.Err()
		default:
		}
	}

	q.PushJobStdout(j.ID, "Metadata generation completed successfully")
	q.CompleteJob(j.ID)
	return nil
}

// Helper functions for the new tasks

// ensureMediaTableSchema ensures the media table has all required columns
func ensureMediaTableSchema(db *sql.DB) error {
	// Create the main media table if it doesn't exist
	createTableSQL := `
	CREATE TABLE IF NOT EXISTS media (
		path TEXT PRIMARY KEY,
		description TEXT,
		transcript TEXT,
		hash TEXT,
		size INTEGER,
		width INTEGER,
		height INTEGER
	);`

	if _, err := db.Exec(createTableSQL); err != nil {
		return fmt.Errorf("failed to create media table: %w", err)
	}

	// Add width and height columns if they don't exist (for backward compatibility)
	_, _ = db.Exec(`ALTER TABLE media ADD COLUMN width INTEGER;`)
	_, _ = db.Exec(`ALTER TABLE media ADD COLUMN height INTEGER;`)

	return nil
}

// scanMediaFiles scans the directory (recursively if specified) for media files
func scanMediaFiles(dir string, recursive bool) ([]string, error) {
	var files []string

	isMedia := func(path string) bool {
		ext := strings.ToLower(filepath.Ext(path))
		switch ext {
		case ".jpg", ".jpeg", ".png", ".gif", ".bmp", ".webp", ".heic", ".tif", ".tiff",
			".mp4", ".mov", ".avi", ".mkv", ".webm", ".wmv":
			return true
		}
		return false
	}

	walkFn := func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() && !recursive && path != dir {
			// If not recursive, skip subdirectories
			return filepath.SkipDir
		}
		if !info.IsDir() && isMedia(path) {
			// Get absolute path
			absPath, err := filepath.Abs(path)
			if err == nil {
				files = append(files, filepath.FromSlash(absPath))
			} else {
				files = append(files, path)
			}
		}
		return nil
	}

	err := filepath.Walk(dir, walkFn)
	if err != nil {
		return nil, err
	}

	return files, nil
}

// getExistingMediaPaths loads existing media paths from the database
func getExistingMediaPaths(db *sql.DB, dirPath string) (map[string]struct{}, error) {
	query := `SELECT path FROM media`
	var args []interface{}

	// If dirPath is specified, filter by it
	if dirPath != "" && dirPath != "." {
		absDir, err := filepath.Abs(dirPath)
		if err == nil {
			dirPath = filepath.FromSlash(absDir)
		}
		query += ` WHERE path LIKE ?`
		args = append(args, dirPath+"%")
	}

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]struct{})
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		result[path] = struct{}{}
	}
	return result, nil
}

// getAllMediaPaths gets all media paths from the database
func getAllMediaPaths(db *sql.DB) ([]string, error) {
	query := `SELECT path FROM media ORDER BY path`
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, nil
}

// insertMediaRecord inserts a basic media record into the database
func insertMediaRecord(db *sql.DB, path string, size int64) error {
	stmt := `INSERT OR IGNORE INTO media (path, size) VALUES (?, ?)`
	_, err := db.Exec(stmt, path, size)
	return err
}

// generateDescriptions generates descriptions for media files using Ollama
func generateDescriptions(ctx context.Context, q *jobqueue.Queue, jobID string, filePaths []string, overwrite bool, model string) error {
	processed := 0
	for _, filePath := range filePaths {
		// Check if context was cancelled
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Check if file exists
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			q.PushJobStdout(jobID, fmt.Sprintf("Warning: file does not exist: %s", filePath))
			continue
		}

		// Check if description already exists and we're not overwriting
		if !overwrite {
			hasDescription, err := hasExistingMetadata(q.Db, filePath, "description")
			if err != nil {
				log.Printf("Error checking existing description for %s: %v", filePath, err)
				continue
			}
			if hasDescription {
				continue
			}
		}

		// Generate description using Ollama
		description, err := describeFileWithOllama(filePath, model)
		if err != nil {
			q.PushJobStdout(jobID, fmt.Sprintf("Warning: failed to describe %s: %v", filePath, err))
			continue
		}

		// Update database
		err = updateMediaMetadata(q.Db, filePath, "description", description)
		if err != nil {
			q.PushJobStdout(jobID, fmt.Sprintf("Warning: failed to update description for %s: %v", filePath, err))
			continue
		}

		processed++
		if processed%10 == 0 || processed == len(filePaths) {
			q.PushJobStdout(jobID, fmt.Sprintf("Description progress: %d/%d files processed", processed, len(filePaths)))
		}
	}

	q.PushJobStdout(jobID, fmt.Sprintf("Generated descriptions for %d files", processed))
	return nil
}

// generateTranscripts generates transcripts for video files using faster-whisper
func generateTranscripts(ctx context.Context, q *jobqueue.Queue, jobID string, filePaths []string, overwrite bool) error {
	processed := 0
	for _, filePath := range filePaths {
		// Check if context was cancelled
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Check if it's a video file
		ext := strings.ToLower(filepath.Ext(filePath))
		isVideo := false
		switch ext {
		case ".mp4", ".mov", ".avi", ".mkv", ".webm", ".wmv":
			isVideo = true
		}

		if !isVideo {
			continue
		}

		// Check if file exists
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			q.PushJobStdout(jobID, fmt.Sprintf("Warning: file does not exist: %s", filePath))
			continue
		}

		// Check if transcript already exists and we're not overwriting
		if !overwrite {
			hasTranscript, err := hasExistingMetadata(q.Db, filePath, "transcript")
			if err != nil {
				log.Printf("Error checking existing transcript for %s: %v", filePath, err)
				continue
			}
			if hasTranscript {
				continue
			}
		}

		// Generate transcript using faster-whisper-xxl
		transcript, err := generateTranscriptWithFasterWhisper(filePath)
		if err != nil {
			q.PushJobStdout(jobID, fmt.Sprintf("Warning: failed to transcribe %s: %v", filePath, err))
			continue
		}

		// Update database
		err = updateMediaMetadata(q.Db, filePath, "transcript", transcript)
		if err != nil {
			q.PushJobStdout(jobID, fmt.Sprintf("Warning: failed to update transcript for %s: %v", filePath, err))
			continue
		}

		processed++
		q.PushJobStdout(jobID, fmt.Sprintf("Transcript progress: %d video files processed", processed))
	}

	q.PushJobStdout(jobID, fmt.Sprintf("Generated transcripts for %d video files", processed))
	return nil
}

// generateHashes generates hashes for media files
func generateHashes(ctx context.Context, q *jobqueue.Queue, jobID string, filePaths []string, overwrite bool) error {
	const maxBytes = 3 * 1024 * 1024 // 3MB

	processed := 0
	for _, filePath := range filePaths {
		// Check if context was cancelled
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Check if file exists
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			q.PushJobStdout(jobID, fmt.Sprintf("Warning: file does not exist: %s", filePath))
			continue
		}

		// Check if hash already exists and we're not overwriting
		if !overwrite {
			hasHash, err := hasExistingMetadata(q.Db, filePath, "hash")
			if err != nil {
				log.Printf("Error checking existing hash for %s: %v", filePath, err)
				continue
			}
			if hasHash {
				continue
			}
		}

		// Get file size and generate hash
		fi, err := os.Stat(filePath)
		if err != nil {
			q.PushJobStdout(jobID, fmt.Sprintf("Warning: failed to stat %s: %v", filePath, err))
			continue
		}

		file, err := os.Open(filePath)
		if err != nil {
			q.PushJobStdout(jobID, fmt.Sprintf("Warning: failed to open %s: %v", filePath, err))
			continue
		}

		hashVal, err := hashFirstNBytes(file, maxBytes)
		file.Close()
		if err != nil {
			q.PushJobStdout(jobID, fmt.Sprintf("Warning: failed to hash %s: %v", filePath, err))
			continue
		}

		// Update database with hash and size
		stmt := `UPDATE media SET hash = ?, size = ? WHERE path = ?`
		_, err = q.Db.Exec(stmt, hashVal, fi.Size(), filePath)
		if err != nil {
			q.PushJobStdout(jobID, fmt.Sprintf("Warning: failed to update hash for %s: %v", filePath, err))
			continue
		}

		processed++
		if processed%50 == 0 || processed == len(filePaths) {
			q.PushJobStdout(jobID, fmt.Sprintf("Hash progress: %d/%d files processed", processed, len(filePaths)))
		}
	}

	q.PushJobStdout(jobID, fmt.Sprintf("Generated hashes for %d files", processed))
	return nil
}

// generateDimensions generates width/height dimensions for media files
func generateDimensions(ctx context.Context, q *jobqueue.Queue, jobID string, filePaths []string, overwrite bool) error {
	processed := 0
	for _, filePath := range filePaths {
		// Check if context was cancelled
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Check if file exists
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			q.PushJobStdout(jobID, fmt.Sprintf("Warning: file does not exist: %s", filePath))
			continue
		}

		// Check if dimensions already exist and we're not overwriting
		if !overwrite {
			hasDimensions, err := hasExistingDimensions(q.Db, filePath)
			if err != nil {
				log.Printf("Error checking existing dimensions for %s: %v", filePath, err)
				continue
			}
			if hasDimensions {
				continue
			}
		}

		// Get dimensions based on file type
		ext := strings.ToLower(filepath.Ext(filePath))
		var width, height int
		var err error

		switch ext {
		case ".jpg", ".jpeg", ".png", ".bmp", ".webp", ".gif", ".tif", ".tiff", ".heic":
			width, height, err = getImageDimensions(filePath)
		case ".mp4", ".mov", ".avi", ".mkv", ".webm":
			width, height, err = getVideoDimensionsFFProbe(filePath)
		default:
			continue // Skip unsupported file types
		}

		if err != nil {
			q.PushJobStdout(jobID, fmt.Sprintf("Warning: failed to get dimensions for %s: %v", filePath, err))
			continue
		}

		// Update database
		stmt := `UPDATE media SET width = ?, height = ? WHERE path = ?`
		_, err = q.Db.Exec(stmt, width, height, filePath)
		if err != nil {
			q.PushJobStdout(jobID, fmt.Sprintf("Warning: failed to update dimensions for %s: %v", filePath, err))
			continue
		}

		processed++
		if processed%50 == 0 || processed == len(filePaths) {
			q.PushJobStdout(jobID, fmt.Sprintf("Dimensions progress: %d/%d files processed", processed, len(filePaths)))
		}
	}

	q.PushJobStdout(jobID, fmt.Sprintf("Generated dimensions for %d files", processed))
	return nil
}

// hasExistingMetadata checks if a file already has metadata of the specified type
func hasExistingMetadata(db *sql.DB, path, metadataType string) (bool, error) {
	query := fmt.Sprintf(`SELECT %s FROM media WHERE path = ?`, metadataType)
	var value sql.NullString
	err := db.QueryRow(query, path).Scan(&value)
	if err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	return value.Valid && value.String != "", nil
}

// hasExistingDimensions checks if a file already has width/height dimensions
func hasExistingDimensions(db *sql.DB, path string) (bool, error) {
	query := `SELECT width, height FROM media WHERE path = ?`
	var width, height sql.NullInt64
	err := db.QueryRow(query, path).Scan(&width, &height)
	if err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	return width.Valid && height.Valid, nil
}

// updateMediaMetadata updates a specific metadata field for a media file
func updateMediaMetadata(db *sql.DB, path, metadataType, value string) error {
	query := fmt.Sprintf(`UPDATE media SET %s = ? WHERE path = ?`, metadataType)
	_, err := db.Exec(query, value, path)
	return err
}

// describeFileWithOllama generates description using Ollama vision model
func describeFileWithOllama(mediaPath, model string) (string, error) {
	ext := strings.ToLower(filepath.Ext(mediaPath))
	var tempImagePath string
	var cleanupPaths []string

	// Handle different file types
	if ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".bmp" || ext == ".webp" {
		// It's an image
		tempImagePath = mediaPath
	} else {
		// Assume it's a video, take a screenshot using ffmpeg
		screenshotPath := filepath.Join(os.TempDir(), "ollama_screenshot_"+filepath.Base(mediaPath)+".jpg")
		cleanupPaths = append(cleanupPaths, screenshotPath)

		ffmpegCmd := exec.Command("ffmpeg",
			"-ss", "1",
			"-i", mediaPath,
			"-frames:v", "1",
			"-q:v", "2",
			"-y", // Overwrite output file
			screenshotPath,
		)
		if err := ffmpegCmd.Run(); err != nil {
			return "", fmt.Errorf("ffmpeg screenshot failed: %w", err)
		}
		tempImagePath = screenshotPath
	}

	// Resize image if needed (max 1024px)
	resizedPath, err := resizeImageIfNeeded(tempImagePath)
	if err != nil {
		// Clean up partials if needed
		for _, p := range cleanupPaths {
			_ = os.Remove(p)
		}
		return "", fmt.Errorf("failed to resize image: %w", err)
	}
	if resizedPath != tempImagePath {
		cleanupPaths = append(cleanupPaths, resizedPath)
	}

	// Call Ollama with the image
	description, err := callOllamaVision(resizedPath, model)
	if err != nil {
		// Clean up partials if needed
		for _, p := range cleanupPaths {
			_ = os.Remove(p)
		}
		return "", fmt.Errorf("ollama call failed: %w", err)
	}

	// Cleanup
	for _, p := range cleanupPaths {
		_ = os.Remove(p)
	}
	return description, nil
}

// resizeImageIfNeeded resizes an image if it's larger than 1024px in any dimension
func resizeImageIfNeeded(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	// Decode the image
	img, _, err := image.Decode(f)
	if err != nil {
		return "", fmt.Errorf("image decode failed: %w", err)
	}

	// Check original dimensions
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()

	// If image is small enough, return original path
	if width <= 1024 && height <= 1024 {
		return path, nil
	}

	// For simplicity, just encode the original as PNG
	// In a real implementation, you'd want proper resizing
	tmpName := fmt.Sprintf("ollama_resized_%s.png", filepath.Base(path))
	convertedPath := filepath.Join(os.TempDir(), tmpName)

	out, err := os.Create(convertedPath)
	if err != nil {
		return "", err
	}
	defer out.Close()

	// For simplicity, just encode the original as PNG
	// In a real implementation, you'd want proper resizing
	if err := png.Encode(out, img); err != nil {
		return "", err
	}

	return convertedPath, nil
}

// callOllamaVision calls Ollama API to describe an image
func callOllamaVision(imagePath, model string) (string, error) {
	// Read image and convert to base64
	data, err := os.ReadFile(imagePath)
	if err != nil {
		return "", fmt.Errorf("could not read image for Ollama: %w", err)
	}
	b64 := base64.StdEncoding.EncodeToString(data)

	// Build JSON payload for Ollama API
	requestJSON := fmt.Sprintf(`{
		"model": "%s",
		"stream": false,
		"prompt": "Please describe this image, paying special attention to the people, the color of hair, clothing, items, text and captions, and actions being performed.",
		"images": ["%s"]
	}`, model, b64)

	// Create request
	req, err := http.NewRequest("POST", "http://localhost:11434/api/generate", strings.NewReader(requestJSON))
	if err != nil {
		return "", fmt.Errorf("failed to build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Send request
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("ollama request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("ollama error: status=%d, body=%s", resp.StatusCode, string(bodyBytes))
	}

	// Read response
	respData, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response body failed: %w", err)
	}

	// Parse JSON response
	var response struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal(respData, &response); err != nil {
		return "", fmt.Errorf("could not unmarshal Ollama response: %w", err)
	}

	return response.Response, nil
}

// generateTranscriptWithFasterWhisper generates transcript using faster-whisper-xxl
func generateTranscriptWithFasterWhisper(filePath string) (string, error) {
	// Run faster-whisper-xxl
	cmd := exec.Command(
		"faster-whisper-xxl.exe",
		"--beep_off",
		"--output_format=vtt",
		"--output_dir=source",
		filePath,
	)

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("faster-whisper-xxl failed: %w", err)
	}

	// Read the generated .vtt file
	filepathNoExt := filePath[:len(filePath)-len(filepath.Ext(filePath))]
	vttPath := filepathNoExt + ".vtt"

	vttData, err := readFileAll(vttPath)
	if err != nil {
		return "", fmt.Errorf("could not read VTT file %s: %w", vttPath, err)
	}

	return vttData, nil
}

// readFileAll reads an entire file as a string
func readFileAll(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var sb strings.Builder
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		sb.WriteString(scanner.Text())
		sb.WriteByte('\n')
	}
	if scanErr := scanner.Err(); scanErr != nil {
		return "", scanErr
	}
	return sb.String(), nil
}

// getImageDimensions gets width/height of an image file
func getImageDimensions(path string) (int, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0, err
	}
	return cfg.Width, cfg.Height, nil
}

// getVideoDimensionsFFProbe gets width/height of a video file using ffprobe
func getVideoDimensionsFFProbe(path string) (int, int, error) {
	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height",
		"-of", "csv=s=x:p=0",
		path)
	out, err := cmd.Output()
	if err != nil {
		return 0, 0, err
	}

	// Parse output like "1920x1080\n"
	dims := strings.Split(strings.TrimSpace(string(out)), "x")
	if len(dims) != 2 {
		return 0, 0, errors.New("unexpected ffprobe output: " + string(out))
	}

	width, wErr := strconv.Atoi(dims[0])
	height, hErr := strconv.Atoi(dims[1])
	if wErr != nil || hErr != nil {
		return 0, 0, fmt.Errorf("failed to parse width/height from: %s", string(out))
	}

	return width, height, nil
}

// hashFirstNBytes calculates SHA-256 hash of the first n bytes of a file
func hashFirstNBytes(r io.Reader, n int64) (string, error) {
	if n < 0 {
		return "", errors.New("invalid byte count")
	}

	hasher := sha256.New()
	limitReader := io.LimitReader(r, n)

	_, err := io.Copy(hasher, limitReader)
	if err != nil {
		return "", err
	}

	sum := hasher.Sum(nil)
	return hex.EncodeToString(sum), nil
}

// isMediaFile checks if a file is a supported media file based on its extension
func isMediaFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".gif", ".bmp", ".webp", ".heic", ".tif", ".tiff",
		".mp4", ".mov", ".avi", ".mkv", ".webm", ".wmv":
		return true
	}
	return false
}

// fileExistsInDatabase checks if a file path exists in the media database
func fileExistsInDatabase(db *sql.DB, path string) (bool, error) {
	query := `SELECT 1 FROM media WHERE path = ? LIMIT 1`
	var exists int
	err := db.QueryRow(query, path).Scan(&exists)
	if err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// moveTask moves media files to a new target directory while preserving structure and updating database references
func moveTask(j *jobqueue.Job, q *jobqueue.Queue, mu *sync.Mutex) error {
	ctx := j.Ctx

	// Parse target directory and optional prefix from arguments
	var targetDir string
	var specifiedPrefix string

	for i, arg := range j.Arguments {
		if arg == "--prefix" || arg == "-p" {
			// Next argument should be the prefix
			if i+1 < len(j.Arguments) {
				specifiedPrefix = strings.TrimSpace(j.Arguments[i+1])
			}
		} else if !strings.HasPrefix(arg, "-") && targetDir == "" {
			// First non-flag argument is the target directory
			targetDir = strings.TrimSpace(arg)
		}
	}

	if targetDir == "" {
		q.PushJobStdout(j.ID, "Error: No target directory specified in arguments")
		q.ErrorJob(j.ID)
		return fmt.Errorf("no target directory specified")
	}

	// Parse comma-separated paths from input
	pathsStr := strings.TrimSpace(j.Input)
	if pathsStr == "" {
		q.PushJobStdout(j.ID, "No paths provided for moving")
		q.CompleteJob(j.ID)
		return nil
	}

	paths := strings.Split(pathsStr, ",")

	q.PushJobStdout(j.ID, fmt.Sprintf("Starting move operation to target directory: %s", targetDir))

	// Clean and validate paths
	var validPaths []string
	for _, path := range paths {
		cleanPath := strings.TrimSpace(path)
		if cleanPath == "" {
			continue
		}

		// Convert to absolute path
		absPath, err := filepath.Abs(cleanPath)
		if err == nil {
			cleanPath = filepath.FromSlash(absPath)
		}

		// Check if file exists
		if _, err := os.Stat(cleanPath); os.IsNotExist(err) {
			q.PushJobStdout(j.ID, fmt.Sprintf("Warning: file does not exist: %s", cleanPath))
			continue
		}

		validPaths = append(validPaths, cleanPath)
	}

	if len(validPaths) == 0 {
		q.PushJobStdout(j.ID, "No valid files found to move")
		q.CompleteJob(j.ID)
		return nil
	}

	q.PushJobStdout(j.ID, fmt.Sprintf("Found %d valid files to move", len(validPaths)))

	// Create target directory if it doesn't exist
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		q.PushJobStdout(j.ID, fmt.Sprintf("Error creating target directory: %v", err))
		q.ErrorJob(j.ID)
		return err
	}

	// Determine prefix to use for preserving structure
	var prefixToUse string
	if specifiedPrefix != "" {
		// Use the specified prefix
		prefixToUse = filepath.Clean(specifiedPrefix)
		q.PushJobStdout(j.ID, fmt.Sprintf("Using specified prefix: %s", prefixToUse))
	} else {
		// Find common prefix of all source paths to preserve structure
		prefixToUse = findCommonPrefix(validPaths)
		q.PushJobStdout(j.ID, fmt.Sprintf("Using calculated common prefix: %s", prefixToUse))
	}

	// Process each file
	moveCount := 0
	updateCount := 0
	for i, srcPath := range validPaths {
		// Check if context was cancelled
		select {
		case <-ctx.Done():
			q.PushJobStdout(j.ID, "Task was canceled")
			q.ErrorJob(j.ID)
			return ctx.Err()
		default:
		}

		// Calculate destination path while preserving structure
		relativePath := strings.TrimPrefix(srcPath, prefixToUse)
		relativePath = strings.TrimPrefix(relativePath, string(filepath.Separator))
		destPath := filepath.Join(targetDir, relativePath)

		// Create destination directory if needed
		destDir := filepath.Dir(destPath)
		if err := os.MkdirAll(destDir, 0755); err != nil {
			q.PushJobStdout(j.ID, fmt.Sprintf("Warning: failed to create destination directory %s: %v", destDir, err))
			continue
		}

		// Move the file
		if err := os.Rename(srcPath, destPath); err != nil {
			q.PushJobStdout(j.ID, fmt.Sprintf("Warning: failed to move %s to %s: %v", srcPath, destPath, err))
			continue
		}

		moveCount++

		// Update database references
		err := updateMediaPathInDatabase(q.Db, srcPath, destPath)
		if err != nil {
			q.PushJobStdout(j.ID, fmt.Sprintf("Warning: failed to update database for %s: %v", srcPath, err))
			// File was moved successfully, but database update failed
			// This is not critical enough to fail the entire operation
		} else {
			updateCount++
		}

		if (i+1)%10 == 0 || i == len(validPaths)-1 {
			q.PushJobStdout(j.ID, fmt.Sprintf("Progress: %d/%d files processed", i+1, len(validPaths)))
		}
	}

	q.PushJobStdout(j.ID, fmt.Sprintf("Move operation completed: %d files moved, %d database entries updated", moveCount, updateCount))

	// Final context check
	select {
	case <-ctx.Done():
		q.PushJobStdout(j.ID, "Task was canceled")
		q.ErrorJob(j.ID)
		return ctx.Err()
	default:
	}

	q.CompleteJob(j.ID)
	return nil
}

// findCommonPrefix finds the common directory prefix among a list of file paths
func findCommonPrefix(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	if len(paths) == 1 {
		return filepath.Dir(paths[0])
	}

	// Start with the directory of the first path
	prefix := filepath.Dir(paths[0])

	for _, path := range paths[1:] {
		pathDir := filepath.Dir(path)

		// Find common prefix between current prefix and this path's directory
		newPrefix := ""
		prefixParts := strings.Split(prefix, string(filepath.Separator))
		pathParts := strings.Split(pathDir, string(filepath.Separator))

		minLen := len(prefixParts)
		if len(pathParts) < minLen {
			minLen = len(pathParts)
		}

		for i := 0; i < minLen; i++ {
			if prefixParts[i] == pathParts[i] {
				if newPrefix == "" {
					newPrefix = prefixParts[i]
				} else {
					newPrefix = filepath.Join(newPrefix, prefixParts[i])
				}
			} else {
				break
			}
		}

		prefix = newPrefix
		if prefix == "" {
			break
		}
	}

	return prefix
}

// updateMediaPathInDatabase updates the path references in both media and media_tag_by_category tables
func updateMediaPathInDatabase(db *sql.DB, oldPath, newPath string) error {
	// Start a transaction to ensure both updates succeed or fail together
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("failed to start transaction: %w", err)
	}
	defer tx.Rollback() // This will be a no-op if tx.Commit() succeeds

	// Update media table
	mediaQuery := `UPDATE media SET path = ? WHERE path = ?`
	_, err = tx.Exec(mediaQuery, newPath, oldPath)
	if err != nil {
		return fmt.Errorf("failed to update media table: %w", err)
	}

	// Update media_tag_by_category table
	tagQuery := `UPDATE media_tag_by_category SET media_path = ? WHERE media_path = ?`
	_, err = tx.Exec(tagQuery, newPath, oldPath)
	if err != nil {
		return fmt.Errorf("failed to update media_tag_by_category table: %w", err)
	}

	// Commit the transaction
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	return nil
}
