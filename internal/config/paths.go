package config

import (
	"os"
	"path/filepath"

	"github.com/adrg/xdg"
)

// Paths holds every filesystem location Cognosis uses, resolved per the XDG
// Base Directory Specification. Precedence is
// env-var-first, adrg/xdg platform default second — nothing else.
type Paths struct {
	ConfigDir string // $XDG_CONFIG_HOME/cognosis
	DataDir   string // $XDG_DATA_HOME/cognosis
	StateDir  string // $XDG_STATE_HOME/cognosis
	CacheDir  string // $XDG_CACHE_HOME/cognosis — reserved, unused in v1
}

// ResolvePaths reads the XDG env vars at call time (not package init) so env
// precedence is testable and respects late changes.
func ResolvePaths() Paths {
	return Paths{
		ConfigDir: filepath.Join(envOr("XDG_CONFIG_HOME", xdg.ConfigHome), "cognosis"),
		DataDir:   filepath.Join(envOr("XDG_DATA_HOME", xdg.DataHome), "cognosis"),
		StateDir:  filepath.Join(envOr("XDG_STATE_HOME", xdg.StateHome), "cognosis"),
		CacheDir:  filepath.Join(envOr("XDG_CACHE_HOME", xdg.CacheHome), "cognosis"),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func (p Paths) ConfigFile() string  { return filepath.Join(p.ConfigDir, "config.yaml") }
func (p Paths) KBDir() string       { return filepath.Join(p.DataDir, "kb") }
func (p Paths) PersonasDir() string { return filepath.Join(p.DataDir, "personas") }
func (p Paths) LockFile() string    { return filepath.Join(p.StateDir, "daemon.lock") }
func (p Paths) TokenFile() string   { return filepath.Join(p.StateDir, "local-token") }

// EnsureDirs creates all four XDG-scoped directories plus the vault dir; first
// daemon start calls this rather than assuming they pre-exist.
func (p Paths) EnsureDirs() error {
	for _, d := range []string{p.ConfigDir, p.DataDir, p.StateDir, p.CacheDir, p.KBDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}
