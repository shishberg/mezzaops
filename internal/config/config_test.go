package config_test

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shishberg/mezzaops/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadConfig_AllSections(t *testing.T) {
	yaml := `
services_dir: "/opt/services"
log_dir: "/var/log/mezzaops"
state_dir: "/var/state"
discord:
  guild_id: "123"
  channel_id: "456"
mattermost:
  url: "http://mm:8065"
  channel: "team/chan"
matrix:
  homeserver: "https://matrix.example.org"
  room: "!abc:matrix.example.org"
  command_prefix: "!ops"
  crypto_db: "/var/state/matrix.db"
webhook:
  port: 9090
dashboard:
  port: 9091
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o644))

	cfg, err := config.LoadConfig(path)
	require.NoError(t, err)

	assert.Equal(t, "/opt/services", cfg.ServicesDir)
	assert.Equal(t, "/var/log/mezzaops", cfg.LogDir)
	assert.Equal(t, "/var/state", cfg.StateDir)
	require.NotNil(t, cfg.Discord)
	assert.Equal(t, "123", cfg.Discord.GuildID)
	assert.Equal(t, "456", cfg.Discord.ChannelID)

	require.NotNil(t, cfg.Mattermost)
	assert.Equal(t, "http://mm:8065", cfg.Mattermost.URL)
	assert.Equal(t, "team/chan", cfg.Mattermost.Channel)

	require.NotNil(t, cfg.Matrix)
	assert.Equal(t, "https://matrix.example.org", cfg.Matrix.Homeserver)
	assert.Equal(t, "!abc:matrix.example.org", cfg.Matrix.Room)
	assert.Equal(t, "!ops", cfg.Matrix.CommandPrefix)
	assert.Equal(t, "/var/state/matrix.db", cfg.Matrix.CryptoDB)

	require.NotNil(t, cfg.Webhook)
	assert.Equal(t, 9090, cfg.Webhook.Port)

	require.NotNil(t, cfg.Dashboard)
	assert.Equal(t, 9091, cfg.Dashboard.Port)
}

func TestLoadConfig_OnlyDiscord(t *testing.T) {
	yaml := `
discord:
  guild_id: "abc"
  channel_id: "def"
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o644))

	cfg, err := config.LoadConfig(path)
	require.NoError(t, err)

	require.NotNil(t, cfg.Discord)
	assert.Equal(t, "abc", cfg.Discord.GuildID)
	assert.Nil(t, cfg.Mattermost)
	assert.Nil(t, cfg.Matrix)
	assert.Nil(t, cfg.Webhook)
	assert.Nil(t, cfg.Dashboard)
}

func TestLoadConfig_Defaults(t *testing.T) {
	yaml := `{}`
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o644))

	cfg, err := config.LoadConfig(path)
	require.NoError(t, err)

	assert.Equal(t, "./services", cfg.ServicesDir)
	assert.Equal(t, "./logs", cfg.LogDir)
	assert.Equal(t, "./state", cfg.StateDir)
}

func TestLoadEnv_FromFile(t *testing.T) {
	content := `DISCORD_TOKEN=disc-tok
MATTERMOST_TOKEN=mm-tok
GITHUB_WEBHOOK_SECRET=secret123
MATRIX_USER_ID=@bot:matrix.example.org
MATRIX_DEVICE_ID=DEV01
MATRIX_ACCESS_TOKEN=syt_abc
MATRIX_PICKLE_KEY=pickle-pass
`
	path := filepath.Join(t.TempDir(), ".env")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	env, err := config.LoadEnv(path)
	require.NoError(t, err)

	assert.Equal(t, "disc-tok", env.DiscordToken)
	assert.Equal(t, "mm-tok", env.MattermostToken)
	assert.Equal(t, "secret123", env.WebhookSecret)
	assert.Equal(t, "@bot:matrix.example.org", env.MatrixUserID)
	assert.Equal(t, "DEV01", env.MatrixDeviceID)
	assert.Equal(t, "syt_abc", env.MatrixAccessToken)
	assert.Equal(t, "pickle-pass", env.MatrixPickleKey)
}

