package config

import (
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

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

// MatrixConfig holds Matrix bot connection settings. Secrets (user/device ID,
// access token, pickle key) live in Env, not here.
type MatrixConfig struct {
	Homeserver    string `yaml:"homeserver"`
	Room          string `yaml:"room"`
	CommandPrefix string `yaml:"command_prefix"`
	CryptoDB      string `yaml:"crypto_db"`
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
	Matrix      *MatrixConfig     `yaml:"matrix"`
	Webhook     *WebhookConfig    `yaml:"webhook"`
	Dashboard   *DashboardConfig  `yaml:"dashboard"`

	// ReadinessTimeout is the maximum time the app layer waits for all
	// chat frontends to report Ready before firing manager.SignalReady().
	// Bounded so a chat server that's unreachable on boot (e.g. Tailscale
	// MagicDNS not ready yet) cannot block adopt:false / autostart
	// services indefinitely. Default 30s when zero or unset.
	ReadinessTimeout time.Duration `yaml:"readiness_timeout"`

	// StartOnBoot, when true, makes the manager autostart process
	// services on cold boot if adoption did not result in a running
	// process. Per-service ServiceConfig.Autostart overrides this.
	// Default false to preserve the pre-C1 behaviour (adopt-only).
	StartOnBoot bool `yaml:"start_on_boot"`
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
	// Autostart, when true, makes the manager Start a process service
	// on cold boot if adoption did not result in a running process
	// (stale pid, no state, adoption disabled). Per-service overrides
	// the global Config.StartOnBoot default. nil means "use the
	// global default". Honoured only when the persisted desired
	// state is not "stopped" (operator explicitly stopped) unless
	// AutostartForce is also set.
	Autostart *bool `yaml:"autostart"`
	// AutostartForce overrides the persisted-"stopped" guard: a
	// service with status:"stopped" in its state file is still
	// started on cold boot. Use with care — it ignores the
	// operator's last command.
	AutostartForce bool `yaml:"autostart_force"`
}

// ShouldAdopt returns whether this service should be adopted on startup.
// Defaults to true if not explicitly set.
func (s *ServiceConfig) ShouldAdopt() bool {
	if s.Adopt != nil {
		return *s.Adopt
	}
	return true
}

// ShouldAutostart resolves the per-service autostart decision against
// the global default. Per-service autostart overrides the global; if
// neither is set, the default is false (preserve the pre-C1
// behaviour). Returns the resolved autostart decision.
func (s *ServiceConfig) ShouldAutostart(globalDefault bool) bool {
	if s.Autostart != nil {
		return *s.Autostart
	}
	return globalDefault
}

// DefaultReadinessTimeout is used when Config.ReadinessTimeout is zero or
// unset. Bounded so a chat server that's unreachable on boot (e.g. Tailscale
// MagicDNS not ready yet) cannot block adopt:false / autostart services
// indefinitely.
const DefaultReadinessTimeout = 30 * time.Second

// EffectiveReadinessTimeout returns the configured readiness timeout, or
// DefaultReadinessTimeout when the field is zero.
func (c *Config) EffectiveReadinessTimeout() time.Duration {
	if c.ReadinessTimeout <= 0 {
		return DefaultReadinessTimeout
	}
	return c.ReadinessTimeout
}

// Env holds secrets loaded from environment variables or a .env file.
type Env struct {
	DiscordToken      string
	MattermostToken   string
	WebhookSecret     string
	MatrixUserID      string
	MatrixDeviceID    string
	MatrixAccessToken string
	MatrixPickleKey   string
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

	// C6: surface unknown top-level keys so a misplaced block (e.g.
	// `process: { adopt: true }` from an older config shape) is no longer
	// silently discarded. We parse the file a second time into a Node so
	// the existing Unmarshal semantics are not perturbed; turning
	// yaml.Unmarshal into a strict decode here would also turn
	// previously-valid configs (e.g. unknown sub-keys) into warnings.
	warnUnknownTopLevelKeys(path, data)

	return cfg, nil
}

// knownTopLevelConfigKeys is the set of top-level keys LoadConfig recognises.
// It is derived from the yaml tags on Config, so a new field is recognised
// automatically.
var knownTopLevelConfigKeys, configAcceptsAnyKey = yamlKeys(reflect.TypeFor[Config]())

// yamlKeys returns the set of YAML mapping keys that decoding into struct
// type t accepts. It follows yaml.v3 conventions: a tag of "-" omits the
// field, ",inline" on a struct field merges that struct's keys, an empty
// name defaults to the lowercased field name, and unexported fields are
// ignored unless embedded, as in yaml.v3. acceptsAny is true when the struct
// has a ",inline" map field, in which case every key is valid.
func yamlKeys(t reflect.Type) (keys map[string]struct{}, acceptsAny bool) {
	keys = make(map[string]struct{})
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() && !f.Anonymous {
			continue
		}
		name, opts, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == "-" && opts == "" {
			continue
		}
		if slices.Contains(strings.Split(opts, ","), "inline") {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			switch ft.Kind() {
			case reflect.Struct:
				inner, innerAny := yamlKeys(ft)
				maps.Copy(keys, inner)
				acceptsAny = acceptsAny || innerAny
			case reflect.Map:
				acceptsAny = true
			}
			continue
		}
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		keys[name] = struct{}{}
	}
	return keys, acceptsAny
}

