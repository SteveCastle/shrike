package renderer

import (
	"embed"
	"html/template"
	"log"
	"net/http"
	"sync"
	"time"
)

var (
	templates *template.Template
	once      sync.Once
)

// --------------------------------------------------------------------
// Template embedding
// --------------------------------------------------------------------

//go:embed templates/*.go.html
var templatesFS embed.FS

const templateGlob = "templates/*.go.html"

// formatTime is a helper function that can be called from templates.
// Example usage in template: {{ formatTime .SomeTimeField }}
func formatTime(t time.Time) string {
	return t.Format("Jan 2, 2006 15:04:05")
}

// initTemplates initializes the templates. Called only once.
func initTemplates() *template.Template {
	tmpl, err := template.New("").
		Funcs(template.FuncMap{
			"formatTime": formatTime,
		}).
		ParseFS(templatesFS, templateGlob)
	if err != nil {
		log.Fatalf("Error parsing embedded templates: %v", err)
	}
	return tmpl
}

// Templates returns the singleton instance of the parsed templates.
func Templates() *template.Template {
	once.Do(func() { templates = initTemplates() })
	return templates
}

// --------------------------------------------------------------------
// Middleware helpers
// --------------------------------------------------------------------

func Logger(next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Println(time.Since(start), r.Method, r.URL.Path)
	}
}

func CORS(next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		enableCors(&w)
		if r.Method == http.MethodOptions {
			return
		}
		next.ServeHTTP(w, r)
	}
}

func ApplyMiddlewares(handler http.HandlerFunc) http.HandlerFunc {
	return Logger(CORS(handler))
}

func enableCors(w *http.ResponseWriter) {
	h := (*w).Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS, PUT, DELETE")
	h.Set("Access-Control-Allow-Headers", "Accept, Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization")
	h.Set("Access-Control-Allow-Credentials", "true")
	h.Set("Access-Control-Expose-Headers", "Content-Length")
}
