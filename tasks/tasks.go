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

	"github.com/stevecastle/shrike/appconfig"
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
	RegisterTask("ffmpeg", "ffmpeg", ffmpegTask)
	RegisterTask("remove", "Remove Media", removeFromDB)
	RegisterTask("cleanup", "CleanUp", cleanUpFn)
	RegisterTask("ingest", "Ingest Media Files", ingestTask)
	RegisterTask("metadata", "Generate Metadata", metadataTask)
	RegisterTask("move", "Move Media Files", moveTask)
	RegisterTask("autotag", "Auto Tag (ONNX)", autotagTask)
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
		_ = q.CancelJob(j.ID)
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
			_ = q.CancelJob(j.ID)
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

	// Determine file list via query or explicit input
	var paths []string
	if qstr, ok := extractQueryFromJob(j); ok {
		q.PushJobStdout(j.ID, fmt.Sprintf("Using query to select files: %s", qstr))
		mediaPaths, err := getMediaPathsByQuery(q.Db, qstr)
		if err != nil {
			q.PushJobStdout(j.ID, fmt.Sprintf("Error loading media paths for query: %v", err))
			q.ErrorJob(j.ID)
			return err
		}
		paths = mediaPaths
	} else {
		// Parse newline-separated paths from input
		pathsStr := strings.TrimSpace(j.Input)
		if pathsStr == "" {
			q.PushJobStdout(j.ID, "No paths provided for removal")
			q.CompleteJob(j.ID)
			return nil
		}

		rawPaths := strings.Split(pathsStr, "\n")
		for _, path := range rawPaths {
			cleanPath := strings.TrimSpace(path)
			if cleanPath != "" {
				paths = append(paths, cleanPath)
			}
		}
	}

	q.PushJobStdout(j.ID, "Starting removal of media items from database")

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
		_ = q.CancelJob(j.ID)
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
		q.PushJobStdout(j.ID, "Cleanup completed successfully:")
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
		_ = q.CancelJob(j.ID)
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
			_ = q.CancelJob(j.ID)
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
		_ = q.CancelJob(j.ID)
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
	var ollamaModel string = appconfig.Get().OllamaModel

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
		"autotag":     true,
	}

	for _, mType := range metadataTypes {
		if !validTypes[strings.ToLower(mType)] {
			q.PushJobStdout(j.ID, fmt.Sprintf("Warning: unknown metadata type '%s' - valid types are: description, transcript, hash, dimensions, autotag", mType))
		}
	}

	q.PushJobStdout(j.ID, fmt.Sprintf("Starting metadata generation for types: %s", strings.Join(metadataTypes, ", ")))
	q.PushJobStdout(j.ID, fmt.Sprintf("Apply scope: %s", applyScope))
	q.PushJobStdout(j.ID, fmt.Sprintf("Overwrite existing: %t", overwrite))

	// Determine source of file list: query, explicit list, or entire DB
	var filesToProcess []string
	var err error

	if qstr, ok := extractQueryFromJob(j); ok {
		q.PushJobStdout(j.ID, fmt.Sprintf("Using query to select files: %s", qstr))
		filesToProcess, err = getMediaPathsByQuery(q.Db, qstr)
		if err != nil {
			q.PushJobStdout(j.ID, fmt.Sprintf("Error loading media paths for query: %v", err))
			q.ErrorJob(j.ID)
			return err
		}
	} else if strings.TrimSpace(j.Input) != "" {
		// If no input provided and no query, expect explicit paths rather than defaulting to DB
		raw := strings.TrimSpace(j.Input)
		if raw == "" {
			q.PushJobStdout(j.ID, "No input paths provided and no query; expecting comma/newline-separated file paths")
			q.CompleteJob(j.ID)
			return nil
		}
		// Parse input as file paths (newline and comma separated)
		inputPaths := parseInputPaths(raw)
		q.PushJobStdout(j.ID, fmt.Sprintf("Processing %d files from input list", len(inputPaths)))
		q.PushJobStdout(j.ID, fmt.Sprintf("Raw: %s", raw))
		for _, p := range inputPaths {
			// Convert to absolute path
			absPath, err := filepath.Abs(p)
			if err == nil {
				p = filepath.FromSlash(absPath)
			}
			// Check if file exists
			if _, err := os.Stat(p); os.IsNotExist(err) {
				q.PushJobStdout(j.ID, fmt.Sprintf("Warning: file does not exist: %s", p))
				continue
			}
			// Check if it's a media file
			if !isMediaFile(p) {
				q.PushJobStdout(j.ID, fmt.Sprintf("Warning: not a supported media file: %s", p))
				continue
			}
			filesToProcess = append(filesToProcess, p)
		}
		q.PushJobStdout(j.ID, "Processing files from input list")
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
		case "autotag":
			err = generateAutoTags(ctx, q, j.ID, filesToProcess, overwrite, ollamaModel)
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
			_ = q.CancelJob(j.ID)
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

// getAllMediaPaths removed: no longer used; metadata task expects explicit paths when no query

// getMediaPathsByQuery retrieves all media paths matching a search query using the media package
func getMediaPathsByQuery(db *sql.DB, query string) ([]string, error) {
	const pageSize = 1000
	offset := 0
	var paths []string

	for {
		items, hasMore, err := media.GetItems(db, offset, pageSize, query)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			paths = append(paths, it.Path)
		}
		if !hasMore {
			break
		}
		offset += pageSize
	}
	return paths, nil
}

