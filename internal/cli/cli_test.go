package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockManager records calls and returns canned responses.
type mockManager struct {
	doCalls        []doCall
	deployCalls    []string
	reloaded       bool
	startAllCalled bool
	stopAllCalled  bool
}

type doCall struct {
	name, op string
}

func (m *mockManager) Do(name, op string) string {
	m.doCalls = append(m.doCalls, doCall{name, op})
	return fmt.Sprintf("%s: %s done", name, op)
}

func (m *mockManager) RequestDeploy(name string) error {
	m.deployCalls = append(m.deployCalls, name)
	return nil
}

func (m *mockManager) StartAll() {
	m.startAllCalled = true
}

func (m *mockManager) StopAll() {
	m.stopAllCalled = true
}

func (m *mockManager) Reload() error {
	m.reloaded = true
	return nil
}

func (m *mockManager) ServiceNames() []string {
	return []string{"alpha", "bravo"}
}

func (m *mockManager) CountRunning() (int, int) {
	return 1, 2
}

// captureOutput redirects os.Stdout to capture printed output during test.
func captureOutput(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	require.NoError(t, err)

	oldStdout := os.Stdout
	os.Stdout = w

	fn()

	os.Stdout = oldStdout
	_ = w.Close()

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	return buf.String()
}

func TestCLI_List(t *testing.T) {
	mgr := &mockManager{}
	input := strings.NewReader("list\nquit\n")

	output := captureOutput(t, func() {
		err := RunWithReader(context.Background(), mgr, input)
		require.NoError(t, err)
	})

	assert.Contains(t, output, "alpha")
	assert.Contains(t, output, "bravo")
}

func TestCLI_Help(t *testing.T) {
	mgr := &mockManager{}
	input := strings.NewReader("help\nquit\n")

	output := captureOutput(t, func() {
		err := RunWithReader(context.Background(), mgr, input)
		require.NoError(t, err)
	})

	assert.Contains(t, output, "status")
	assert.Contains(t, output, "start")
	assert.Contains(t, output, "deploy")
}

func TestCLI_StartService(t *testing.T) {
	mgr := &mockManager{}
	input := strings.NewReader("start myservice\nquit\n")

	output := captureOutput(t, func() {
		err := RunWithReader(context.Background(), mgr, input)
		require.NoError(t, err)
	})

	require.Len(t, mgr.doCalls, 1)
	assert.Equal(t, "myservice", mgr.doCalls[0].name)
	assert.Equal(t, "start", mgr.doCalls[0].op)
	assert.Contains(t, output, "myservice: start done")
}

func TestCLI_Quit(t *testing.T) {
	mgr := &mockManager{}
	input := strings.NewReader("quit\n")

	err := RunWithReader(context.Background(), mgr, input)
	assert.NoError(t, err)
}

func TestCLI_UnknownCommand(t *testing.T) {
	mgr := &mockManager{}
	input := strings.NewReader("foobar\nquit\n")

	output := captureOutput(t, func() {
		err := RunWithReader(context.Background(), mgr, input)
		require.NoError(t, err)
	})

	assert.Contains(t, output, "unknown command: foobar")
}

func TestCLI_Deploy(t *testing.T) {
	mgr := &mockManager{}
	input := strings.NewReader("deploy myapp\nquit\n")

	output := captureOutput(t, func() {
		err := RunWithReader(context.Background(), mgr, input)
		require.NoError(t, err)
	})

	require.Len(t, mgr.deployCalls, 1)
	assert.Equal(t, "myapp", mgr.deployCalls[0])
	assert.Contains(t, output, "deploy requested for myapp")
}

func TestCLI_Reload(t *testing.T) {
	mgr := &mockManager{}
	input := strings.NewReader("reload\nquit\n")

	output := captureOutput(t, func() {
		err := RunWithReader(context.Background(), mgr, input)
		require.NoError(t, err)
	})

	assert.True(t, mgr.reloaded)
	assert.Contains(t, output, "config reloaded")
}

func TestCLI_Count(t *testing.T) {
	mgr := &mockManager{}
	input := strings.NewReader("count\nquit\n")

	output := captureOutput(t, func() {
		err := RunWithReader(context.Background(), mgr, input)
		require.NoError(t, err)
	})

	assert.Contains(t, output, "1/2 running")
}

func TestCLI_StartAll(t *testing.T) {
	mgr := &mockManager{}
	input := strings.NewReader("start-all\nquit\n")

	output := captureOutput(t, func() {
		err := RunWithReader(context.Background(), mgr, input)
		require.NoError(t, err)
	})

	assert.True(t, mgr.startAllCalled)
	assert.Contains(t, output, "starting all services")
}

func TestCLI_StopAll(t *testing.T) {
	mgr := &mockManager{}
	input := strings.NewReader("stop-all\nquit\n")

	output := captureOutput(t, func() {
		err := RunWithReader(context.Background(), mgr, input)
		require.NoError(t, err)
	})

	assert.True(t, mgr.stopAllCalled)
	assert.Contains(t, output, "stopping all services")
}

func TestCLI_MissingServiceArg(t *testing.T) {
	mgr := &mockManager{}
	input := strings.NewReader("start\nquit\n")

	output := captureOutput(t, func() {
		err := RunWithReader(context.Background(), mgr, input)
		require.NoError(t, err)
	})

	assert.Contains(t, output, "usage: start <service>")
	assert.Empty(t, mgr.doCalls)
}
