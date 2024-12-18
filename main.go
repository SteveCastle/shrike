package main

import (
	"encoding/json"
	"net/http"
	"text/template"

	"github.com/stevecastle/shrike/jobqueue"
	"github.com/stevecastle/shrike/runners"
	"github.com/stevecastle/shrike/stream"
)

type TemplateInput struct {
	Jobs []jobqueue.Job
}
var tmpl = template.Must(template.ParseGlob("client/templates/*.go.html"))

func homeHandler(queue *jobqueue.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := TemplateInput{Jobs: queue.GetJobs()}

		if err := tmpl.ExecuteTemplate(w, "home", data); err != nil {
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
		queue.AddJob(req.Input,"gallery-dl", []string{"-d", "X:/scrapes/"})
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
	http.HandleFunc("/stream", stream.StreamHandler)
	http.HandleFunc("/create", createJobHandler(queue))
	http.HandleFunc("/cancel", cancelHandler(queue))
	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("client/static"))))
	http.ListenAndServe(":8080", nil)
}

func readJSONBody(r *http.Request, v interface{}) error {
	body := r.Body
	defer body.Close()
	return json.NewDecoder(body).Decode(v)
}