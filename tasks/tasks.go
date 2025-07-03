package tasks

import (
	"bufio"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

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
	RegisterTask("remove", "remove", removeFromDB)
	RegisterTask("cleanup", "CleanUp", cleanUpFn)
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
