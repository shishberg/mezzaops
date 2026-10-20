package config

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestYAMLKeys_InlineSkipAndDefaults(t *testing.T) {
	type base struct {
		Shared string `yaml:"shared"`
	}
	type sample struct {
		base      `yaml:",inline"`
		Named     string `yaml:"named,omitempty"`
		Skipped   string `yaml:"-"`
		Defaulted string
		hidden    string //nolint:unused // verifies unexported fields are ignored
	}
	got := yamlKeys(reflect.TypeFor[sample]())
	assert.Equal(t, map[string]struct{}{
		"shared":    {},
		"named":     {},
		"defaulted": {},
	}, got)
}