func TestLoadEnv_FallbackToEnvironment(t *testing.T) {
	t.Setenv("DISCORD_TOKEN", "env-disc")
	t.Setenv("MATTERMOST_TOKEN", "env-mm")
	t.Setenv("GITHUB_WEBHOOK_SECRET", "env-secret")
	t.Setenv("MATRIX_USER_ID", "@env:example.org")
	t.Setenv("MATRIX_DEVICE_ID", "ENVDEV")
	t.Setenv("MATRIX_ACCESS_TOKEN", "env-token")
	t.Setenv("MATRIX_PICKLE_KEY", "env-pickle")

	env, err := config.LoadEnv("")
	require.NoError(t, err)

	assert.Equal(t, "env-disc", env.DiscordToken)
	assert.Equal(t, "env-mm", env.MattermostToken)
	assert.Equal(t, "env-secret", env.WebhookSecret)
	assert.Equal(t, "@env:example.org", env.MatrixUserID)
	assert.Equal(t, "ENVDEV", env.MatrixDeviceID)
	assert.Equal(t, "env-token", env.MatrixAccessToken)
	assert.Equal(t, "env-pickle", env.MatrixPickleKey)
}

func TestLoadEnv_FallbackWhenFileMissing(t *testing.T) {
	t.Setenv("DISCORD_TOKEN", "fallback-disc")
	t.Setenv("MATTERMOST_TOKEN", "fallback-mm")
	t.Setenv("GITHUB_WEBHOOK_SECRET", "fallback-secret")

	env, err := config.LoadEnv(filepath.Join(t.TempDir(), "nonexistent.env"))
	require.NoError(t, err)

	assert.Equal(t, "fallback-disc", env.DiscordToken)
	assert.Equal(t, "fallback-mm", env.MattermostToken)
	assert.Equal(t, "fallback-secret", env.WebhookSecret)
}

func TestLoadEnv_ErrorOnUnreadableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	require.NoError(t, os.WriteFile(path, []byte("TOKEN=x"), 0o000))

	_, err := config.LoadEnv(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading env file")
}

func TestLoadConfig_FileNotFound(t *testing.T) {
	_, err := config.LoadConfig(filepath.Join(t.TempDir(), "missing.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading config")
}

func TestLoadConfig_InvalidYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yaml")
	require.NoError(t, os.WriteFile(path, []byte("{{invalid"), 0o644))

	_, err := config.LoadConfig(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing config")
}

func TestLoadServices_EmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	services, err := config.LoadServices(dir)
	require.NoError(t, err)
	assert.Empty(t, services)
}

func TestLoadServices_NonexistentDirectory(t *testing.T) {
	_, err := config.LoadServices(filepath.Join(t.TempDir(), "nope"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading services dir")
}

func TestLoadServices_InvalidYAMLFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte("{{invalid"), 0o644))

	_, err := config.LoadServices(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing service file")
}

func TestLoadServices_MultipleFiles(t *testing.T) {
	dir := t.TempDir()

	svc1 := `
branch: main
repo: https://github.com/foo/bar
dir: /opt/bar
entrypoint: ["go", "run", "."]
deploy:
  - "git pull"
  - "go build"
`
	svc2 := `
branch: develop
repo: https://github.com/foo/baz
dir: /opt/baz
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bar.yaml"), []byte(svc1), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "baz.yml"), []byte(svc2), 0o644))

	services, err := config.LoadServices(dir)
	require.NoError(t, err)
	require.Len(t, services, 2)

	// Files are read in directory order, which is alphabetical
	assert.Equal(t, "bar", services[0].Name)
	assert.Equal(t, "main", services[0].Branch)
	assert.Equal(t, []string{"go", "run", "."}, services[0].Entrypoint)
	assert.Equal(t, []string{"git pull", "go build"}, services[0].Deploy)

	assert.Equal(t, "baz", services[1].Name)
	assert.Equal(t, "develop", services[1].Branch)
}

func TestLoadServices_NameFromFilename(t *testing.T) {
	dir := t.TempDir()

	yaml := `
branch: main
repo: https://github.com/foo/bar
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "myservice.yaml"), []byte(yaml), 0o644))

	services, err := config.LoadServices(dir)
	require.NoError(t, err)
	require.Len(t, services, 1)
	assert.Equal(t, "myservice", services[0].Name)
}

