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
	Command string
	Arguments    []string
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
		// Strip the last arg from the args array and assign it to the input
		queue.AddJob(c.Command, c.Arguments[:len(c.Arguments)-1], c.Arguments[len(c.Arguments)-1])
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

		args := strings.Fields(req.Input)
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

		queue.AddJob(command, args, input)
		w.WriteHeader(http.StatusOK)
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


func main() {
	queue := jobqueue.NewQueue()
	runners.New(queue, 2)

	http.HandleFunc("/", renderer.ApplyMiddlewares(homeHandler(queue)))
	http.HandleFunc("/job/{id}", renderer.ApplyMiddlewares(detailHandler(queue)))
	http.HandleFunc("/job/{id}/cancel", renderer.ApplyMiddlewares(cancelHandler(queue)))
	http.HandleFunc("/job/{id}/copy", renderer.ApplyMiddlewares(copyHandler(queue)))
	http.HandleFunc("/stream", stream.StreamHandler)
	http.HandleFunc("/create", renderer.ApplyMiddlewares(createJobHandler(queue)))
	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("client/static"))))
	http.ListenAndServe(":8090", nil)
}

func readJSONBody(r *http.Request, v interface{}) error {
	body := r.Body
	defer body.Close()
	return json.NewDecoder(body).Decode(v)
}

func ParseCommandLine(input string) (string, []string) {
	var args []string
	var current strings.Builder
	inQuotes := false

	for _, char := range input {
		switch char {
		case '"':
			inQuotes = !inQuotes // Toggle the inQuotes flag
		case ' ':
			if inQuotes {
				current.WriteRune(char) // Add space if inside quotes
			} else {
				if current.Len() > 0 {
					args = append(args, current.String())
					current.Reset()
				}
			}
		default:
			current.WriteRune(char)
		}
	}

	// Add the last token if it exists
	if current.Len() > 0 {
		args = append(args, current.String())
	}

	if len(args) == 0 {
		return "", nil // No command provided
	}

	command := args[0]       // First token is the command
	arguments := args[1:]    // Remaining tokens are arguments
	return command, arguments
}