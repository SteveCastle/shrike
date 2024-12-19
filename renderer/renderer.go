package renderer

import (
	"html/template"
	"log"
	"sync"
	"time"
)

var (
	templates     *template.Template
	once          sync.Once
	templatePaths = "client/templates/*.go.html"
)

// formatTime is a helper function that can be called from templates.
// Example usage in template: {{ formatTime .SomeTimeField }}
func formatTime(t time.Time) string {
	return t.Format("Jan 2, 2006 15:04:05")
}

// initTemplates initializes the templates. Called only once.
func initTemplates() *template.Template {
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"formatTime": formatTime,
	}).ParseGlob(templatePaths)
	if err != nil {
		log.Fatalf("Error parsing templates: %v", err)
	}
	return tmpl
}

// Templates returns the singleton instance of the parsed templates.
func Templates() *template.Template {
	once.Do(func() {
		templates = initTemplates()
	})
	return templates
}
