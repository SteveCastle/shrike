package runners

import (
	"sync"

	"github.com/stevecastle/shrike/jobqueue"
	"github.com/stevecastle/shrike/tasks"
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

		tasksMap := tasks.GetTasks()
		if task, exists := tasksMap[j.Command]; exists {
			// Ensure job state is finalized even if task forgets
			if err := task.Fn(j, r.queue, &r.mu); err != nil {
				// If context is canceled, prefer Cancelled state
				select {
				case <-j.Ctx.Done():
					_ = r.queue.CancelJob(j.ID)
				default:
					_ = r.queue.ErrorJob(j.ID)
				}
			}
		} else {
			// If the task is not found, we should mark the job as failed.
			r.queue.PushJobStdout(j.ID, "Task not found: "+j.Command)
			r.queue.ErrorJob(j.ID)
			return
		}
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
		// No job available or error encountered.
		return
	}

	r.runJob(job)
}
