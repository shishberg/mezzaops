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
	got, acceptsAny := yamlKeys(reflect.TypeFor[sample]())
	assert.False(t, acceptsAny)
	assert.Equal(t, map[string]struct{}{
		"shared":    {},
		"named":     {},
		"defaulted": {},
	}, got)
}

func TestYAMLKeys_InlineMapAcceptsAnyKey(t *testing.T) {
	type withMap struct {
		Known string         `yaml:"known"`
		Extra map[string]any `yaml:",inline"`
	}
	keys, acceptsAny := yamlKeys(reflect.TypeFor[withMap]())
	assert.True(t, acceptsAny)
	assert.Contains(t, keys, "known")

	_, acceptsAny = yamlKeys(reflect.TypeFor[Config]())
	assert.False(t, acceptsAny)
}
