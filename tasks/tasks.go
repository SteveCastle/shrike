package tasks

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/stevecastle/shrike/embedexec"
	"github.com/stevecastle/shrike/jobqueue"
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

	// Load all files in the ./scripts directory and create a command with the file name minus the extension
	// This will allow us to execute any script in the ./scripts directory by sending a command with the same name
	// as the script file.
	// This is a simple way to allow for dynamic script execution without hardcoding each script.

	// Open the scripts directory
	dir, err := os.Open("./scripts")
	if err != nil {
		fmt.Println("Error opening scripts directory:", err)
		return
	}
	defer dir.Close()

	// Read all files in the directory
	files, err := dir.Readdir(0)
	if err != nil {
		fmt.Println("Error reading scripts directory:", err)
		return
	}

	// Loop through each file
	for _, file := range files {
		// If the file is a regular file (not a directory or symlink)
		if file.Mode().IsRegular() {
			// Get the file name
			name := file.Name()
			// Get the file name without the extension
			command := strings.TrimSuffix(name, filepath.Ext(name))
			// Register the task with the command name
			RegisterTask(command, command, executeCommand)
		}
	}

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
	// Heuristic: if it contains a path separator or a "." near the end,
	// treat it as a path.  Tweak as needed for your domain.
	if !strings.ContainsAny(a, `/\`) && !strings.Contains(a, ".") {
		return a // plain argument (flag, keyword, etc.)
	}

	clean := filepath.Clean(a)

	// Convert to absolute when possible (fail-safe if $PWD disappears).
	if abs, err := filepath.Abs(clean); err == nil {
		clean = abs
	}

	// Use platform-native separators so the invoked program sees the
	// right thing whether it’s on Windows or POSIX.
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
	cmd, cleanup, err := embedexec.Run(ctx, j.Command, scriptArgs...)
	if err != nil {
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
		_ = q.ErrorJob(j.ID)
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		_ = q.ErrorJob(j.ID)
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
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
		_ = q.ErrorJob(j.ID)
		return ctx.Err()
	default:
	}

	if err != nil {
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
