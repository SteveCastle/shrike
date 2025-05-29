//go:build windows
// +build windows

package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kardianos/service"

	"github.com/stevecastle/shrike/jobqueue"
	"github.com/stevecastle/shrike/renderer"
	"github.com/stevecastle/shrike/runners"
	"github.com/stevecastle/shrike/stream"
)

/* -----------------------------------------------------------------
 * Utility – run from the folder that contains the executable so the
 *           templates and static files are found when the service
 *           starts from C:\Windows\System32.
 * ----------------------------------------------------------------- */
func init() {
	exe, err := os.Executable()
	if err == nil {
		_ = os.Chdir(filepath.Dir(exe))
	}
}

/* -----------------------------------------------------------------
 * Web-handler helpers (unchanged from your original file)
 * ----------------------------------------------------------------- */

type ListTemplateData struct{ Jobs []jobqueue.Job }
type DetailTemplateData struct{ Job *jobqueue.Job }

type Command struct {
	Command   string
	Arguments []string
}

func homeHandler(queue *jobqueue.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		/* POST = legacy JSON workflow launch ------------------------- */
		if r.Method == http.MethodPost {
			var c Command
			if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
				http.Error(w, "bad json", http.StatusBadRequest)
				return
			}
			workflow := jobqueue.Workflow{
				Command:   c.Command,
				Arguments: c.Arguments[:len(c.Arguments)-1],
				Input:     c.Arguments[len(c.Arguments)-1],
			}
			queue.AddWorkflow(workflow)
		}

		/* GET – render job list ------------------------------------- */
		data := ListTemplateData{Jobs: queue.GetJobs()}
		if err := renderer.Templates().ExecuteTemplate(w, "home", data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

func detailHandler(queue *jobqueue.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		job := queue.GetJob(id)
		if job == nil {
			http.NotFound(w, r)
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

		var req CreateJobHandlerRequest
		if err := readJSONBody(r, &req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}

		args := ParseCommand(req.Input)
		if len(args) == 0 {
			http.Error(w, "Invalid input", http.StatusBadRequest)
			return
		}

		cmd, input := args[0], ""
		if len(args) > 1 {
			input = args[len(args)-1]
			args = args[1 : len(args)-1]
		} else {
			args = nil
		}

		id, err := queue.AddWorkflow(jobqueue.Workflow{
			Command:   cmd,
			Arguments: args,
			Input:     input,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
	}
}

func cancelHandler(queue *jobqueue.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Use POST", http.StatusMethodNotAllowed)
			return
		}
		queue.CancelJob(r.PathValue("id"))
	}
}

func copyHandler(queue *jobqueue.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Use POST", http.StatusMethodNotAllowed)
			return
		}
		queue.CopyJob(r.PathValue("id"))
	}
}

func removeHandler(queue *jobqueue.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Use POST", http.StatusMethodNotAllowed)
			return
		}
		if err := queue.RemoveJob(r.PathValue("id")); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

func readJSONBody(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

func ParseCommand(input string) []string {
	var (
		result   []string
		current  strings.Builder
		inQuotes bool
	)
	for i := 0; i < len(input); i++ {
		c := input[i]
		switch c {
		case '"':
			inQuotes = !inQuotes
		case ' ':
			if inQuotes {
				current.WriteByte(c)
			} else if current.Len() > 0 {
				result = append(result, current.String())
				current.Reset()
			}
		default:
			current.WriteByte(c)
		}
	}
	if current.Len() > 0 {
		result = append(result, current.String())
	}
	return result
}

/* -----------------------------------------------------------------
 * Windows-service integration
 * ----------------------------------------------------------------- */

type program struct {
	server *http.Server
}

func (p *program) Start(s service.Service) error {
	// ––––– set up queue, runners and handlers –––––
	queue := jobqueue.NewQueue()
	runners.New(queue, 2)

	mux := http.NewServeMux()
	mux.HandleFunc("/", renderer.ApplyMiddlewares(homeHandler(queue)))
	mux.HandleFunc("/job/{id}", renderer.ApplyMiddlewares(detailHandler(queue)))
	mux.HandleFunc("/job/{id}/cancel", renderer.ApplyMiddlewares(cancelHandler(queue)))
	mux.HandleFunc("/job/{id}/copy", renderer.ApplyMiddlewares(copyHandler(queue)))
	mux.HandleFunc("/job/{id}/remove", renderer.ApplyMiddlewares(removeHandler(queue)))
	mux.HandleFunc("/stream", stream.StreamHandler)
	mux.HandleFunc("/create", renderer.ApplyMiddlewares(createJobHandler(queue)))
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("client/static"))))

	p.server = &http.Server{
		Addr:    ":8090",
		Handler: mux,
	}

	// run the server in a goroutine so Start can return
	go func() {
		if err := p.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("shrike-server: %v", err)
		}
	}()

	return nil
}

func (p *program) Stop(s service.Service) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return p.server.Shutdown(ctx)
}

/* -----------------------------------------------------------------
 * main – console + service entry-point
 * ----------------------------------------------------------------- */

func main() {
	cfg := &service.Config{
		Name:        "ShrikeServer",
		DisplayName: "Shrike Job Server",
		Description: "Background service hosting the Shrike job queue web UI",
	}

	prg := &program{}
	svc, err := service.New(prg, cfg)
	if err != nil {
		log.Fatalf("cannot create service: %v", err)
	}

	// Support "install", "uninstall", "start", "stop", "restart", "status"
	if len(os.Args) > 1 {
		if err := service.Control(svc, os.Args[1]); err != nil {
			log.Fatalf("service control failed: %v", err)
		}
		return
	}

	/* ---------- Interactive console (go run / debugging) ---------- */
	if service.Interactive() {
		if err := prg.Start(svc); err != nil {
			log.Fatalf("start: %v", err)
		}
		// Wait for Ctrl-C or SIGTERM then shut down cleanly
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		_ = prg.Stop(svc)
		return
	}

	/* -------------------- Real Windows service -------------------- */
	if err := svc.Run(); err != nil {
		log.Fatalf("run: %v", err)
	}
}
