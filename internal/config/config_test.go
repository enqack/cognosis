package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// isolate points every XDG dir at a temp root so tests never touch the real
// user directories.
func isolate(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, k := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(k, filepath.Join(root, k))
	}
	return root
}

func TestXDGEnvPrecedence(t *testing.T) {
	root := isolate(t)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	got, err := cfg.Get("kb_path")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "XDG_DATA_HOME", "cognosis", "kb")
	if got != want {
		t.Fatalf("kb_path = %q, want env-derived %q", got, want)
	}
}

func TestConfigSetGetRoundTrip(t *testing.T) {
	isolate(t)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Set("bind_address", "127.0.0.1:9999"); err != nil {
		t.Fatal(err)
	}

	// A fresh Load must see the persisted value — proves it hit config.yaml,
	// not just in-memory viper state.
	cfg2, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	got, err := cfg2.Get("bind_address")
	if err != nil {
		t.Fatal(err)
	}
	if got != "127.0.0.1:9999" {
		t.Fatalf("bind_address after round-trip = %q", got)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	isolate(t)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Set("dsn", "postgres://from-file"); err != nil {
		t.Fatal(err)
	}

	t.Setenv("COGNOSIS_DSN", "postgres://from-env")
	cfg2, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.DSN != "postgres://from-env" {
		t.Fatalf("DSN = %q, want env to win over file", cfg2.DSN)
	}
}

func TestUnknownKeyRejected(t *testing.T) {
	isolate(t)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.Get("no_such_key"); err == nil {
		t.Fatal("Get(no_such_key) should fail")
	}
	if err := cfg.Set("no_such_key", "x"); err == nil {
		t.Fatal("Set(no_such_key) should fail")
	}
}

func TestDefaults(t *testing.T) {
	isolate(t)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReconcileSweepInterval != time.Hour {
		t.Fatalf("sweep interval default = %v, want 1h", cfg.ReconcileSweepInterval)
	}
	if cfg.Embedding.Provider != "ollama" {
		t.Fatalf("embedding provider default = %q", cfg.Embedding.Provider)
	}
}

func TestEnsureDirs(t *testing.T) {
	isolate(t)

	p := ResolvePaths()
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{p.ConfigDir, p.DataDir, p.StateDir, p.CacheDir, p.KBDir()} {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			t.Fatalf("dir %s not created: %v", d, err)
		}
	}
}