// warnUnknownTopLevelKeys walks the top-level mapping of a parsed config
// file and logs a warning for each key that is not in
// knownTopLevelConfigKeys. Load still succeeds — warnings are informational
// only. Malformed YAML is left to the main parser to reject.
//
// We unmarshal into map[string]yaml.Node (rather than walking the raw AST)
// so that YAML merge keys (`<<`) and aliases are resolved by the decoder
// before we look at the top-level keys. Walking the raw Node.Content slice
// would warn about `<<` itself, not the keys being merged in.
func warnUnknownTopLevelKeys(path string, data []byte) {
	if configAcceptsAnyKey {
		return // Config has an inline map, so no key is unknown.
	}
	var top map[string]yaml.Node
	if err := yaml.Unmarshal(data, &top); err != nil {
		return
	}
	for key := range top {
		if _, ok := knownTopLevelConfigKeys[key]; ok {
			continue
		}
		log.Printf("config %s: unknown top-level key %q (typo, or a misplaced sub-section such as process.* or webhook.* — see boot-resilience.md C6)", path, key)
	}
}

// LoadEnv reads secrets from a .env file. If path is empty or the file does
// not exist, it falls back to os.Getenv for each variable.
func LoadEnv(path string) (*Env, error) {
	if path != "" {
		vars, err := godotenv.Read(path)
		if err == nil {
			return &Env{
				DiscordToken:      vars["DISCORD_TOKEN"],
				MattermostToken:   vars["MATTERMOST_TOKEN"],
				WebhookSecret:     vars["GITHUB_WEBHOOK_SECRET"],
				MatrixUserID:      vars["MATRIX_USER_ID"],
				MatrixDeviceID:    vars["MATRIX_DEVICE_ID"],
				MatrixAccessToken: vars["MATRIX_ACCESS_TOKEN"],
				MatrixPickleKey:   vars["MATRIX_PICKLE_KEY"],
			}, nil
		}
		// Only fall through to os.Getenv if the file simply doesn't exist.
		// Other errors (permission denied, malformed content) are real problems.
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("reading env file: %w", err)
		}
	}

	return &Env{
		DiscordToken:      os.Getenv("DISCORD_TOKEN"),
		MattermostToken:   os.Getenv("MATTERMOST_TOKEN"),
		WebhookSecret:     os.Getenv("GITHUB_WEBHOOK_SECRET"),
		MatrixUserID:      os.Getenv("MATRIX_USER_ID"),
		MatrixDeviceID:    os.Getenv("MATRIX_DEVICE_ID"),
		MatrixAccessToken: os.Getenv("MATRIX_ACCESS_TOKEN"),
		MatrixPickleKey:   os.Getenv("MATRIX_PICKLE_KEY"),
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
