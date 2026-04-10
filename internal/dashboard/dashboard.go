package dashboard

import (
	"bytes"
	"encoding/json"
	"html/template"
	"io/fs"
	"net/http"

	"github.com/shishberg/mezzaops/internal/config"
	"github.com/shishberg/mezzaops/internal/service"
)

// StateProvider returns the current state of all services.
type StateProvider interface {
	GetAllStates() map[string]service.ServiceState
	GetServiceState(name string) (service.ServiceState, bool)
	GetServiceLogs(name string) string
	GetServiceConfig(name string) (config.ServiceConfig, bool)
}

// serviceDetailData is the template data for the service detail page.
type serviceDetailData struct {
	Name   string
	State  service.ServiceState
	Config config.ServiceConfig
	Logs   string
}

// Dashboard serves the web UI and JSON API for service status.
type Dashboard struct {
	provider StateProvider
	tmpl     *template.Template
	mux      *http.ServeMux
}

// New creates a Dashboard, parsing templates from the given filesystem.
func New(provider StateProvider, templatesFS fs.FS) (*Dashboard, error) {
	tmpl, err := template.ParseFS(templatesFS, "index.html", "service.html")
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
	d.mux.HandleFunc("GET /service/{name}", d.handleServiceDetail)
	d.mux.HandleFunc("GET /api/service/{name}/logs", d.handleAPIServiceLogs)

	return d, nil
}

// ServeHTTP dispatches requests to the registered routes.
func (d *Dashboard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mux.ServeHTTP(w, r)
}

func (d *Dashboard) handleIndex(w http.ResponseWriter, _ *http.Request) {
	states := d.provider.GetAllStates()
	var buf bytes.Buffer
	if err := d.tmpl.ExecuteTemplate(&buf, "index.html", states); err != nil {
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

func (d *Dashboard) handleServiceDetail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	state, ok := d.provider.GetServiceState(name)
	if !ok {
		http.Error(w, "service not found", http.StatusNotFound)
		return
	}

	cfg, _ := d.provider.GetServiceConfig(name)
	logs := d.provider.GetServiceLogs(name)

	data := serviceDetailData{
		Name:   name,
		State:  state,
		Config: cfg,
		Logs:   logs,
	}

	var buf bytes.Buffer
	if err := d.tmpl.ExecuteTemplate(&buf, "service.html", data); err != nil {
		http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

func (d *Dashboard) handleAPIServiceLogs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	_, ok := d.provider.GetServiceState(name)
	if !ok {
		http.Error(w, "service not found", http.StatusNotFound)
		return
	}

	logs := d.provider.GetServiceLogs(name)
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]string{"logs": logs}); err != nil {
		http.Error(w, "json error: "+err.Error(), http.StatusInternalServerError)
	}
}
