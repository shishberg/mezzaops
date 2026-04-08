package deploy

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// Result describes the outcome of running a sequence of deploy steps.
type Result struct {
	Status     string // "success" or "failed"
	Output     string // combined stdout/stderr from all steps
	FailedStep string // which step failed, empty on success
}

// RunSteps executes shell steps sequentially in the given working directory.
// It stops on the first failure or context cancellation.
func RunSteps(ctx context.Context, steps []string, workingDir string) (*Result, error) {
	var output bytes.Buffer

	for _, step := range steps {
		if ctx.Err() != nil {
			return &Result{
				Status:     "failed",
				Output:     output.String(),
				FailedStep: step,
			}, nil
		}

		fmt.Fprintf(&output, "$ %s\n", step)

		cmd := exec.CommandContext(ctx, "sh", "-c", step)
		cmd.Dir = workingDir
		cmd.Stdout = &output
		cmd.Stderr = &output

		if err := cmd.Run(); err != nil {
			fmt.Fprintf(&output, "ERROR: %s\n", err)
			return &Result{
				Status:     "failed",
				Output:     output.String(),
				FailedStep: step,
			}, nil
		}
	}

	return &Result{
		Status: "success",
		Output: output.String(),
	}, nil
}
