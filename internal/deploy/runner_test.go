package deploy_test

import (
	"context"
	"testing"

	"github.com/shishberg/mezzaops/internal/deploy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunSteps_AllSucceed(t *testing.T) {
	steps := []string{
		"echo step1",
		"echo step2",
		"echo step3",
	}

	result, err := deploy.RunSteps(context.Background(), steps, t.TempDir())
	require.NoError(t, err)

	assert.Equal(t, "success", result.Status)
	assert.Contains(t, result.Output, "step1")
	assert.Contains(t, result.Output, "step2")
	assert.Contains(t, result.Output, "step3")
	assert.Empty(t, result.FailedStep)
}

func TestRunSteps_FailsOnMiddleStep(t *testing.T) {
	steps := []string{
		"echo ok",
		"exit 1",
		"echo should-not-run",
	}

	result, err := deploy.RunSteps(context.Background(), steps, t.TempDir())
	require.NoError(t, err)

	assert.Equal(t, "failed", result.Status)
	assert.Equal(t, "exit 1", result.FailedStep)
	assert.NotContains(t, result.Output, "should-not-run")
}

func TestRunSteps_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	steps := []string{"echo hello"}

	result, err := deploy.RunSteps(ctx, steps, t.TempDir())
	require.NoError(t, err)
	assert.Equal(t, "failed", result.Status)
}

func TestRunSteps_OutputCapturesStdoutAndStderr(t *testing.T) {
	steps := []string{
		"echo stdout-msg",
		"echo stderr-msg >&2",
	}

	result, err := deploy.RunSteps(context.Background(), steps, t.TempDir())
	require.NoError(t, err)

	assert.Equal(t, "success", result.Status)
	assert.Contains(t, result.Output, "stdout-msg")
	assert.Contains(t, result.Output, "stderr-msg")
}

func TestRunSteps_EmptySteps(t *testing.T) {
	result, err := deploy.RunSteps(context.Background(), nil, t.TempDir())
	require.NoError(t, err)
	assert.Equal(t, "success", result.Status)
}
