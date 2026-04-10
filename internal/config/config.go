package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/joho/godotenv"
	"gopkg.in/yaml.v3"
)

// DiscordConfig holds Discord bot connection settings.
type DiscordConfig struct {
	GuildID   string `yaml:"guild_id"`
	ChannelID string `yaml:"channel_id"`
}

// MattermostConfig holds Mattermost bot connection settings.
type MattermostConfig struct {
	URL     string `yaml:"url"`
	Channel string `yaml:"channel"`
}

// WebhookConfig holds GitHub webhook listener settings.
type WebhookConfig struct {
	Port int `yaml:"port"`
}

// DashboardConfig holds web dashboard settings.
type DashboardConfig struct {
	Port int `yaml:"port"`
}

// Config is the top-level application configuration loaded from YAML.
type Config struct {
	ServicesDir string            `yaml:"services_dir"`
	LogDir      string            `yaml:"log_dir"`
	StateDir    string            `yaml:"state_dir"`
	Discord     *DiscordConfig    `yaml:"discord"`
	Mattermost  *MattermostConfig `yaml:"mattermost"`
	Webhook     *WebhookConfig    `yaml:"webhook"`
	Dashboard   *DashboardConfig  `yaml:"dashboard"`
}

// ServiceProcessConfig describes how to manage a service's process.
type ServiceProcessConfig struct {
	Cmd string `yaml:"cmd"`
}

// ServiceConfig describes a single deployable service.
type ServiceConfig struct {
	Name                string               `yaml:"-"`
	Branch              string               `yaml:"branch"`
	Repo                string               `yaml:"repo"`
	Dir                 string               `yaml:"dir"`
	Entrypoint          []string             `yaml:"entrypoint"`
	Process             ServiceProcessConfig `yaml:"process"`
	Deploy              []string             `yaml:"deploy"`
	ServiceName         string               `yaml:"service_name"`
	UserService         bool                 `yaml:"user_service"`
	Sudo                bool                 `yaml:"sudo"`
	RequireConfirmation bool                 `yaml:"require_confirmation"`
	SelfDeploy          bool                 `yaml:"self_deploy"`
	Adopt               *bool                `yaml:"adopt"`
}

// ShouldAdopt returns whether this service should be adopted on startup.
// Defaults to true if not explicitly set.
func (s *ServiceConfig) ShouldAdopt() bool {
	if s.Adopt != nil {
		return *s.Adopt
	}
	return true
}

// Env holds secrets loaded from environment variables or a .env file.
type Env struct {
	DiscordToken    string
	MattermostToken string
	WebhookSecret   string
}

// LoadConfig reads a YAML config file and applies defaults.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	cfg := &Config{
		ServicesDir: "./services",
		LogDir:      "./logs",
		StateDir:    "./state",
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	return cfg, nil
}

// LoadEnv reads secrets from a .env file. If path is empty or the file does
// not exist, it falls back to os.Getenv for each variable.
func LoadEnv(path string) (*Env, error) {
	if path != "" {
		vars, err := godotenv.Read(path)
		if err == nil {
			return &Env{
				DiscordToken:    vars["DISCORD_TOKEN"],
				MattermostToken: vars["MATTERMOST_TOKEN"],
				WebhookSecret:   vars["GITHUB_WEBHOOK_SECRET"],
			}, nil
		}
		// Only fall through to os.Getenv if the file simply doesn't exist.
		// Other errors (permission denied, malformed content) are real problems.
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("reading env file: %w", err)
		}
	}

	return &Env{
		DiscordToken:    os.Getenv("DISCORD_TOKEN"),
		MattermostToken: os.Getenv("MATTERMOST_TOKEN"),
		WebhookSecret:   os.Getenv("GITHUB_WEBHOOK_SECRET"),
	}, nil
}

// LoadServices scans a directory for .yaml/.yml files and parses each as a
// ServiceConfig. The Name field is set from the filename when not provided
// in the YAML itself.
func LoadServices(dir string) ([]ServiceConfig, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading services dir: %w", err)
	}

	var services []ServiceConfig
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := filepath.Ext(entry.Name())
		if ext != ".yaml" && ext != ".yml" {
			continue
		}

		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("reading service file %s: %w", entry.Name(), err)
		}

		var svc ServiceConfig
		if err := yaml.Unmarshal(data, &svc); err != nil {
			return nil, fmt.Errorf("parsing service file %s: %w", entry.Name(), err)
		}

		if svc.Name == "" {
			svc.Name = strings.TrimSuffix(entry.Name(), ext)
		}

		services = append(services, svc)
	}

	return services, nil
}
