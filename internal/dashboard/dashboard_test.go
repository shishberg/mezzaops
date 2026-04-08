package dashboard_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/shishberg/mezzaops/internal/dashboard"
	"github.com/shishberg/mezzaops/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockStateProvider struct {
	states map[string]service.ServiceState
}

func (m *mockStateProvider) GetAllStates() map[string]service.ServiceState {
	return m.states
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
