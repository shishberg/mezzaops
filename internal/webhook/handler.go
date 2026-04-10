package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// DeployTrigger is called when a valid push event is received.
type DeployTrigger interface {
	HandlePush(repo string, branch string)
}

// Handler handles incoming GitHub webhook requests.
type Handler struct {
	secret  string
	trigger DeployTrigger
}

// NewHandler creates a new webhook Handler with the given HMAC secret and deploy trigger.
func NewHandler(secret string, trigger DeployTrigger) *Handler {
	return &Handler{secret: secret, trigger: trigger}
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1 MB limit
	if err != nil {
		http.Error(w, "failed to read body", http.StatusInternalServerError)
		return
	}

	sig := r.Header.Get("X-Hub-Signature-256")
	if !h.verifySignature(sig, body) {
		http.Error(w, "invalid signature", http.StatusForbidden)
		return
	}

	event := r.Header.Get("X-GitHub-Event")
	if event != "push" {
		w.WriteHeader(http.StatusOK)
		return
	}

	jsonBody := body
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		form, err := url.ParseQuery(string(body))
		if err != nil {
			http.Error(w, "malformed form body", http.StatusBadRequest)
			return
		}
		if form.Get("payload") == "" {
			http.Error(w, "missing payload field", http.StatusBadRequest)
			return
		}
		jsonBody = []byte(form.Get("payload"))
	}

	var payload struct {
		Ref        string `json:"ref"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(jsonBody, &payload); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	// Ignore tag pushes
	if strings.HasPrefix(payload.Ref, "refs/tags/") {
		w.WriteHeader(http.StatusOK)
		return
	}

	branch := strings.TrimPrefix(payload.Ref, "refs/heads/")
	h.trigger.HandlePush(payload.Repository.FullName, branch)
	w.WriteHeader(http.StatusOK)
}

// verifySignature checks that the X-Hub-Signature-256 header matches the HMAC of the body.
func (h *Handler) verifySignature(signature string, body []byte) bool {
	mac := hmac.New(sha256.New, []byte(h.secret))
	mac.Write(body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(signature), []byte(expected))
}
