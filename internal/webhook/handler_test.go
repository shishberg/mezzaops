package webhook_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/shishberg/mezzaops/internal/webhook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func signPayload(secret string, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

type mockDeployTrigger struct {
	called     bool
	calledRepo string
	calledRef  string
}

func (m *mockDeployTrigger) HandlePush(repo string, branch string) {
	m.called = true
	m.calledRepo = repo
	m.calledRef = branch
}

func TestWebhook_ValidPush(t *testing.T) {
	const secret = "test-secret"
	trigger := &mockDeployTrigger{}
	handler := webhook.NewHandler(secret, trigger)

	payload := map[string]interface{}{
		"ref": "refs/heads/main",
		"repository": map[string]interface{}{
			"full_name": "acme/myapp",
		},
	}
	body, err := json.Marshal(payload)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/webhook/github", strings.NewReader(string(body)))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", signPayload(secret, body))

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	assert.True(t, trigger.called)
	assert.Equal(t, "acme/myapp", trigger.calledRepo)
	assert.Equal(t, "main", trigger.calledRef)
}

func TestWebhook_InvalidSignature(t *testing.T) {
	const secret = "test-secret"
	trigger := &mockDeployTrigger{}
	handler := webhook.NewHandler(secret, trigger)

	payload := map[string]interface{}{
		"ref": "refs/heads/main",
		"repository": map[string]interface{}{
			"full_name": "acme/myapp",
		},
	}
	body, err := json.Marshal(payload)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/webhook/github", strings.NewReader(string(body)))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", "sha256=invalidsignature")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusForbidden, rr.Code)
	assert.False(t, trigger.called)
}

func TestWebhook_NonPushEvent(t *testing.T) {
	const secret = "test-secret"
	trigger := &mockDeployTrigger{}
	handler := webhook.NewHandler(secret, trigger)

	payload := map[string]interface{}{
		"action": "opened",
		"pull_request": map[string]interface{}{
			"number": 42,
		},
	}
	body, err := json.Marshal(payload)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/webhook/github", strings.NewReader(string(body)))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", signPayload(secret, body))

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	assert.False(t, trigger.called)
}

func TestWebhook_TagPushIgnored(t *testing.T) {
	const secret = "test-secret"
	trigger := &mockDeployTrigger{}
	handler := webhook.NewHandler(secret, trigger)

	payload := map[string]interface{}{
		"ref": "refs/tags/v1.0.0",
		"repository": map[string]interface{}{
			"full_name": "acme/myapp",
		},
	}
	body, err := json.Marshal(payload)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/webhook/github", strings.NewReader(string(body)))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", signPayload(secret, body))

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	assert.False(t, trigger.called)
}

func TestWebhook_MissingSignature(t *testing.T) {
	const secret = "test-secret"
	trigger := &mockDeployTrigger{}
	handler := webhook.NewHandler(secret, trigger)

	payload := map[string]interface{}{
		"ref": "refs/heads/main",
		"repository": map[string]interface{}{
			"full_name": "acme/myapp",
		},
	}
	body, err := json.Marshal(payload)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/webhook/github", strings.NewReader(string(body)))
	req.Header.Set("X-GitHub-Event", "push")
	// No X-Hub-Signature-256 header set

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusForbidden, rr.Code)
	assert.False(t, trigger.called)
}

func TestWebhook_FormURLEncoded(t *testing.T) {
	const secret = "test-secret"
	trigger := &mockDeployTrigger{}
	handler := webhook.NewHandler(secret, trigger)

	payload := map[string]interface{}{
		"ref": "refs/heads/main",
		"repository": map[string]interface{}{
			"full_name": "acme/myapp",
		},
	}
	jsonBody, err := json.Marshal(payload)
	require.NoError(t, err)

	formBody := "payload=" + url.QueryEscape(string(jsonBody))

	req := httptest.NewRequest(http.MethodPost, "/webhook/github", strings.NewReader(formBody))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", signPayload(secret, []byte(formBody)))

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	assert.True(t, trigger.called)
	assert.Equal(t, "acme/myapp", trigger.calledRepo)
	assert.Equal(t, "main", trigger.calledRef)
}

func TestWebhook_FormURLEncoded_WithCharset(t *testing.T) {
	const secret = "test-secret"
	trigger := &mockDeployTrigger{}
	handler := webhook.NewHandler(secret, trigger)

	payload := map[string]interface{}{
		"ref": "refs/heads/main",
		"repository": map[string]interface{}{
			"full_name": "acme/myapp",
		},
	}
	jsonBody, err := json.Marshal(payload)
	require.NoError(t, err)

	formBody := "payload=" + url.QueryEscape(string(jsonBody))

	req := httptest.NewRequest(http.MethodPost, "/webhook/github", strings.NewReader(formBody))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", signPayload(secret, []byte(formBody)))

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	assert.True(t, trigger.called)
	assert.Equal(t, "acme/myapp", trigger.calledRepo)
	assert.Equal(t, "main", trigger.calledRef)
}

func TestWebhook_FormURLEncoded_MissingPayloadField(t *testing.T) {
	const secret = "test-secret"
	trigger := &mockDeployTrigger{}
	handler := webhook.NewHandler(secret, trigger)

	formBody := "other=value"

	req := httptest.NewRequest(http.MethodPost, "/webhook/github", strings.NewReader(formBody))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", signPayload(secret, []byte(formBody)))

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.False(t, trigger.called)
}

func TestWebhook_ExtractsRepoBranch(t *testing.T) {
	const secret = "deploy-key"
	trigger := &mockDeployTrigger{}
	handler := webhook.NewHandler(secret, trigger)

	payload := map[string]interface{}{
		"ref": "refs/heads/feature/deploy-v2",
		"repository": map[string]interface{}{
			"full_name": "org/backend-api",
		},
	}
	body, err := json.Marshal(payload)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/webhook/github", strings.NewReader(string(body)))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", signPayload(secret, body))

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	assert.True(t, trigger.called)
	assert.Equal(t, "org/backend-api", trigger.calledRepo)
	assert.Equal(t, "feature/deploy-v2", trigger.calledRef)
}
