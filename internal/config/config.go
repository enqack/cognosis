// Package config owns XDG path resolution and Viper-backed configuration.
// Nothing else in the codebase resolves a filesystem path or reads an env var
// for settings — it all flows through here.
package config

import (
	"errors"
	"strings"
	"time"

	"github.com/spf13/viper"

	"github.com/enqack/cognosis/internal/cogerr"
)

// Persona is one entry in the enabled-persona registry: lightweight
// metadata kept alongside — not instead of — the persona files themselves.
type Persona struct {
	ID          string `mapstructure:"id"`
	Name        string `mapstructure:"name"`
	Description string `mapstructure:"description"`
}

// Embedding identifies the active embedding provider (config-only setting,
// no runtime toggle).
type Embedding struct {
	Provider string `mapstructure:"provider"` // "ollama" | remote provider name
	Model    string `mapstructure:"model"`
	URL      string `mapstructure:"url"`
}

// TLS enables built-in TLS termination — the fallback for setups without a
// reverse proxy. Both files set = non-loopback binds become legal; the
// documented default remote path keeps Cognosis on loopback behind a
// TLS-terminating proxy instead.
type TLS struct {
	CertFile string `mapstructure:"cert_file"`
	KeyFile  string `mapstructure:"key_file"`
}

func (t TLS) Enabled() bool { return t.CertFile != "" && t.KeyFile != "" }

// Config is the typed view of config.yaml plus env overrides.
type Config struct {
	DSN                    string        `mapstructure:"dsn"`
	BindAddress            string        `mapstructure:"bind_address"`
	KBPath                 string        `mapstructure:"kb_path"`
	ReconcileSweepInterval time.Duration `mapstructure:"reconcile_sweep_interval"`
	Embedding              Embedding     `mapstructure:"embedding"`
	TLS                    TLS           `mapstructure:"tls"`
	Personas               []Persona     `mapstructure:"personas"`

	paths Paths
	viper *viper.Viper
}

// Load resolves paths, applies defaults, reads config.yaml if present, and
// binds COGNOSIS_* env vars (e.g. COGNOSIS_DSN) over file values.
func Load() (*Config, error) {
	const op = "config.Load"
	paths := ResolvePaths()

	v := viper.New()
	v.SetConfigFile(paths.ConfigFile())
	v.SetConfigType("yaml")

	v.SetDefault("dsn", "")
	v.SetDefault("bind_address", "127.0.0.1:7433")
	v.SetDefault("kb_path", paths.KBDir())
	v.SetDefault("reconcile_sweep_interval", time.Hour)
	v.SetDefault("embedding.provider", "ollama")
	v.SetDefault("embedding.model", "nomic-embed-text:v1.5")
	v.SetDefault("embedding.url", "http://localhost:11434")
	v.SetDefault("personas", []map[string]any{{"id": "deep-thoughts"}})
	v.SetDefault("tls.cert_file", "")
	v.SetDefault("tls.key_file", "")

	v.SetEnvPrefix("COGNOSIS")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	if err := v.ReadInConfig(); err != nil {
		// A missing config file is the normal first-run state; anything else
		// (unreadable, invalid YAML) is a real error.
		var parseErr *viper.ConfigParseError
		if errors.As(err, &parseErr) {
			return nil, cogerr.E(op, cogerr.Validation, err)
		}
	}

	var c Config
	if err := v.Unmarshal(&c); err != nil {
		return nil, cogerr.E(op, cogerr.Validation, err)
	}
	c.paths = paths
	c.viper = v
	return &c, nil
}

func (c *Config) Paths() Paths { return c.paths }

// EnabledPersonaIDs flattens the persona registry to the enabled id list.
func (c *Config) EnabledPersonaIDs() []string {
	out := make([]string, 0, len(c.Personas))
	for _, p := range c.Personas {
		if p.ID != "" {
			out = append(out, p.ID)
		}
	}
	return out
}

// Get returns the effective value for a config key (env > file > default).
func (c *Config) Get(key string) (any, error) {
	if !c.viper.IsSet(key) && c.viper.Get(key) == nil {
		return nil, cogerr.Ef("config.Get", cogerr.NotFound, "unknown key %q", key)
	}
	return c.viper.Get(key), nil
}

// Set persists a single key to config.yaml (creating it on first use).
func (c *Config) Set(key string, value string) error {
	const op = "config.Set"
	if _, err := c.Get(key); err != nil {
		return cogerr.Ef(op, cogerr.NotFound, "unknown key %q", key)
	}
	c.viper.Set(key, value)
	if err := c.paths.EnsureDirs(); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	if err := c.viper.WriteConfigAs(c.paths.ConfigFile()); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	return nil
}
