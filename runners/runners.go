package runners

import (
	"fmt"
	"io"
	"os/exec"
	"sync"

	"github.com/stevecastle/shrike/jobqueue"
)

// Runners manages a pool of concurrent job runners.
type Runners struct {
	queue         *jobqueue.Queue
	maxConcurrent int
	mu            sync.Mutex
	running       int
}

// New creates a new Runners instance with a given concurrency level.
func New(queue *jobqueue.Queue, maxConcurrent int) *Runners {
	r := &Runners{
		queue:         queue,
		maxConcurrent: maxConcurrent,
	}

	// Start a goroutine to listen to the signal channel.
	go func() {
		for range r.queue.Signal {
			// When a signal is received, attempt to pick up a new job.
			r.CheckForJobs()
		}
	}()

	return r
}

// CheckForJobs attempts to claim and run a new job if the runners are not at capacity.
// This can be called externally or triggered by signals.
func (r *Runners) CheckForJobs() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.tryFetchJobAndRun()
}

// runJob starts a single job in a separate goroutine. Once it completes,
// we decrement the running count and attempt to fetch the next job.
func (r *Runners) runJob(j *jobqueue.Job) {
	r.running++
	go func() {
		defer func() {
			r.mu.Lock()
			r.running--
			// After finishing this job, try fetching another one
			r.tryFetchJobAndRun()
			r.mu.Unlock()
		}()

		r.executeCommand(j)
	}()
}

// tryFetchJobAndRun tries to fetch a new job if capacity allows.
func (r *Runners) tryFetchJobAndRun() {
	if r.running >= r.maxConcurrent {
		// Already at capacity.
		return
	}

	job, err := r.queue.ClaimJob()
	if err != nil || job == nil {
		// No job available or error encountered (could log or handle err if needed).
		return
	}

	r.runJob(job)
}

// executeCommand runs the job command and updates the job state accordingly.
func (r *Runners) executeCommand(j *jobqueue.Job) {
	ctx := j.Ctx
	fmt.Println("Executing job:", j.ID, j.Command, j.Arguments)
	// append j.Input to the end of arguments
	args := append(j.Arguments, j.Input)

	cmd := exec.CommandContext(ctx, j.Command, args...,)

	// Provide input to stdin if specified
	if j.Input != "" {
		cmd.Stdin = stringToReadCloser(j.Input)
	}

	err := cmd.Run()

	// Check if the context was canceled during execution
	select {
	case <-ctx.Done():
		// Job was canceled. We consider it errored due to cancellation.
		_ = r.queue.ErrorJob(j.ID) // Consider handling error returns.
		return
	default:
		// Context not canceled, proceed with normal error handling.
	}

	if err != nil {
		// Command failed
		_ = r.queue.ErrorJob(j.ID) // Consider handling error returns.
		return
	}

	// Command succeeded
	fmt.Println("Job completed:", j.ID)
	_ = r.queue.CompleteJob(j.ID) // Consider handling error returns.
}

// stringToReadCloser helps provide input to the command's stdin.
func stringToReadCloser(s string) io.ReadCloser {
	return &stringReadCloser{data: []byte(s)}
}

type stringReadCloser struct {
	data []byte
}

func (r *stringReadCloser) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func (r *stringReadCloser) Close() error {
	return nil
}
