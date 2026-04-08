package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const minimalTemplate = `<!DOCTYPE html><html><body>{{range $name, $state := .}}{{$name}}{{end}}</body></html>`

func writeTestConfig(t *testing.T, dir string) string {
	t.Helper()

	// Create services directory with one service
	svcDir := filepath.Join(dir, "services")
	require.NoError(t, os.MkdirAll(svcDir, 0o755))

	svcYAML := `branch: main
repo: org/testrepo
dir: /tmp
deploy:
  - echo ok
`
	require.NoError(t, os.WriteFile(filepath.Join(svcDir, "testsvc.yaml"), []byte(svcYAML), 0o644))

	return svcDir
}

func writeDashboardOnlyConfig(t *testing.T, dir string) string {
	t.Helper()
	svcDir := writeTestConfig(t, dir)

	configYAML := `services_dir: ` + svcDir + `
log_dir: ` + filepath.Join(dir, "logs") + `
state_dir: ` + filepath.Join(dir, "state") + `
dashboard:
  port: 0
`
	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(configYAML), 0o644))

	// Create a .env with no tokens
	envPath := filepath.Join(dir, ".env")
	require.NoError(t, os.WriteFile(envPath, []byte(""), 0o644))

	return cfgPath
}

func writeMinimalConfig(t *testing.T, dir string) string {
	t.Helper()
	svcDir := writeTestConfig(t, dir)

	configYAML := `services_dir: ` + svcDir + `
log_dir: ` + filepath.Join(dir, "logs") + `
state_dir: ` + filepath.Join(dir, "state") + `
`
	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(configYAML), 0o644))

	envPath := filepath.Join(dir, ".env")
	require.NoError(t, os.WriteFile(envPath, []byte(""), 0o644))

	return cfgPath
}

func TestNew_DashboardOnly(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeDashboardOnlyConfig(t, dir)
	envPath := filepath.Join(dir, ".env")

	tmplFS := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte(minimalTemplate)},
	}

	a, err := app_New(cfgPath, envPath, tmplFS)
	require.NoError(t, err)
	require.NotNil(t, a)

	// Dashboard should be configured
	assert.NotNil(t, a.dashboardSrv)

	// Discord and Mattermost should NOT be configured
	assert.Nil(t, a.discordBot)
	assert.Nil(t, a.mmBot)

	// Webhook should NOT be configured (no webhook section in config)
	assert.Nil(t, a.webhookSrv)

	// Manager should be created
	assert.NotNil(t, a.manager)

	// Clean up
	a.manager.Stop()
}

func TestNew_NoFrontends(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeMinimalConfig(t, dir)
	envPath := filepath.Join(dir, ".env")

	tmplFS := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte(minimalTemplate)},
	}

	a, err := app_New(cfgPath, envPath, tmplFS)
	require.NoError(t, err)
	require.NotNil(t, a)

	// Manager should still exist
	assert.NotNil(t, a.manager)

	// No frontends
	assert.Nil(t, a.discordBot)
	assert.Nil(t, a.mmBot)
	assert.Nil(t, a.webhookSrv)
	assert.Nil(t, a.dashboardSrv)

	a.manager.Stop()
}

func TestHandlePush_MatchingService(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeMinimalConfig(t, dir)
	envPath := filepath.Join(dir, ".env")

	tmplFS := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte(minimalTemplate)},
	}

	a, err := app_New(cfgPath, envPath, tmplFS)
	require.NoError(t, err)
	defer a.manager.Stop()

	// HandlePush with matching repo/branch should trigger deploy
	a.HandlePush("org/testrepo", "main")

	// Give the async deploy a moment to be queued
	time.Sleep(50 * time.Millisecond)

	// The service should exist and have been triggered
	names := a.manager.ServiceNames()
	assert.Contains(t, names, "testsvc")
}

func TestHandlePush_RequireConfirmation(t *testing.T) {
	dir := t.TempDir()
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

	configYAML := `services_dir: ` + svcDir + `
log_dir: ` + filepath.Join(dir, "logs") + `
state_dir: ` + filepath.Join(dir, "state") + `
`
	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(configYAML), 0o644))

	envPath := filepath.Join(dir, ".env")
	require.NoError(t, os.WriteFile(envPath, []byte(""), 0o644))

	tmplFS := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte(minimalTemplate)},
	}

	a, err := app_New(cfgPath, envPath, tmplFS)
	require.NoError(t, err)
	defer a.manager.Stop()

	// HandlePush with require_confirmation should add pending, not deploy
	a.HandlePush("org/confirmrepo", "main")

	// Check that confirmation is pending
	assert.True(t, a.confirmations.IsPending("confirmsvc"))
}

func TestConfirm_AfterPush(t *testing.T) {
	dir := t.TempDir()
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

	configYAML := `services_dir: ` + svcDir + `
log_dir: ` + filepath.Join(dir, "logs") + `
state_dir: ` + filepath.Join(dir, "state") + `
`
	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(configYAML), 0o644))

	envPath := filepath.Join(dir, ".env")
	require.NoError(t, os.WriteFile(envPath, []byte(""), 0o644))

	tmplFS := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte(minimalTemplate)},
	}

	a, err := app_New(cfgPath, envPath, tmplFS)
	require.NoError(t, err)
	defer a.manager.Stop()

	// Add a pending confirmation via HandlePush
	a.HandlePush("org/confirmrepo", "main")
	assert.True(t, a.confirmations.IsPending("confirmsvc"))

	// Confirm it
	ok := a.Confirm("confirmsvc")
	assert.True(t, ok)

	// Pending should be cleared
	assert.False(t, a.confirmations.IsPending("confirmsvc"))
}

func TestConfirm_WithoutPending(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeMinimalConfig(t, dir)
	envPath := filepath.Join(dir, ".env")

	tmplFS := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte(minimalTemplate)},
	}

	a, err := app_New(cfgPath, envPath, tmplFS)
	require.NoError(t, err)
	defer a.manager.Stop()

	// Confirm with no pending should return false
	ok := a.Confirm("nonexistent")
	assert.False(t, ok)
}

func TestShutdown(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeDashboardOnlyConfig(t, dir)
	envPath := filepath.Join(dir, ".env")

	tmplFS := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte(minimalTemplate)},
	}

	a, err := app_New(cfgPath, envPath, tmplFS)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())

	// Run in background
	done := make(chan error, 1)
	go func() {
		done <- a.Run(ctx)
	}()

	// Give it a moment to start
	time.Sleep(50 * time.Millisecond)

	// Shutdown should complete without hanging
	cancel()
	a.Shutdown()

	select {
	case <-done:
		// OK
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after Shutdown")
	}
}

func TestManager_Accessor(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeMinimalConfig(t, dir)
	envPath := filepath.Join(dir, ".env")

	tmplFS := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte(minimalTemplate)},
	}

	a, err := app_New(cfgPath, envPath, tmplFS)
	require.NoError(t, err)
	defer a.manager.Stop()

	// Manager() should return the same manager
	assert.Equal(t, a.manager, a.Manager())
}

// app_New is an alias that lets us call New without conflicting with test helpers.
// This avoids import cycles and lets the test file stay in the same package.
var app_New = New
