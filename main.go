package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/stevecastle/shrike/jobqueue"
	"github.com/stevecastle/shrike/renderer"
	"github.com/stevecastle/shrike/runners"
	"github.com/stevecastle/shrike/stream"
)

type ListTemplateData struct {
	Jobs []jobqueue.Job
}

type Command struct {
	Command   string
	Arguments []string
}

func homeHandler(queue *jobqueue.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		//  Handle posts for backwards compatibility
		if r.Method == http.MethodPost {
			var c Command
			err := json.NewDecoder(r.Body).Decode(&c)
			if err != nil {
				fmt.Println("Error decoding JSON: ", err)
				return
			}
			workflow := jobqueue.Workflow{
				Command:   c.Command,
				Arguments: c.Arguments[:len(c.Arguments)-1],
				Input:     c.Arguments[len(c.Arguments)-1],
			}
			queue.AddWorkflow(workflow)
		}
		// GET request
		data := ListTemplateData{Jobs: queue.GetJobs()}

		if err := renderer.Templates().ExecuteTemplate(w, "home", data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

type DetailTemplateData struct {
	Job *jobqueue.Job
}

func detailHandler(queue *jobqueue.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		job := queue.GetJob(id)

		// If job is nil, return 404
		if job == nil {
			http.Error(w, "Job not found", http.StatusNotFound)
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

func createJobHandler(queue *jobqueue.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Use POST", http.StatusMethodNotAllowed)
			return
		}
		body := r.Body
		defer body.Close()
		var req CreateJobHandlerRequest
		readJSONBody(r, &req)
		// Split the input string on spaces the first item is the command and the rest are arguments, the last is the input
		// only the command is guranteed to be present, args and input may be empty

		args := ParseCommand(req.Input)
		if len(args) == 0 {
			http.Error(w, "Invalid input", http.StatusBadRequest)
			return
		}
		command := args[0]
		var input string
		if len(args) > 1 {
			input = args[len(args)-1]
			args = args[1 : len(args)-1]
		} else {
			args = []string{}
		}
		workflow := jobqueue.Workflow{
			Command:   command,
			Arguments: args,
			Input:     input,
		}
		id, err := queue.AddWorkflow(workflow)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"id": id})
	}
}

type CancelHandlerRequest struct {
	ID string `json:"id"`
}

func cancelHandler(queue *jobqueue.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Use POST", http.StatusMethodNotAllowed)
			return
		}
		ID := r.PathValue("id")
		queue.CancelJob(ID)
		w.WriteHeader(http.StatusOK)
	}
}

func copyHandler(queue *jobqueue.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Use POST", http.StatusMethodNotAllowed)
			return
		}
		ID := r.PathValue("id")
		queue.CopyJob(ID)
		w.WriteHeader(http.StatusOK)
	}
}

func removeHandler(queue *jobqueue.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Use POST", http.StatusMethodNotAllowed)
			return
		}
		ID := r.PathValue("id")
		err := queue.RemoveJob(ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

func main() {
	queue := jobqueue.NewQueue()
	runners.New(queue, 2)

	http.HandleFunc("/", renderer.ApplyMiddlewares(homeHandler(queue)))
	http.HandleFunc("/job/{id}", renderer.ApplyMiddlewares(detailHandler(queue)))
	http.HandleFunc("/job/{id}/cancel", renderer.ApplyMiddlewares(cancelHandler(queue)))
	http.HandleFunc("/job/{id}/copy", renderer.ApplyMiddlewares(copyHandler(queue)))
	http.HandleFunc("/job/{id}/remove", renderer.ApplyMiddlewares(removeHandler(queue)))
	http.HandleFunc("/stream", stream.StreamHandler)
	http.HandleFunc("/create", renderer.ApplyMiddlewares(createJobHandler(queue)))
	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("client/static"))))
	err := http.ListenAndServe(":8090", nil)
	if err != nil {
		fmt.Println("Error starting server: ", err)
	}
}

func readJSONBody(r *http.Request, v interface{}) error {
	body := r.Body
	defer body.Close()
	return json.NewDecoder(body).Decode(v)
}

func ParseCommand(input string) []string {
	var result []string
	var current strings.Builder
	inQuotes := false

	// Iterate through each rune in the input string
	for i := 0; i < len(input); i++ {
		char := input[i]

		switch char {
		case '"':
			// Toggle quote state
			inQuotes = !inQuotes

		case ' ':
			if inQuotes {
				// If we're in quotes, space is part of the current argument
				current.WriteByte(char)
			} else if current.Len() > 0 {
				// If we're not in quotes and have accumulated characters,
				// add the current argument to result and reset
				result = append(result, current.String())
				current.Reset()
			}

		default:
			// Add character to current argument
			current.WriteByte(char)
		}
	}

	// Add the last argument if there's anything remaining
	if current.Len() > 0 {
		result = append(result, current.String())
	}

	return result
}
