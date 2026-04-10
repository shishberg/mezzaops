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

// PushEvent is the relevant subset of a GitHub push webhook payload.
type PushEvent struct {
	Repo       string // e.g. "org/repo"
	Branch     string // e.g. "main"
	Compare    string // URL to the GitHub compare page
	Pusher     string // username of the pusher
	HeadCommit HeadCommit
}

// HeadCommit captures the head commit details from a push webhook.
type HeadCommit struct {
	ID        string // full SHA
	Message   string // commit message (may be multiline)
	URL       string // commit URL
	Author    string // author name
	Timestamp string // timestamp as received (keep as string for simplicity)
}

// DeployTrigger is called when a valid push event is received.
type DeployTrigger interface {
	HandlePush(event PushEvent)
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

	eventType := r.Header.Get("X-GitHub-Event")
	if eventType != "push" {
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
		Compare    string `json:"compare"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Pusher struct {
			Name string `json:"name"`
		} `json:"pusher"`
		HeadCommit *struct {
			ID        string `json:"id"`
			Message   string `json:"message"`
			URL       string `json:"url"`
			Timestamp string `json:"timestamp"`
			Author    struct {
				Name string `json:"name"`
			} `json:"author"`
		} `json:"head_commit"`
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
	event := PushEvent{
		Repo:    payload.Repository.FullName,
		Branch:  branch,
		Compare: payload.Compare,
		Pusher:  payload.Pusher.Name,
	}
	if payload.HeadCommit != nil {
		event.HeadCommit = HeadCommit{
			ID:        payload.HeadCommit.ID,
			Message:   payload.HeadCommit.Message,
			URL:       payload.HeadCommit.URL,
			Author:    payload.HeadCommit.Author.Name,
			Timestamp: payload.HeadCommit.Timestamp,
		}
	}
	h.trigger.HandlePush(event)
	w.WriteHeader(http.StatusOK)
}

// verifySignature checks that the X-Hub-Signature-256 header matches the HMAC of the body.
func (h *Handler) verifySignature(signature string, body []byte) bool {
	mac := hmac.New(sha256.New, []byte(h.secret))
	mac.Write(body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(signature), []byte(expected))
}
