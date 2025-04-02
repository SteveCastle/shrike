package tasks

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

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

func executeCommand(j *jobqueue.Job, q *jobqueue.Queue, mu *sync.Mutex) error {
	ctx := j.Ctx
	fmt.Println("Executing job:", j.ID, j.Command, j.Arguments, j.Input)

	// the script is located in ./scripts/{j.Command}.ps1
	script := fmt.Sprintf("./scripts/%s.ps1", j.Command)

	// append j.Input to the end of arguments
	powershellArgs := []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", script}
	doubleQuoteWrapped := fmt.Sprintf("\"%s\"", j.Input)
	scriptArgs := append(j.Arguments, doubleQuoteWrapped)
	args := append(powershellArgs, scriptArgs...)
	cmd := exec.CommandContext(ctx, "powershell", args...)

	go func() {
		<-ctx.Done()
		if cmd.Process != nil {
			exec.Command("taskkill", "/F", "/T", "/PID", fmt.Sprintf("%d", cmd.Process.Pid)).Run()
		}
	}()

	// Provide input to stdin if specified
	if j.StdIn != nil {
		cmd.Stdin = j.StdIn
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		// If we can't get a stdout pipe, mark job as errored.
		_ = q.ErrorJob(j.ID)
		fmt.Println(err)
		return err
	}

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		// If we can't get a stderr pipe, mark job as errored.
		_ = q.ErrorJob(j.ID)
		fmt.Println(err)
		return err
	}

	// Start the command
	if err := cmd.Start(); err != nil {
		_ = q.ErrorJob(j.ID)
		fmt.Println(err)
		return err
	}

	// We'll use two goroutines to read stdout and stderr
	doneReading := make(chan struct{})
	doneCount := 0
	totalReaders := 2

	// Helper function to scan a pipe and push lines to the queue
	scanAndPush := func(pipe io.ReadCloser) {
		scanner := bufio.NewScanner(pipe)
		for scanner.Scan() {
			line := scanner.Text()
			// Push each line (stdout or stderr) to the queue as it arrives
			_ = q.PushJobStdout(j.ID, line)
		}
		if err := scanner.Err(); err != nil && err != io.EOF {
			_ = q.ErrorJob(j.ID)
			fmt.Println(err)
		}

		// Signal one reader is done
		mu.Lock()
		doneCount++
		if doneCount == totalReaders {
			close(doneReading)
		}
		mu.Unlock()
	}

	// Read stdout lines in a separate goroutine
	go scanAndPush(stdoutPipe)

	// Read stderr lines in a separate goroutine
	go scanAndPush(stderrPipe)

	// Wait for the command to finish
	err = cmd.Wait()

	// Ensure we've finished reading from both stdout and stderr
	<-doneReading

	// Check if the context was canceled
	select {
	case <-ctx.Done():
		// Job was canceled. Mark as errored (or canceled).
		_ = q.ErrorJob(j.ID)
		fmt.Println(err)
		return ctx.Err()
	default:
		// Context not canceled, proceed with normal error handling.
	}

	if err != nil {
		// Command failed
		_ = q.ErrorJob(j.ID)
		fmt.Println(err)
		return err
	}

	// Command succeeded
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
