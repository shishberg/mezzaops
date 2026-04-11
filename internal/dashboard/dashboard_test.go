package dashboard_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/shishberg/mezzaops/internal/config"
	"github.com/shishberg/mezzaops/internal/dashboard"
	"github.com/shishberg/mezzaops/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockStateProvider struct {
	states  map[string]service.ServiceState
	configs map[string]config.ServiceConfig
	logs    map[string]string
}

func (m *mockStateProvider) GetAllStates() map[string]service.ServiceState {
	return m.states
}

func (m *mockStateProvider) GetServiceState(name string) (service.ServiceState, bool) {
	s, ok := m.states[name]
	return s, ok
}

func (m *mockStateProvider) GetServiceLogs(name string) string {
	if m.logs == nil {
		return ""
	}
	return m.logs[name]
}

func (m *mockStateProvider) GetServiceConfig(name string) (config.ServiceConfig, bool) {
	if m.configs == nil {
		return config.ServiceConfig{}, false
	}
	c, ok := m.configs[name]
	return c, ok
}

func TestDashboard_RendersServices(t *testing.T) {
	provider := &mockStateProvider{
		states: map[string]service.ServiceState{
			"myapp": {
				Status:     "running",
				LastDeploy: time.Date(2026, 4, 6, 12, 0, 0, 0, time.UTC),
				LastResult: "success",
				LastOutput: "$ echo deployed\ndeployed\n",
			},
			"api": {
				Status:     "failed",
				LastDeploy: time.Date(2026, 4, 6, 11, 0, 0, 0, time.UTC),
				LastResult: "failed",
				LastOutput: "$ make test\nFAIL\n",
				FailedStep: "make test",
			},
		},
	}

	d, err := dashboard.New(provider, os.DirFS("../../templates"))
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	d.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.Contains(t, body, "myapp")
	assert.Contains(t, body, "running")
	assert.Contains(t, body, "api")
	assert.Contains(t, body, "failed")
}

func TestDashboard_JSONEndpoint(t *testing.T) {
	provider := &mockStateProvider{
		states: map[string]service.ServiceState{
			"myapp": {Status: "running"},
		},
	}

	d, err := dashboard.New(provider, os.DirFS("../../templates"))
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rr := httptest.NewRecorder()
	d.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Header().Get("Content-Type"), "application/json")

	var result map[string]service.ServiceState
	err = json.Unmarshal(rr.Body.Bytes(), &result)
	require.NoError(t, err)
	assert.Contains(t, result, "myapp")
	assert.Equal(t, "running", result["myapp"].Status)
}

func TestDashboard_StatusBadges(t *testing.T) {
	provider := &mockStateProvider{
		states: map[string]service.ServiceState{
			"web": {Status: "running"},
			"db":  {Status: "stopped"},
		},
	}

	d, err := dashboard.New(provider, os.DirFS("../../templates"))
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	d.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.Contains(t, body, "badge-running")
	assert.Contains(t, body, "badge-stopped")
}

func TestDashboard_ServiceDetail(t *testing.T) {
	provider := &mockStateProvider{
		states: map[string]service.ServiceState{
			"myapp": {
				Status:     "running",
				LastDeploy: time.Date(2026, 4, 6, 12, 0, 0, 0, time.UTC),
				LastResult: "success",
				LastOutput: "$ echo deployed\ndeployed\n",
			},
		},
		configs: map[string]config.ServiceConfig{
			"myapp": {
				Name:   "myapp",
				Dir:    "/opt/myapp",
				Branch: "main",
				Repo:   "github.com/org/myapp",
			},
		},
		logs: map[string]string{
			"myapp": "2026-04-06 server started on :8080\n",
		},
	}

	d, err := dashboard.New(provider, os.DirFS("../../templates"))
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/service/myapp", nil)
	rr := httptest.NewRecorder()
	d.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.Contains(t, body, "myapp")
	assert.Contains(t, body, "badge-running")
	assert.Contains(t, body, "server started on :8080")
	assert.Contains(t, body, "All services")
	assert.Contains(t, body, "branch")
	assert.Contains(t, body, "main")
	assert.Contains(t, body, "repo")
	assert.Contains(t, body, "github.com/org/myapp")
	assert.Contains(t, body, "dir")
	assert.Contains(t, body, "/opt/myapp")
}

