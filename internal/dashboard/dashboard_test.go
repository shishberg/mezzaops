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
