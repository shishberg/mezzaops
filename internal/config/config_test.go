package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/shishberg/mezzaops/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadConfig_AllSections(t *testing.T) {
	yaml := `
services_dir: "/opt/services"
log_dir: "/var/log/mezzaops"
state_dir: "/var/state"
process:
  adopt: false
discord:
  guild_id: "123"
  channel_id: "456"
mattermost:
  url: "http://mm:8065"
  channel: "team/chan"
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
	assert.False(t, cfg.Process.Adopt)

	require.NotNil(t, cfg.Discord)
	assert.Equal(t, "123", cfg.Discord.GuildID)
	assert.Equal(t, "456", cfg.Discord.ChannelID)

	require.NotNil(t, cfg.Mattermost)
	assert.Equal(t, "http://mm:8065", cfg.Mattermost.URL)
	assert.Equal(t, "team/chan", cfg.Mattermost.Channel)

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
	assert.True(t, cfg.Process.Adopt)
}

func TestLoadEnv_FromFile(t *testing.T) {
	content := `DISCORD_TOKEN=disc-tok
MATTERMOST_TOKEN=mm-tok
GITHUB_WEBHOOK_SECRET=secret123
`
	path := filepath.Join(t.TempDir(), ".env")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	env, err := config.LoadEnv(path)
	require.NoError(t, err)

	assert.Equal(t, "disc-tok", env.DiscordToken)
	assert.Equal(t, "mm-tok", env.MattermostToken)
	assert.Equal(t, "secret123", env.WebhookSecret)
}

func TestLoadEnv_FallbackToEnvironment(t *testing.T) {
	t.Setenv("DISCORD_TOKEN", "env-disc")
	t.Setenv("MATTERMOST_TOKEN", "env-mm")
	t.Setenv("GITHUB_WEBHOOK_SECRET", "env-secret")

	env, err := config.LoadEnv("")
	require.NoError(t, err)

	assert.Equal(t, "env-disc", env.DiscordToken)
	assert.Equal(t, "env-mm", env.MattermostToken)
	assert.Equal(t, "env-secret", env.WebhookSecret)
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