func TestConfigFields(t *testing.T) {
	cfg := config.ServiceConfig{
		Name:       "myapp",
		Dir:        "/opt/myapp",
		Branch:     "main",
		Repo:       "github.com/org/myapp",
		Entrypoint: []string{"go", "run", "."},
		SelfDeploy: true,
	}

	fields := dashboard.ConfigFields(cfg)

	// Build a map for easy lookup.
	m := make(map[string]string)
	for _, f := range fields {
		m[f.Key] = f.Value
	}

	// Present fields.
	assert.Equal(t, "main", m["branch"])
	assert.Equal(t, "github.com/org/myapp", m["repo"])
	assert.Equal(t, "/opt/myapp", m["dir"])
	assert.Equal(t, "go, run, .", m["entrypoint"])
	assert.Equal(t, "true", m["self_deploy"])

	// Zero-value fields must not be included.
	for _, f := range fields {
		assert.NotEqual(t, "service_name", f.Key, "zero-value service_name should be skipped")
		assert.NotEqual(t, "deploy", f.Key, "zero-value deploy should be skipped")
		assert.NotEqual(t, "user_service", f.Key, "zero-value user_service should be skipped")
		assert.NotEqual(t, "sudo", f.Key, "zero-value sudo should be skipped")
		assert.NotEqual(t, "require_confirmation", f.Key, "zero-value require_confirmation should be skipped")
	}

	// Verify declaration order: branch, repo, dir come first.
	keys := make([]string, len(fields))
	for i, f := range fields {
		keys[i] = f.Key
	}
	assert.Equal(t, "branch", keys[0])
	assert.Equal(t, "repo", keys[1])
	assert.Equal(t, "dir", keys[2])
	assert.Equal(t, "entrypoint", keys[3])
	assert.Equal(t, "self_deploy", keys[4])
}

func TestConfigFields_Empty(t *testing.T) {
	cfg := config.ServiceConfig{}
	fields := dashboard.ConfigFields(cfg)
	assert.Empty(t, fields)
}

func TestDashboard_ServiceDetail_NotFound(t *testing.T) {
	provider := &mockStateProvider{
		states: map[string]service.ServiceState{},
	}

	d, err := dashboard.New(provider, os.DirFS("../../templates"))
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/service/nonexistent", nil)
	rr := httptest.NewRecorder()
	d.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusNotFound, rr.Code)
}

func TestDashboard_ServiceLogsAPI(t *testing.T) {
	provider := &mockStateProvider{
		states: map[string]service.ServiceState{
			"myapp": {Status: "running"},
		},
		logs: map[string]string{
			"myapp": "log line 1\nlog line 2\n",
		},
	}

	d, err := dashboard.New(provider, os.DirFS("../../templates"))
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/api/service/myapp/logs", nil)
	rr := httptest.NewRecorder()
	d.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Header().Get("Content-Type"), "application/json")

	var result map[string]string
	err = json.Unmarshal(rr.Body.Bytes(), &result)
	require.NoError(t, err)
	assert.Contains(t, result["logs"], "log line 1")
}

func TestDashboard_ServiceNameLinks(t *testing.T) {
	provider := &mockStateProvider{
		states: map[string]service.ServiceState{
			"myapp": {Status: "running"},
		},
	}

	d, err := dashboard.New(provider, os.DirFS("../../templates"))
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	d.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.Contains(t, body, `href="/service/myapp"`)
}

// assertAutoReloadToggle verifies that a rendered dashboard page has an
// auto-reload toggle that is off by default, persists via localStorage, and
// does not fire location.reload() unconditionally on page load.
func assertAutoReloadToggle(t *testing.T, body string) {
	t.Helper()

	// Toggle button must be present with a stable id for the script.
	assert.Contains(t, body, `id="autoreload-toggle"`,
		"page must include the auto-reload toggle button")

	// The initial rendered label must reflect the "off" default — users
	// should see "Off" on first load, before any JS runs.
	assert.Contains(t, body, "Auto-reload: Off",
		"toggle must render as Off by default")

	// Persistence: the script must read/write localStorage so the setting
	// survives a reload (required so that turning the toggle on stays on
	// across the auto-reload it triggers).
	assert.Contains(t, body, "localStorage",
		"auto-reload state must be persisted in localStorage")

	// The old unconditional reload must be gone. Any reload must be gated
	// on the toggle state; we assert the raw unconditional call is absent.
	assert.NotContains(t, body, "setTimeout(function() { location.reload(); }, 10000)",
		"page must not unconditionally schedule a reload")
}

func TestDashboard_IndexAutoReloadToggle(t *testing.T) {
	provider := &mockStateProvider{
		states: map[string]service.ServiceState{
			"myapp": {Status: "running"},
		},
	}

	d, err := dashboard.New(provider, os.DirFS("../../templates"))
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	d.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	assertAutoReloadToggle(t, rr.Body.String())
}

func TestDashboard_ServiceDetailAutoReloadToggle(t *testing.T) {
	provider := &mockStateProvider{
		states: map[string]service.ServiceState{
			"myapp": {Status: "running"},
		},
		configs: map[string]config.ServiceConfig{
			"myapp": {Name: "myapp", Dir: "/opt/myapp"},
		},
	}

	d, err := dashboard.New(provider, os.DirFS("../../templates"))
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/service/myapp", nil)
	rr := httptest.NewRecorder()
	d.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	assertAutoReloadToggle(t, rr.Body.String())
}
