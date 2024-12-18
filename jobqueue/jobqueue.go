package jobqueue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"text/template"
	"time"

	"github.com/google/uuid"
	"github.com/stevecastle/shrike/stream"
)

// JobState represents the current state of a job in the queue.
type JobState int

type SerialziedEvent struct {
	UpdateType string `json:"updateType"`
	Job Job `json:"job"`
	HTML string `json:"html"`
}

const (
	StatePending JobState = iota
	StateInProgress
	StateCompleted
	StateCancelled
	StateError
)

var tmpl = template.Must(template.ParseFiles("client/templates/job.go.html"))


func (s JobState) String() string {
	switch s {
	case StatePending:
		return "Pending"
	case StateInProgress:
		return "InProgress"
	case StateCompleted:
		return "Completed"
	case StateCancelled:
		return "Cancelled"
	case StateError:
		return "Error"
	default:
		return "Unknown"
	}
}

// Job represents an individual task in the queue.
type Job struct {
	ID           string `json:"id"` // Unique identifier for the job
	Command 	string `json:"command"`
	Arguments []string
	Input 	string `json:"input"`
	Dependencies []string `json:"dependencies"` // IDs of jobs that must complete before this one
	State        JobState `json:"state"`
	Ctx       context.Context    `json:"-"`
	Cancel   context.CancelFunc `json:"-"`

	// Timestamps for various states
	CreatedAt   time.Time `json:"created_at"`
	ClaimedAt   time.Time `json:"claimed_at"`
	CompletedAt time.Time `json:"completed_at"`
	ErroredAt   time.Time `json:"errored_at"`
}

// Queue is a thread-safe structure that manages Jobs with dependencies.
type Queue struct {
	mu       sync.Mutex
	Jobs     map[string]*Job
	JobOrder []string // Keep track of the order in which jobs are added
	Signal chan string
}

// NewQueue initializes and returns a new Queue.
func NewQueue() *Queue {
	return &Queue{
		Jobs: make(map[string]*Job),
		Signal : make(chan string),
	}
}

// AddJob adds a new job to the queue with the given dependencies.
// It generates a UUID for the job and returns it.
func (q *Queue) AddJob(input string, command string, arguments []string) (string, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	id := uuid.NewString()
	if _, exists := q.Jobs[id]; exists {
		// Extremely unlikely to happen due to UUID uniqueness,
		// but we check for completeness.
		return "", errors.New("job with given ID already exists")
	}

	ctx, cancel := context.WithCancel(context.Background())
	job := &Job{
		ID:           id,
		Input: input,
		Command: command,
		Arguments: arguments,
		Dependencies: nil,
		State:        StatePending,
		Ctx: ctx,
		Cancel: cancel,
		CreatedAt:    time.Now(),
	}
	q.Jobs[id] = job
	q.JobOrder = append(q.JobOrder, id)

	// Broadcast the new job to the Signal channel
	q.Signal <- id
	error := createSerializedMessage("create", job)
	if error != nil {
		return "", error
	}

	return id, nil
}

// ClaimJob tries to find a pending job whose dependencies are all completed,
// in FIFO order. If successful, it returns the job and marks it as InProgress.
// If no suitable job is found, it returns nil and no error.
func (q *Queue) ClaimJob() (*Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	for _, jobID := range q.JobOrder {
		job := q.Jobs[jobID]
		if job.State == StatePending && q.canClaim(job) {
			job.State = StateInProgress
			job.ClaimedAt = time.Now()
			err := createSerializedMessage("update", job)
			if err != nil {
				return nil, err
			}
			return job, nil
		}
	}

	// No claimable job found
	return nil, nil
}

// canClaim checks if a job's dependencies are all completed.
func (q *Queue) canClaim(job *Job) bool {
	for _, dep := range job.Dependencies {
		depJob, exists := q.Jobs[dep]
		if !exists {
			// If dependency doesn't exist, can't claim
			return false
		}
		if depJob.State != StateCompleted {
			// If any dependency is not completed, can't claim
			return false
		}
	}
	return true
}

// ErrorJob sets a job's state to error if it is currently in progress.
func (q *Queue) ErrorJob(id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	job, exists := q.Jobs[id]
	if !exists {
		return errors.New("job not found")
	}

	if job.State != StateInProgress {
		return errors.New("job is not in progress, cannot set error")
	}

	job.State = StateError
	job.ErroredAt = time.Now()
	err := createSerializedMessage("update", job)
	if err != nil {
		return nil
	}
	return nil
}

// CancelJob sets a job's state to cancelled if it is currently pending.
func (q *Queue) CancelJob(id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	job, exists := q.Jobs[id]
	if !exists {
		return errors.New("job not found")
	}

	if job.State != StatePending && job.State != StateInProgress {
		return errors.New("job is not pending, or in progree, cannot cancel")
	}
	job.Cancel()
	job.State = StateCancelled

	err := createSerializedMessage("update", job)
	if err != nil {
		return err
	}

	return nil
}

// CompleteJob marks the specified job as completed if it is currently InProgress.
// Returns an error if the job does not exist, or if it's not in a valid state to be completed.
func (q *Queue) CompleteJob(id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	job, exists := q.Jobs[id]
	if !exists {
		return errors.New("job not found")
	}

	if job.State != StateInProgress {
		return errors.New("job is not in progress, cannot complete")
	}

	job.State = StateCompleted
	job.CompletedAt = time.Now()
	err := createSerializedMessage("update", job)
	if err != nil {
		return nil
	}
	return nil
}

// GetJobs returns a slice of all jobs in the queue sorted by CreatedAt time in descending order.

func (q *Queue) GetJobs() []Job {
	q.mu.Lock()
	defer q.mu.Unlock()
	length := len(q.Jobs)
	jobs := make([]Job, 0, length)
	for  i := length - 1; i >= 0; i-- {
		jobs = append(jobs, *q.Jobs[q.JobOrder[i]])
	}
	return jobs
}

// createSerializedMessage serializes the given job and broadcasts it with the specified update type.
// It returns an error if template execution or JSON marshalling fails.
func createSerializedMessage(updateType string, job *Job) error {
	var html bytes.Buffer
	if err := tmpl.ExecuteTemplate(&html, "job", job); err != nil {
		return fmt.Errorf("error executing template: %v", err)
	}

	serializedEvent := SerialziedEvent{
		UpdateType: updateType,
		Job:        *job,
		HTML:       html.String(),
	}

	j, err := json.Marshal(serializedEvent)
	if err != nil {
		return fmt.Errorf("error marshalling event: %v", err)
	}

	stream.Broadcast(stream.Message{Type: updateType, Msg: string(j)})
	return nil
}