package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFormatStatusOverview(t *testing.T) {
	got := FormatStatusOverview(map[string]ServiceState{
		"zeta":  {Status: "running"},
		"alpha": {Status: "stopped", LastResult: "deploy failed"},
	})
	assert.Equal(t, "**Service Status**\n- **alpha**: stopped (last: deploy failed)\n- **zeta**: running\n", got)
}

func TestFormatStatusOverview_Empty(t *testing.T) {
	assert.Equal(t, "No services configured.", FormatStatusOverview(nil))
}
