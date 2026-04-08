package dashboard

import (
	"bytes"
	"encoding/json"
	"html/template"
	"io/fs"
	"net/http"

	"github.com/shishberg/mezzaops/internal/service"
)

// StateProvider returns the current state of all services.
type StateProvider interface {
	GetAllStates() map[string]service.ServiceState
}

// Dashboard serves the web UI and JSON API for service status.
type Dashboard struct {
	provider StateProvider
	tmpl     *template.Template
	mux      *http.ServeMux
}

// New creates a Dashboard, parsing the index.html template from the given filesystem.
func New(provider StateProvider, templatesFS fs.FS) (*Dashboard, error) {
	tmpl, err := template.ParseFS(templatesFS, "index.html")
	if err != nil {
		return nil, err
	}

	d := &Dashboard{
		provider: provider,
		tmpl:     tmpl,
		mux:      http.NewServeMux(),
	}

	d.mux.HandleFunc("GET /", d.handleIndex)
	d.mux.HandleFunc("GET /api/status", d.handleAPIStatus)

	return d, nil
}

// ServeHTTP dispatches requests to the registered routes.
func (d *Dashboard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mux.ServeHTTP(w, r)
}

func (d *Dashboard) handleIndex(w http.ResponseWriter, _ *http.Request) {
	states := d.provider.GetAllStates()
	var buf bytes.Buffer
	if err := d.tmpl.Execute(&buf, states); err != nil {
		http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

func (d *Dashboard) handleAPIStatus(w http.ResponseWriter, _ *http.Request) {
	states := d.provider.GetAllStates()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(states); err != nil {
		http.Error(w, "json error: "+err.Error(), http.StatusInternalServerError)
	}
}