func TestLoadServices_IgnoresNonYAMLAndDirectories(t *testing.T) {
	dir := t.TempDir()

	yaml := `branch: main`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "good.yaml"), []byte(yaml), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("not yaml"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.md"), []byte("# notes"), 0o644))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "subdir"), 0o755))

	services, err := config.LoadServices(dir)
	require.NoError(t, err)
	require.Len(t, services, 1)
	assert.Equal(t, "good", services[0].Name)
}

// --- C6: warn on unknown top-level config keys ---

// captureLog redirects the default log writer for the duration of fn and
// returns whatever was written. Tests use it to assert on log.Printf output
// from LoadConfig. Concurrent calls are serialised so parallel test runs do
// not race on the default log writer.
var logCaptureMu sync.Mutex

func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	logCaptureMu.Lock()
	defer logCaptureMu.Unlock()

	orig := log.Writer()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })

	fn()
	return buf.String()
}

func TestLoadConfig_WarnsOnUnknownTopLevelKey(t *testing.T) {
	// `process:` is the canonical example from boot-resilience.md P6: the
	// operator reasonably assumed it was a global default, but Config has
	// no Process field and the block is silently discarded.
	yaml := `
log_dir: "/var/log/mezzaops"
state_dir: "/var/state"
process:
  adopt: true
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o644))

	var (
		cfg *config.Config
		err error
	)
	out := captureLog(t, func() {
		cfg, err = config.LoadConfig(path)
	})

	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, "/var/log/mezzaops", cfg.LogDir)
	assert.Equal(t, "/var/state", cfg.StateDir)

	// Assert the warning names the unknown key as a quoted token, not just
	// that the string "process" appears in the help text.
	keys := parseUnknownKeys(out)
	assert.Contains(t, keys, "process",
		"expected a warning naming 'process' as an unknown key, got: %q", out)
}

func TestLoadConfig_WarnsOnMultipleUnknownTopLevelKeys(t *testing.T) {
	yaml := `
log_dir: "/var/log/mezzaops"
process:
  adopt: true
typo_field: 42
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o644))

	var err error
	out := captureLog(t, func() {
		_, err = config.LoadConfig(path)
	})

	require.NoError(t, err)
	keys := parseUnknownKeys(out)
	assert.ElementsMatch(t, []string{"process", "typo_field"}, keys,
		"expected warnings for both unknown keys, got: %v", keys)
}

func TestLoadConfig_WarnsAfterYAMLMergeKey(t *testing.T) {
	// Regression for the codex review: walking the raw AST warned about
	// the merge key `<<` itself, not the keys it merged in. A merged-in
	// `process:` block should warn about `process`.
	yaml := `
defaults: &def
  process:
    adopt: true
  log_dir: /tmp
log_dir: /tmp
process: {adopt: true}
<<: *def
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o644))

	var err error
	out := captureLog(t, func() {
		_, err = config.LoadConfig(path)
	})
	require.NoError(t, err)
	keys := parseUnknownKeys(out)
	assert.Contains(t, keys, "process",
		"merged-in process should be reported as unknown, got: %v", keys)
	assert.NotContains(t, keys, "<<",
		"the merge key itself should not be reported, got: %v", keys)
}

// parseUnknownKeys extracts the set of unknown top-level keys named in log
// output. LoadConfig logs them as `unknown top-level key %q`; we parse the
// quoted value rather than asserting on substrings (which the help text
// would otherwise satisfy for any unknown key).
func parseUnknownKeys(logOutput string) []string {
	var keys []string
	for _, line := range strings.Split(logOutput, "\n") {
		const marker = `unknown top-level key "`
		i := strings.Index(line, marker)
		if i < 0 {
			continue
		}
		rest := line[i+len(marker):]
		j := strings.IndexByte(rest, '"')
		if j < 0 {
			continue
		}
		keys = append(keys, rest[:j])
	}
	return keys
}