// extractQueryFromJob checks args and input for a query string
// Supported forms:
//   - Argument flag: --query "..." or -q "..." or --query=... or -q=...
//   - Input prefix:  "query:..." or "q:..."
func extractQueryFromJob(j *jobqueue.Job) (string, bool) {
	// Check arguments
	for i := 0; i < len(j.Arguments); i++ {
		arg := j.Arguments[i]
		lower := strings.ToLower(arg)
		// Prefer base64-encoded query if provided
		if strings.HasPrefix(lower, "--query64=") {
			enc := arg[len("--query64="):]
			if dec, err := base64.StdEncoding.DecodeString(enc); err == nil {
				return string(dec), true
			}
		}
		if lower == "--query64" {
			if i+1 < len(j.Arguments) {
				enc := j.Arguments[i+1]
				if dec, err := base64.StdEncoding.DecodeString(enc); err == nil {
					return string(dec), true
				}
			}
		}
		if lower == "--query" {
			if i+1 < len(j.Arguments) {
				start := i + 1
				end := start
				for end < len(j.Arguments) {
					next := j.Arguments[end]
					if strings.HasPrefix(next, "-") && end > start { // stop at next flag once we have at least one token
						break
					}
					end++
				}
				return strings.Join(j.Arguments[start:end], " "), true
			}
		}
		if strings.HasPrefix(lower, "--query=") {
			return arg[len("--query="):], true
		}
	}

	// Check input prefix
	input := strings.TrimSpace(j.Input)
	if input != "" {

		// If the entire command line was placed in input, only extract explicit --query/--query64 flags
		tokens := tokenizeCommandLine(input)
		for i := 0; i < len(tokens); i++ {
			lt := strings.ToLower(tokens[i])
			// Handle base64 query in tokenized input
			if lt == "--query64" {
				if i+1 < len(tokens) {
					start := i + 1
					end := start
					for end < len(tokens) {
						next := tokens[end]
						if strings.HasPrefix(next, "-") && end > start {
							break
						}
						end++
					}
					joined := strings.Join(tokens[start:end], " ")
					if dec, err := base64.StdEncoding.DecodeString(joined); err == nil {
						return string(dec), true
					}
				}
			}
			if lt == "--query" {
				if i+1 < len(tokens) {
					start := i + 1
					end := start
					for end < len(tokens) {
						next := tokens[end]
						if strings.HasPrefix(next, "-") && end > start {
							break
						}
						end++
					}
					return strings.Join(tokens[start:end], " "), true
				}
			}
			if strings.HasPrefix(lt, "--query=") {
				return tokens[i][len("--query="):], true
			}
			if strings.HasPrefix(lt, "--query64=") {
				enc := tokens[i][len("--query64="):]
				if dec, err := base64.StdEncoding.DecodeString(enc); err == nil {
					return string(dec), true
				}
			}
		}
	}

	return "", false
}

// tokenizeCommandLine splits a command line string into tokens, respecting simple quotes
func tokenizeCommandLine(s string) []string {
	var tokens []string
	var b strings.Builder
	inQuotes := false
	quoteChar := byte(0)

	flush := func() {
		if b.Len() > 0 {
			tokens = append(tokens, b.String())
			b.Reset()
		}
	}

	for i := 0; i < len(s); i++ {
		c := s[i]
		if inQuotes {
			if c == quoteChar {
				inQuotes = false
				continue
			}
			b.WriteByte(c)
			continue
		}
		if c == '"' || c == '\'' {
			inQuotes = true
			quoteChar = c
			continue
		}
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			flush()
			continue
		}
		b.WriteByte(c)
	}
	flush()
	return tokens
}

// looksLikeSearchQuery removed: explicit flags only

