package tasks

import (
	"time"

	"github.com/stevecastle/shrike/jobqueue"
)

type Task struct {
	ID   string                     `json:"id"`
	Name string                     `json:"name"`
	Fn   func(j *jobqueue.Job, q *jobqueue.Queue  ) error `json:"-"`
}

type TaskMap map[string]Task

var tasks TaskMap

func init() {
	tasks = make(TaskMap)
	// Register a task that waits 5 seconds then completes
	RegisterTask("wait", "Wait", func(j *jobqueue.Job, q *jobqueue.Queue) error {
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
	})
}

func RegisterTask(id, name string, fn func(j *jobqueue.Job, q *jobqueue.Queue) error) {
	tasks[id] = Task{
		ID:   id,
		Name: name,
		Fn:   fn,
	}
}


func GetTasks() TaskMap {

	return tasks
}