func TestLoadConfig_NoWarningOnValidConfig(t *testing.T) {
	// Same shape as TestLoadConfig_AllSections. Every key here is in the
	// known set, so no warning is expected.
	yaml := `
services_dir: "/opt/services"
log_dir: "/var/log/mezzaops"
state_dir: "/var/state"
discord:
  guild_id: "123"
  channel_id: "456"
mattermost:
  url: "http://mm:8065"
  channel: "team/chan"
matrix:
  homeserver: "https://matrix.example.org"
  room: "!abc:matrix.example.org"
  command_prefix: "!ops"
  crypto_db: "/var/state/matrix.db"
webhook:
  port: 9090
dashboard:
  port: 9091
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o644))

	var (
		cfg *config.Config
		err error
	)
	out := captureLog(t, func() {
		cfg, err = config.LoadConfig(path)
	})

	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, "/opt/services", cfg.ServicesDir)
	// Default test runs in parallel; captureLog already serialises against
	// other captureLog callers, so we just need to assert no warning was
	// emitted about an unknown key.
	assert.False(t, strings.Contains(out, "unknown"),
		"valid config should not log a warning, got: %q", out)
}

func TestLoadConfig_NoWarningOnMinimalConfig(t *testing.T) {
	// Just the three required keys plus one optional section that happens
	// to be absent — no warning expected, because absence is not a typo.
	yaml := `{}`
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o644))

	var err error
	out := captureLog(t, func() {
		_, err = config.LoadConfig(path)
	})
	require.NoError(t, err)
	assert.False(t, strings.Contains(out, "unknown"),
		"minimal config should not log a warning, got: %q", out)
}

func TestLoadConfig_DefaultsReadinessTimeoutTo30s(t *testing.T) {
	yaml := `{}`
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o644))

	cfg, err := config.LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, 30*time.Second, cfg.EffectiveReadinessTimeout(),
		"zero/empty readiness_timeout should default to 30s")
}

func TestLoadConfig_ParsesReadinessTimeout(t *testing.T) {
	// YAML library decodes time.Duration as nanoseconds when the value is
	// a bare integer, so we write it as a Go duration string ("45s") which
	// the library also accepts.
	yaml := `readiness_timeout: 45s`
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o644))

	cfg, err := config.LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, 45*time.Second, cfg.EffectiveReadinessTimeout())
}

func TestLoadConfig_ZeroReadinessTimeoutUsesDefault(t *testing.T) {
	// Explicit zero should still trigger the default. The app layer
	// must not block indefinitely on a misconfigured value.
	yaml := `readiness_timeout: 0s`
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o644))

	cfg, err := config.LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, 30*time.Second, cfg.EffectiveReadinessTimeout())
}

func TestLoadConfig_ReadinessTimeoutIsKnownKey(t *testing.T) {
	// readiness_timeout is a real top-level key — it must not be reported
	// as unknown. Regression for a codex review finding: adding the field
	// to Config but forgetting to add it to knownTopLevelConfigKeys.
	yaml := `readiness_timeout: 45s`
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o644))

	var err error
	out := captureLog(t, func() {
		_, err = config.LoadConfig(path)
	})
	require.NoError(t, err)
	assert.False(t, strings.Contains(out, "unknown"),
		"readiness_timeout should be a known top-level key, got: %q", out)
}
