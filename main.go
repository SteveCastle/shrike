package main

import (
	"encoding/json"
	"fmt"
	"log"
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
		enableCors(&w)
		log.Printf("Request: %s %s\n", r.RemoteAddr, r.Host)

		if (*r).Method == "OPTIONS" {
			return
		}
		//  Handle posts for backwards compatibility
		if r.Method == http.MethodPost {
		var c Command
		err := json.NewDecoder(r.Body).Decode(&c)
		if err != nil {
			fmt.Println("Error decoding JSON: ", err)
			return
		}
		// Strip the last arg from the args array and assign it to the input
		queue.AddJob(c.Arguments[len(c.Arguments)-1], c.Command, c.Arguments[:len(c.Arguments)-1])
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
		enableCors(&w)

		if (*r).Method == "OPTIONS" {
			return
		}

		id := r.PathValue("id")
		job := queue.GetJob(id)
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
		args := strings.Split(req.Input, " ")
		input := args[len(args)-1]
		args = args[:len(args)-1]
		queue.AddJob(input, args[0], args[1:])
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
		// Parse from json string in body
		body := r.Body
		defer body.Close()
		var req CancelHandlerRequest
		readJSONBody(r, &req)

		queue.CancelJob(req.ID)
		w.WriteHeader(http.StatusOK)
	}
}


func main() {
	queue := jobqueue.NewQueue()
	runners.New(queue, 2)

	http.HandleFunc("/", homeHandler(queue))
	http.HandleFunc("/job/{id}", detailHandler(queue))
	http.HandleFunc("/stream", stream.StreamHandler)
	http.HandleFunc("/create", createJobHandler(queue))
	http.HandleFunc("/cancel", cancelHandler(queue))
	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("client/static"))))
	http.ListenAndServe(":8090", nil)
}

func readJSONBody(r *http.Request, v interface{}) error {
	body := r.Body
	defer body.Close()
	return json.NewDecoder(body).Decode(v)
}

func enableCors(w *http.ResponseWriter) {
	(*w).Header().Set("Access-Control-Allow-Origin", "*")
	(*w).Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS, PUT, DELETE")
	(*w).Header().Set("Access-Control-Allow-Headers", "Accept, Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization")
	(*w).Header().Set("Access-Control-Allow-Credentials", "true")
	(*w).Header().Set("Access-Control-Expose-Headers", "Content-Length")
	(*w).Header().Set("Access-Control-Allow-Headers", "Content-Type")
}
