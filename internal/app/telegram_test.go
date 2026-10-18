package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/shishberg/mezzaops/internal/webhook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeTelegramAPI records the text of every sendMessage call.
type fakeTelegramAPI struct {
	*httptest.Server
	mu    sync.Mutex
	texts []string
}

func newFakeTelegramAPI(t *testing.T) *fakeTelegramAPI {
	t.Helper()
	f := &fakeTelegramAPI{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			var body struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(raw, &body)
			f.mu.Lock()
			f.texts = append(f.texts, body.Text)
			f.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			_, _ = io.WriteString(w, `{"ok":true,"result":{"id":1,"is_bot":true,"username":"test_bot"}}`)
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			time.Sleep(20 * time.Millisecond)
			_, _ = io.WriteString(w, `{"ok":true,"result":[]}`)
		default:
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeTelegramAPI) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.texts...)
}

// writeTelegramConfig writes a config with a telegram section pointing at
// apiURL, a require_confirmation service, and a .env containing envContent.
func writeTelegramConfig(t *testing.T, dir, apiURL, envContent string) (cfgPath, envPath string) {
	t.Helper()
	svcDir := filepath.Join(dir, "services")
	require.NoError(t, os.MkdirAll(svcDir, 0o755))
	svcYAML := `branch: main
repo: org/confirmrepo
dir: /tmp
require_confirmation: true
deploy:
  - echo ok
`
	require.NoError(t, os.WriteFile(filepath.Join(svcDir, "confirmsvc.yaml"), []byte(svcYAML), 0o644))

	configYAML := fmt.Sprintf(`services_dir: %s
log_dir: %s
state_dir: %s
telegram:
  chat_id: -1001234567890
  api_url: %q
`, svcDir, filepath.Join(dir, "logs"), filepath.Join(dir, "state"), apiURL)
	cfgPath = filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(configYAML), 0o644))

	envPath = filepath.Join(dir, ".env")
	require.NoError(t, os.WriteFile(envPath, []byte(envContent), 0o644))
	return cfgPath, envPath
}

func telegramTemplates() fstest.MapFS {
	return fstest.MapFS{
		"index.html":   &fstest.MapFile{Data: []byte(minimalTemplate)},
		"service.html": &fstest.MapFile{Data: []byte(minimalServiceTemplate)},
	}
}

func TestNew_TelegramConfigured(t *testing.T) {
	api := newFakeTelegramAPI(t)
	dir := t.TempDir()
	cfgPath, envPath := writeTelegramConfig(t, dir, api.URL, "TELEGRAM_TOKEN=123:tok\n")

	a, err := app_New(cfgPath, envPath, telegramTemplates())
	require.NoError(t, err)
	defer a.manager.Stop()

	assert.NotNil(t, a.telegramBot)
}

func TestNew_TelegramWithoutTokenIsSkipped(t *testing.T) {
	api := newFakeTelegramAPI(t)
	dir := t.TempDir()
	cfgPath, envPath := writeTelegramConfig(t, dir, api.URL, "")

	a, err := app_New(cfgPath, envPath, telegramTemplates())
	require.NoError(t, err)
	defer a.manager.Stop()

	assert.Nil(t, a.telegramBot)
}

func TestNew_TelegramTokenWithoutSectionIsSkipped(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeMinimalConfig(t, dir)
	envPath := filepath.Join(dir, ".env")
	require.NoError(t, os.WriteFile(envPath, []byte("TELEGRAM_TOKEN=123:tok\n"), 0o644))

	a, err := app_New(cfgPath, envPath, telegramTemplates())
	require.NoError(t, err)
	defer a.manager.Stop()

	assert.Nil(t, a.telegramBot)
}

func TestHandlePush_RequireConfirmation_PromptsTelegram(t *testing.T) {
	api := newFakeTelegramAPI(t)
	dir := t.TempDir()
	cfgPath, envPath := writeTelegramConfig(t, dir, api.URL, "TELEGRAM_TOKEN=123:tok\n")

	a, err := app_New(cfgPath, envPath, telegramTemplates())
	require.NoError(t, err)
	defer a.manager.Stop()
	require.NotNil(t, a.telegramBot)

	// The sender goroutine starts with Run; HandlePush only queues.
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { defer close(runDone); _ = a.Run(ctx) }()
	defer func() { cancel(); <-runDone }()

	a.HandlePush(webhook.PushEvent{Repo: "org/confirmrepo", Branch: "main"})

	assert.True(t, a.confirmations.IsPending("confirmsvc"))
	require.Eventually(t, func() bool {
		for _, text := range api.sent() {
			if strings.Contains(text, "Deploy queued for <b>confirmsvc</b> (repo: org/confirmrepo, branch: main). "+
				"Reply <code>/confirm confirmsvc</code> to proceed.") {
				return true
			}
		}
		return false
	}, 5*time.Second, 5*time.Millisecond, "sent: %v", api.sent())
}

func TestNew_TelegramWithoutChatIDIsDisabledWithWarning(t *testing.T) {
	api := newFakeTelegramAPI(t)
	dir := t.TempDir()
	cfgPath, envPath := writeTelegramConfig(t, dir, api.URL, "TELEGRAM_TOKEN=123:tok\n")
	data, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	// A misspelled key leaves chat_id at zero.
	require.NoError(t, os.WriteFile(cfgPath, []byte(strings.ReplaceAll(string(data), "chat_id:", "chat_idd:")), 0o644))

	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)

	a, err := app_New(cfgPath, envPath, telegramTemplates())
	require.NoError(t, err, "one bad section must not stop the supervisor booting")
	defer a.manager.Stop()

	assert.Nil(t, a.telegramBot)
	assert.Contains(t, buf.String(), "telegram")
	assert.Contains(t, buf.String(), "chat_id")
}