// parseInputPaths parses newline and comma separated file paths from j.Input
func parseInputPaths(raw string) []string {
	var paths []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		for _, part := range strings.Split(line, ",") {
			p := strings.Trim(strings.TrimSpace(part), `"'`)
			if p != "" {
				paths = append(paths, p)
			}
		}
	}
	return paths
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
		description, err := describeFileWithOllama(ctx, filePath, model)
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
		transcript, err := generateTranscriptWithFasterWhisper(ctx, filePath)
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
func describeFileWithOllama(ctx context.Context, mediaPath, model string) (string, error) {
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

		ffmpegCmd := exec.CommandContext(ctx, "ffmpeg",
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
	description, err := callOllamaVision(ctx, resizedPath, model)
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
func callOllamaVision(ctx context.Context, imagePath, model string) (string, error) {
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
		"prompt": %s,
		"images": ["%s"]
	}`, model, strconv.Quote(appconfig.Get().DescribePrompt), b64)

	// Create request
	base := strings.TrimRight(appconfig.Get().OllamaBaseURL, "/")
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/api/generate", strings.NewReader(requestJSON))
	if err != nil {
		return "", fmt.Errorf("failed to build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Send request
	client := &http.Client{Timeout: 600 * time.Second}
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
func generateTranscriptWithFasterWhisper(ctx context.Context, filePath string) (string, error) {
	// Run faster-whisper (path configurable)
	exePath := appconfig.Get().FasterWhisperPath
	if strings.TrimSpace(exePath) == "" {
		exePath = "faster-whisper-xxl.exe"
	}
	cmd := exec.CommandContext(ctx,
		exePath,
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

	// Build source path list from query or input
	var cleanedPaths []string
	if qstr, ok := extractQueryFromJob(j); ok {
		q.PushJobStdout(j.ID, fmt.Sprintf("Using query to select files: %s", qstr))
		mediaPaths, err := getMediaPathsByQuery(q.Db, qstr)
		if err != nil {
			q.PushJobStdout(j.ID, fmt.Sprintf("Error loading media paths for query: %v", err))
			q.ErrorJob(j.ID)
			return err
		}
		cleanedPaths = mediaPaths
	} else {
		// Parse newline-separated paths from input with better handling
		pathsStr := strings.TrimSpace(j.Input)
		if pathsStr == "" {
			q.PushJobStdout(j.ID, "No paths provided for moving")
			q.CompleteJob(j.ID)
			return nil
		}

		// Split by newline and clean each path more thoroughly
		rawPaths := strings.Split(pathsStr, "\n")
		for _, rawPath := range rawPaths {
			cleanPath := strings.TrimSpace(rawPath)
			if cleanPath == "" {
				continue
			}

			// Remove any surrounding quotes that might be present
			cleanPath = strings.Trim(cleanPath, `"'`)
			if cleanPath != "" {
				cleanedPaths = append(cleanedPaths, cleanPath)
			}
		}
	}

	if len(cleanedPaths) == 0 {
		q.PushJobStdout(j.ID, "No valid paths found after parsing input")
		q.CompleteJob(j.ID)
		return nil
	}

	q.PushJobStdout(j.ID, fmt.Sprintf("Starting move operation to target directory: %s", targetDir))
	q.PushJobStdout(j.ID, fmt.Sprintf("Parsed %d paths from input", len(cleanedPaths)))

	// Clean and validate paths
	var validPaths []string
	for _, path := range cleanedPaths {
		// Convert to absolute path
		absPath, err := filepath.Abs(path)
		if err != nil {
			q.PushJobStdout(j.ID, fmt.Sprintf("Warning: could not resolve absolute path for %s: %v", path, err))
			continue
		}
		cleanPath := filepath.FromSlash(absPath)

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
		absPrefix, err := filepath.Abs(specifiedPrefix)
		if err == nil {
			prefixToUse = filepath.FromSlash(absPrefix)
		} else {
			prefixToUse = filepath.Clean(specifiedPrefix)
		}
		q.PushJobStdout(j.ID, fmt.Sprintf("Using specified prefix: %s", prefixToUse))
	} else {
		// Find common prefix of all source paths to preserve structure
		prefixToUse = findCommonPrefix(validPaths)
		if prefixToUse == "" {
			q.PushJobStdout(j.ID, "Warning: No common prefix found, files will be moved to target directory root")
		} else {
			q.PushJobStdout(j.ID, fmt.Sprintf("Using calculated common prefix: %s", prefixToUse))
		}
	}

	// Process each file
	moveCount := 0
	updateCount := 0
	for i, srcPath := range validPaths {
		// Check if context was cancelled
		select {
		case <-ctx.Done():
			q.PushJobStdout(j.ID, "Task was canceled")
			_ = q.CancelJob(j.ID)
			return ctx.Err()
		default:
		}

		// Calculate destination path while preserving structure
		var relativePath string
		if prefixToUse != "" && strings.HasPrefix(srcPath, prefixToUse) {
			relativePath = strings.TrimPrefix(srcPath, prefixToUse)
			relativePath = strings.TrimPrefix(relativePath, string(filepath.Separator))
		} else {
			// If no common prefix or path doesn't start with prefix, just use filename
			relativePath = filepath.Base(srcPath)
		}

		// Ensure we have a valid relative path
		if relativePath == "" {
			relativePath = filepath.Base(srcPath)
		}

		destPath := filepath.Join(targetDir, relativePath)

		// Check if destination already exists
		if _, err := os.Stat(destPath); err == nil {
			q.PushJobStdout(j.ID, fmt.Sprintf("Warning: destination already exists, skipping: %s", destPath))
			continue
		}

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
		q.PushJobStdout(j.ID, fmt.Sprintf("Moved: %s -> %s", srcPath, destPath))

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

	// Handle edge case where first path is at root
	if prefix == "." || prefix == "/" || (len(prefix) == 3 && prefix[1] == ':' && prefix[2] == '\\') {
		return ""
	}

	for _, path := range paths[1:] {
		pathDir := filepath.Dir(path)

		// Handle edge case where any path is at root
		if pathDir == "." || pathDir == "/" || (len(pathDir) == 3 && pathDir[1] == ':' && pathDir[2] == '\\') {
			return ""
		}

		// Find common prefix between current prefix and this path's directory
		newPrefix := ""
		prefixParts := strings.Split(filepath.Clean(prefix), string(filepath.Separator))
		pathParts := strings.Split(filepath.Clean(pathDir), string(filepath.Separator))

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

	// Clean the final prefix and ensure it's a valid directory path
	if prefix != "" {
		prefix = filepath.Clean(prefix)
		// Ensure prefix ends with separator for consistent trimming
		if !strings.HasSuffix(prefix, string(filepath.Separator)) {
			prefix += string(filepath.Separator)
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

// getAllAvailableTags fetches all unique tags and their categories from the database
func getAllAvailableTags(db *sql.DB) ([]TagInfo, error) {
	query := `
		SELECT DISTINCT tag_label, category_label 
		FROM media_tag_by_category 
		ORDER BY category_label, tag_label
	`

	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("failed to query available tags: %w", err)
	}
	defer rows.Close()

	var tags []TagInfo
	for rows.Next() {
		var tag TagInfo
		if err := rows.Scan(&tag.Label, &tag.Category); err != nil {
			return nil, fmt.Errorf("failed to scan tag row: %w", err)
		}
		tags = append(tags, tag)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating over tag rows: %w", err)
	}

	return tags, nil
}

// TagInfo represents a tag with its category for the auto-tagging system
type TagInfo struct {
	Label    string
	Category string
}

// generateAutoTags generates automatic tags for media files using vision model
func generateAutoTags(ctx context.Context, q *jobqueue.Queue, jobID string, filePaths []string, overwrite bool, model string) error {
	// First, get all available tags from the database
	availableTags, err := getAllAvailableTags(q.Db)
	if err != nil {
		return fmt.Errorf("failed to fetch available tags: %w", err)
	}

	if len(availableTags) == 0 {
		q.PushJobStdout(jobID, "No tags available in database for auto-tagging")
		return nil
	}

	q.PushJobStdout(jobID, fmt.Sprintf("Found %d available tags in database", len(availableTags)))

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

		// Skip if not an image file (auto-tagging currently only works for images)
		ext := strings.ToLower(filepath.Ext(filePath))
		isImage := false
		switch ext {
		case ".jpg", ".jpeg", ".png", ".bmp", ".webp", ".gif", ".tif", ".tiff", ".heic":
			isImage = true
		}

		if !isImage {
			continue
		}

		// Check if tags already exist and we're not overwriting
		if !overwrite {
			existingTags, err := getExistingTagsForFile(q.Db, filePath)
			if err != nil {
				q.PushJobStdout(jobID, fmt.Sprintf("Warning: failed to check existing tags for %s: %v", filePath, err))
				continue
			}
			if len(existingTags) > 0 {
				continue
			}
		}

		// Generate auto tags using vision model
		selectedTags, err := generateAutoTagsWithVision(ctx, filePath, availableTags, model)
		if err != nil {
			q.PushJobStdout(jobID, fmt.Sprintf("Warning: failed to auto-tag %s: %v", filePath, err))
			continue
		}

		if len(selectedTags) == 0 {
			q.PushJobStdout(jobID, fmt.Sprintf("No tags selected for: %s", filePath))
			continue
		}

		// Remove existing tags if overwriting
		if overwrite {
			err = removeExistingTagsForFile(q.Db, filePath)
			if err != nil {
				q.PushJobStdout(jobID, fmt.Sprintf("Warning: failed to remove existing tags for %s: %v", filePath, err))
				continue
			}
		}

		// Insert the selected tags into the database
		err = insertTagsForFile(q.Db, filePath, selectedTags)
		if err != nil {
			q.PushJobStdout(jobID, fmt.Sprintf("Warning: failed to insert tags for %s: %v", filePath, err))
			continue
		}

		processed++
		tagLabels := make([]string, len(selectedTags))
		for i, tag := range selectedTags {
			tagLabels[i] = tag.Label
		}
		q.PushJobStdout(jobID, fmt.Sprintf("Auto-tagged %s with: %s", filepath.Base(filePath), strings.Join(tagLabels, ", ")))

		if processed%10 == 0 || processed == len(filePaths) {
			q.PushJobStdout(jobID, fmt.Sprintf("Auto-tag progress: %d image files processed", processed))
		}
	}

	q.PushJobStdout(jobID, fmt.Sprintf("Generated auto-tags for %d image files", processed))
	return nil
}

// getExistingTagsForFile checks if a file already has tags
func getExistingTagsForFile(db *sql.DB, filePath string) ([]TagInfo, error) {
	query := `
		SELECT tag_label, category_label 
		FROM media_tag_by_category 
		WHERE media_path = ?
	`

	rows, err := db.Query(query, filePath)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tags []TagInfo
	for rows.Next() {
		var tag TagInfo
		if err := rows.Scan(&tag.Label, &tag.Category); err != nil {
			return nil, err
		}
		tags = append(tags, tag)
	}

	return tags, nil
}

// removeExistingTagsForFile removes all existing tags for a file
func removeExistingTagsForFile(db *sql.DB, filePath string) error {
	query := `DELETE FROM media_tag_by_category WHERE media_path = ?`
	_, err := db.Exec(query, filePath)
	return err
}

// insertTagsForFile inserts tags for a file into the database
func insertTagsForFile(db *sql.DB, filePath string, tags []TagInfo) error {
	// Ensure tag definitions exist in tags table first
	if err := ensureTagsExist(db, tags); err != nil {
		return err
	}
	stmt := `INSERT INTO media_tag_by_category (media_path, tag_label, category_label) VALUES (?, ?, ?)`

	for _, tag := range tags {
		_, err := db.Exec(stmt, filePath, tag.Label, tag.Category)
		if err != nil {
			return fmt.Errorf("failed to insert tag %s/%s: %w", tag.Category, tag.Label, err)
		}
	}

	return nil
}

// ensureTagsExist inserts any missing tags into the tag table.
// The tag table is expected to have columns: label, category_label
func ensureTagsExist(db *sql.DB, tags []TagInfo) error {
	if len(tags) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("ensureTagsExist: begin tx: %w", err)
	}
	defer tx.Rollback()

	insertSQL := `INSERT OR IGNORE INTO tag (label, category_label) VALUES (?, ?)`
	for _, t := range tags {
		if strings.TrimSpace(t.Label) == "" {
			continue
		}
		if _, err := tx.Exec(insertSQL, t.Label, t.Category); err != nil {
			return fmt.Errorf("ensureTagsExist: insert %s/%s: %w", t.Category, t.Label, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ensureTagsExist: commit: %w", err)
	}
	return nil
}

// autotagTask runs onnxtag.exe and writes suggested tag assignments
func autotagTask(j *jobqueue.Job, q *jobqueue.Queue, mu *sync.Mutex) error {
	ctx := j.Ctx

	// Determine source paths: query or input list
	var paths []string
	if qstr, ok := extractQueryFromJob(j); ok {
		q.PushJobStdout(j.ID, fmt.Sprintf("autotag: using query to select files: %s", qstr))
		mediaPaths, err := getMediaPathsByQuery(q.Db, qstr)
		if err != nil {
			q.PushJobStdout(j.ID, "autotag: failed to load paths from query: "+err.Error())
			q.ErrorJob(j.ID)
			return err
		}
		paths = mediaPaths
	} else {
		raw := strings.TrimSpace(j.Input)
		if raw == "" {
			q.PushJobStdout(j.ID, "autotag: no image path provided in job input or query flag")
			q.CompleteJob(j.ID)
			return nil
		}

		// Accept comma-separated or newline-separated list of files
		paths = parseInputPaths(raw)
	}

	// Ensure the Suggested category exists
	if err := ensureCategoryExists(q.Db, "Suggested", 0); err != nil {
		q.PushJobStdout(j.ID, "autotag: failed to ensure category: "+err.Error())
		q.ErrorJob(j.ID)
		return err
	}

	if len(paths) == 0 {
		q.PushJobStdout(j.ID, "autotag: no valid paths parsed from input")
		q.CompleteJob(j.ID)
		return nil
	}

	// Process sequentially
	for idx, imagePath := range paths {
		select {
		case <-ctx.Done():
			q.PushJobStdout(j.ID, "autotag: task canceled")
			_ = q.CancelJob(j.ID)
			return ctx.Err()
		default:
		}

		// Build command arguments from configuration
		cfg := appconfig.Get()
		args := []string{}
		if strings.TrimSpace(cfg.OnnxTagger.LabelsPath) != "" {
			args = append(args, `--labels=`+cfg.OnnxTagger.LabelsPath)
		}
		if strings.TrimSpace(cfg.OnnxTagger.ConfigPath) != "" {
			args = append(args, `--config=`+cfg.OnnxTagger.ConfigPath)
		}
		if strings.TrimSpace(cfg.OnnxTagger.ModelPath) != "" {
			args = append(args, `--model=`+cfg.OnnxTagger.ModelPath)
		}
		if strings.TrimSpace(cfg.OnnxTagger.ORTSharedLibraryPath) != "" {
			args = append(args, `--ort=`+cfg.OnnxTagger.ORTSharedLibraryPath)
		}
		if cfg.OnnxTagger.GeneralThreshold > 0 {
			args = append(args, `--general-thresh=`+fmt.Sprintf("%g", cfg.OnnxTagger.GeneralThreshold))
		}
		if cfg.OnnxTagger.CharacterThreshold > 0 {
			args = append(args, `--character-thresh=`+fmt.Sprintf("%g", cfg.OnnxTagger.CharacterThreshold))
		}
		args = append(args, `--image=`+imagePath)

		q.PushJobStdout(j.ID, fmt.Sprintf("autotag: [%d/%d] tagging %s", idx+1, len(paths), imagePath))

		// Launch onnxtag via embedded exec (falls back to system PATH)
		cmd, cleanup, err := embedexec.GetExec(ctx, "onnxtag", args...)
		if err != nil {
			q.PushJobStdout(j.ID, "autotag: failed to prepare onnxtag: "+err.Error())
			q.ErrorJob(j.ID)
			return err
		}
		if cleanup != nil {
			defer cleanup()
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			q.PushJobStdout(j.ID, "autotag: failed to get stdout pipe: "+err.Error())
			q.ErrorJob(j.ID)
			return err
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			q.PushJobStdout(j.ID, "autotag: failed to get stderr pipe: "+err.Error())
			q.ErrorJob(j.ID)
			return err
		}

		// Drain stderr immediately to avoid pipe blocking
		doneErr := make(chan struct{})
		go func() {
			s := bufio.NewScanner(stderr)
			for s.Scan() {
				_ = q.PushJobStdout(j.ID, "autotag stderr: "+s.Text())
			}
			close(doneErr)
		}()

		if err := cmd.Start(); err != nil {
			q.PushJobStdout(j.ID, "autotag: failed to start onnxtag: "+err.Error())
			q.ErrorJob(j.ID)
			return err
		}

		// Collect stdout lines as tags (one per line: either "tag" or "tag:score")
		var tags []string
		scan := bufio.NewScanner(stdout)
		for scan.Scan() {
			line := strings.TrimSpace(scan.Text())
			if line != "" {
				tags = append(tags, line)
				_ = q.PushJobStdout(j.ID, "autotag: "+line)
			}
		}
		_ = cmd.Wait()
		<-doneErr

		if len(tags) == 0 {
			q.PushJobStdout(j.ID, "autotag: no tags returned")
			continue
		}

		// Convert to TagInfo with Suggested category
		var tagInfos []TagInfo
		for _, t := range tags {
			// accept "name" or "name:score"; take left side as name
			name := t
			if pos := strings.LastIndex(t, ":"); pos > 0 {
				name = strings.TrimSpace(t[:pos])
			}
			if name == "" {
				continue
			}
			tagInfos = append(tagInfos, TagInfo{Label: name, Category: "Suggested"})
		}

		// Insert tag assignments
		if err := insertTagsForFile(q.Db, imagePath, tagInfos); err != nil {
			q.PushJobStdout(j.ID, "autotag: failed to insert tags: "+err.Error())
			q.ErrorJob(j.ID)
			return err
		}

		q.PushJobStdout(j.ID, fmt.Sprintf("autotag: wrote %d Suggested tags for %s", len(tagInfos), imagePath))
	}

	q.CompleteJob(j.ID)
	return nil
}

// ffmpegTask runs ffmpeg per selected file. It accepts either a query (--query/--query64)
// to select files from the DB, or newline/comma-separated explicit file paths in j.Input.
// Arguments can include the following placeholders, which will be expanded per file:
//
//	{input}  - the absolute source file path
//	{dir}    - directory of source file
//	{base}   - base filename (with extension)
//	{name}   - filename without extension
//	{ext}    - extension including dot (e.g., .mp4)
//	{idx}    - 1-based index of the file in the batch
//
// To output to the same folder, you can refer to {dir}\{name}_out{ext}, etc.
func ffmpegTask(j *jobqueue.Job, q *jobqueue.Queue, mu *sync.Mutex) error {
	ctx := j.Ctx

	// Resolve file list via query or explicit input
	var files []string
	if qstr, ok := extractQueryFromJob(j); ok {
		q.PushJobStdout(j.ID, fmt.Sprintf("ffmpeg: using query to select files: %s", qstr))
		mediaPaths, err := getMediaPathsByQuery(q.Db, qstr)
		if err != nil {
			q.PushJobStdout(j.ID, "ffmpeg: failed to load paths from query: "+err.Error())
			q.ErrorJob(j.ID)
			return err
		}
		files = mediaPaths
	} else {
		raw := strings.TrimSpace(j.Input)
		if raw == "" {
			q.PushJobStdout(j.ID, "ffmpeg: no input paths or query provided")
			q.CompleteJob(j.ID)
			return nil
		}
		files = parseInputPaths(raw)
	}

	if len(files) == 0 {
		q.PushJobStdout(j.ID, "ffmpeg: no files to process")
		q.CompleteJob(j.ID)
		return nil
	}

	// The rest of the arguments are an ffmpeg argument template.
	// We will build a per-file argument list by replacing placeholders.
	// Example: -i {input} -c:v libx264 -crf 23 {dir}/{name}.mp4
	templateArgs := append([]string{}, j.Arguments...)
	if len(templateArgs) == 0 {
		q.PushJobStdout(j.ID, "ffmpeg: no arguments provided for ffmpeg")
		q.CompleteJob(j.ID)
		return nil
	}

	// Process each file
	for idx, src := range files {
		// Context check
		select {
		case <-ctx.Done():
			q.PushJobStdout(j.ID, "ffmpeg: task canceled")
			q.ErrorJob(j.ID)
			return ctx.Err()
		default:
		}

		abs := src
		if a, err := filepath.Abs(src); err == nil {
			abs = filepath.FromSlash(a)
		}
		dir := filepath.Dir(abs)
		base := filepath.Base(abs)
		ext := filepath.Ext(abs)
		name := strings.TrimSuffix(base, ext)
		idxStr := strconv.Itoa(idx + 1)

		// Expand placeholders in a fresh slice
		expanded := make([]string, len(templateArgs))
		for i, ta := range templateArgs {
			s := ta
			s = strings.ReplaceAll(s, "{input}", abs)
			s = strings.ReplaceAll(s, "{dir}", dir)
			s = strings.ReplaceAll(s, "{base}", base)
			s = strings.ReplaceAll(s, "{name}", name)
			s = strings.ReplaceAll(s, "{ext}", ext)
			s = strings.ReplaceAll(s, "{idx}", idxStr)
			expanded[i] = s
		}

		// Ensure -i is present; if not, prepend -i {input}
		hasInput := false
		for _, e := range expanded {
			if e == "-i" {
				hasInput = true
				break
			}
			if strings.Contains(e, "-i=") || strings.Contains(e, "-i:") { // uncommon forms
				hasInput = true
				break
			}
		}
		finalArgs := expanded
		if !hasInput {
			finalArgs = append([]string{"-i", abs}, expanded...)
		}

		q.PushJobStdout(j.ID, "ffmpeg: running on "+base)

		// Launch ffmpeg via embedded exec (falls back to system PATH)
		cmd, cleanup, err := embedexec.GetExec(ctx, "ffmpeg", finalArgs...)
		if err != nil {
			q.PushJobStdout(j.ID, "ffmpeg: failed to prepare: "+err.Error())
			q.ErrorJob(j.ID)
			return err
		}
		if cleanup != nil {
			defer cleanup()
		}

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			q.PushJobStdout(j.ID, "ffmpeg: stdout pipe error: "+err.Error())
			q.ErrorJob(j.ID)
			return err
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			q.PushJobStdout(j.ID, "ffmpeg: stderr pipe error: "+err.Error())
			q.ErrorJob(j.ID)
			return err
		}

		// Drain stderr to log ffmpeg progress/errors
		doneErr := make(chan struct{})
		go func() {
			s := bufio.NewScanner(stderr)
			for s.Scan() {
				_ = q.PushJobStdout(j.ID, "ffmpeg: "+s.Text())
			}
			close(doneErr)
		}()

		if err := cmd.Start(); err != nil {
			q.PushJobStdout(j.ID, "ffmpeg: failed to start: "+err.Error())
			q.ErrorJob(j.ID)
			return err
		}

		// Stream stdout too
		scan := bufio.NewScanner(stdout)
		for scan.Scan() {
			_ = q.PushJobStdout(j.ID, scan.Text())
		}
		_ = cmd.Wait()
		<-doneErr

		q.PushJobStdout(j.ID, "ffmpeg: completed for "+base)
	}

	q.CompleteJob(j.ID)
	return nil
}

// ensureCategoryExists inserts the category if it doesn't already exist.
// The category table is expected to have columns: label, weight
func ensureCategoryExists(db *sql.DB, label string, weight int) error {
	label = strings.TrimSpace(label)
	if label == "" {
		return errors.New("ensureCategoryExists: empty label")
	}
	_, err := db.Exec(`INSERT OR IGNORE INTO category (label, weight) VALUES (?, ?)`, label, weight)
	if err != nil {
		return fmt.Errorf("ensureCategoryExists: insert %s: %w", label, err)
	}
	return nil
}

// generateAutoTagsWithVision uses the vision model to select appropriate tags from available options
func generateAutoTagsWithVision(ctx context.Context, mediaPath string, availableTags []TagInfo, model string) ([]TagInfo, error) {
	ext := strings.ToLower(filepath.Ext(mediaPath))
	var tempImagePath string
	var cleanupPaths []string

	// Handle different file types (similar to description function)
	if ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".bmp" || ext == ".webp" {
		tempImagePath = mediaPath
	} else {
		// For videos, take a screenshot using ffmpeg
		screenshotPath := filepath.Join(os.TempDir(), "autotag_screenshot_"+filepath.Base(mediaPath)+".jpg")
		cleanupPaths = append(cleanupPaths, screenshotPath)

		ffmpegCmd := exec.CommandContext(ctx, "ffmpeg",
			"-ss", "1",
			"-i", mediaPath,
			"-frames:v", "1",
			"-q:v", "2",
			"-y",
			screenshotPath,
		)
		if err := ffmpegCmd.Run(); err != nil {
			return nil, fmt.Errorf("ffmpeg screenshot failed: %w", err)
		}
		tempImagePath = screenshotPath
	}

	// Resize image if needed
	resizedPath, err := resizeImageIfNeeded(tempImagePath)
	if err != nil {
		for _, p := range cleanupPaths {
			_ = os.Remove(p)
		}
		return nil, fmt.Errorf("failed to resize image: %w", err)
	}
	if resizedPath != tempImagePath {
		cleanupPaths = append(cleanupPaths, resizedPath)
	}

	// Create the prompt with available tags
	selectedTags, err := callOllamaVisionForTags(ctx, resizedPath, availableTags, model)
	if err != nil {
		for _, p := range cleanupPaths {
			_ = os.Remove(p)
		}
		return nil, fmt.Errorf("ollama auto-tag call failed: %w", err)
	}

	// Cleanup temp files
	for _, p := range cleanupPaths {
		_ = os.Remove(p)
	}

	return selectedTags, nil
}

// callOllamaVisionForTags calls Ollama API to select appropriate tags for an image
func callOllamaVisionForTags(ctx context.Context, imagePath string, availableTags []TagInfo, model string) ([]TagInfo, error) {
	// Read image and convert to base64
	data, err := os.ReadFile(imagePath)
	if err != nil {
		return nil, fmt.Errorf("could not read image for Ollama: %w", err)
	}
	b64 := base64.StdEncoding.EncodeToString(data)

	// Build the tag options string for the prompt
	var tagOptions strings.Builder
	tagOptions.WriteString("Available tags by category:\n")

	// Group tags by category for better organization
	categoryMap := make(map[string][]string)
	for _, tag := range availableTags {
		categoryMap[tag.Category] = append(categoryMap[tag.Category], tag.Label)
	}

	for category, labels := range categoryMap {
		tagOptions.WriteString(fmt.Sprintf("- %s: %s\n", category, strings.Join(labels, ", ")))
	}

	// Create the prompt using template from config
	prompt := fmt.Sprintf(appconfig.Get().AutotagPrompt, tagOptions.String())

	// Log the final prompt for debugging
	log.Printf("AutoTag Vision Prompt for %s:\n%s", imagePath, prompt)

	// Build JSON payload for Ollama API
	requestJSON := fmt.Sprintf(`{
		"model": "%s",
		"stream": false,
		"prompt": %s,
		"images": ["%s"]
	}`, model, strconv.Quote(prompt), b64)

	// Create request
	base := strings.TrimRight(appconfig.Get().OllamaBaseURL, "/")
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/api/generate", strings.NewReader(requestJSON))
	if err != nil {
		return nil, fmt.Errorf("failed to build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Send request
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama error: status=%d, body=%s", resp.StatusCode, string(bodyBytes))
	}

	// Read response
	respData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body failed: %w", err)
	}

	// Parse JSON response
	var response struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal(respData, &response); err != nil {
		return nil, fmt.Errorf("could not unmarshal Ollama response: %w", err)
	}

	// Log the raw response for debugging
	log.Printf("AutoTag Vision Raw Response for %s:\n%s", imagePath, response.Response)

	// Parse the tags from the response
	selectedTags, err := parseTagsFromResponse(response.Response, availableTags)
	if err != nil {
		return nil, fmt.Errorf("failed to parse tags from response: %w", err)
	}

	// Log the parsed tags for debugging
	tagNames := make([]string, len(selectedTags))
	for i, tag := range selectedTags {
		tagNames[i] = fmt.Sprintf("%s/%s", tag.Category, tag.Label)
	}
	log.Printf("AutoTag Vision Parsed Tags for %s: [%s]", imagePath, strings.Join(tagNames, ", "))

	return selectedTags, nil
}

// parseTagsFromResponse extracts valid tags from the Ollama response
func parseTagsFromResponse(response string, availableTags []TagInfo) ([]TagInfo, error) {
	// Try to find JSON array in the response
	response = strings.TrimSpace(response)

	// Find the JSON array in the response
	start := strings.Index(response, "[")
	end := strings.LastIndex(response, "]")

	if start == -1 || end == -1 || start >= end {
		log.Printf("AutoTag Parse: No valid JSON array found in response (start=%d, end=%d)", start, end)
		return []TagInfo{}, nil // Return empty if no valid JSON found
	}

	jsonStr := response[start : end+1]
	log.Printf("AutoTag Parse: Extracted JSON string: %s", jsonStr)

	// Parse the JSON array
	var rawTags []map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &rawTags); err != nil {
		log.Printf("AutoTag Parse: JSON unmarshal failed: %v", err)
		return []TagInfo{}, nil // Return empty if JSON parsing fails
	}

	log.Printf("AutoTag Parse: Successfully parsed %d raw tags from JSON", len(rawTags))

	// Create a map of available tags for quick lookup
	tagMap := make(map[string]TagInfo)
	for _, tag := range availableTags {
		key := strings.ToLower(tag.Category) + ":" + strings.ToLower(tag.Label)
		tagMap[key] = tag
	}

	// Validate and filter the selected tags
	var selectedTags []TagInfo
	for i, rawTag := range rawTags {
		labelInterface, hasLabel := rawTag["label"]
		categoryInterface, hasCategory := rawTag["category"]

		if !hasLabel || !hasCategory {
			log.Printf("AutoTag Parse: Raw tag %d missing label or category fields", i)
			continue
		}

		label, labelOk := labelInterface.(string)
		category, categoryOk := categoryInterface.(string)

		if !labelOk || !categoryOk {
			log.Printf("AutoTag Parse: Raw tag %d has non-string label or category", i)
			continue
		}

		// Check if this tag exists in our available tags
		key := strings.ToLower(category) + ":" + strings.ToLower(label)
		if validTag, exists := tagMap[key]; exists {
			selectedTags = append(selectedTags, validTag)
			log.Printf("AutoTag Parse: Validated tag %d: %s/%s", i, category, label)
		} else {
			log.Printf("AutoTag Parse: Tag %d not found in available tags: %s/%s (key: %s)", i, category, label, key)
		}
	}

	return selectedTags, nil
}
