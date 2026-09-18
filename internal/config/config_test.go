package config

import (
	"path/filepath"
	"testing"
	"time"
)

func TestParseMode(t *testing.T) {
	tests := []struct {
		in      string
		want    Mode
		wantErr bool
	}{
		{"plan", ModePlan, false},
		{"approve", ModeApprove, false},
		{"auto", ModeAuto, false},
		{"AUTO", ModeAuto, false},
		{"  plan  ", ModePlan, false},
		{"yolo", "", true},
		{"", "", true},
	}
	for _, tt := range tests {
		got, err := ParseMode(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseMode(%q) = %q, want error", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseMode(%q): unexpected error: %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseMode(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestModeCapabilities pins the security-relevant semantics of each mode.
// plan must never be mutating; approve must always prompt.
func TestModeCapabilities(t *testing.T) {
	if ModePlan.Mutating() {
		t.Error("plan mode must not permit mutating tools")
	}
	if ModePlan.Prompts() {
		t.Error("plan mode should refuse outright, not prompt")
	}
	if !ModeApprove.Mutating() || !ModeApprove.Prompts() {
		t.Error("approve mode must permit mutating tools behind a prompt")
	}
	if !ModeAuto.Mutating() {
		t.Error("auto mode must permit mutating tools")
	}
	if ModeAuto.Prompts() {
		t.Error("auto mode must not prompt")
	}
}

func TestDurationRoundTrip(t *testing.T) {
	var d Duration
	if err := d.UnmarshalJSON([]byte(`"90s"`)); err != nil {
		t.Fatalf("unmarshal string: %v", err)
	}
	if d.D() != 90*time.Second {
		t.Errorf("got %v, want 90s", d.D())
	}
	if err := d.UnmarshalJSON([]byte(`45`)); err != nil {
		t.Fatalf("unmarshal number: %v", err)
	}
	if d.D() != 45*time.Second {
		t.Errorf("got %v, want 45s", d.D())
	}
	if err := d.UnmarshalJSON([]byte(`"not-a-duration"`)); err == nil {
		t.Error("expected an error for a malformed duration")
	}
}

func TestLoadLayersFileThenEnv(t *testing.T) {
	ws := t.TempDir()
	mkdirWrite(t, filepath.Join(ws, ".mkr", "config.json"), `{
		"endpoint": "http://gpu-01:8000",
		"model": "Qwen/Qwen3-Coder-30B-A3B",
		"mode": "plan",
		"max_turns": 7,
		"exec_timeout": "30s"
	}`)

	// Isolate from the developer's real config directory.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	cfg, err := Load(ws)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Endpoint != "http://gpu-01:8000" {
		t.Errorf("endpoint = %q, want the workspace file's value", cfg.Endpoint)
	}
	if cfg.Mode != ModePlan {
		t.Errorf("mode = %q, want plan", cfg.Mode)
	}
	if cfg.MaxTurns != 7 {
		t.Errorf("max_turns = %d, want 7", cfg.MaxTurns)
	}
	if cfg.ExecTimeout.D() != 30*time.Second {
		t.Errorf("exec_timeout = %v, want 30s", cfg.ExecTimeout.D())
	}
	// Keys absent from the file must retain their defaults.
	if cfg.Temperature != Default().Temperature {
		t.Errorf("temperature = %v, want the default %v", cfg.Temperature, Default().Temperature)
	}
	if !cfg.Redact {
		t.Error("redact must default to true")
	}

	// Environment overrides the file.
	t.Setenv("MKR_MODE", "auto")
	t.Setenv("MKR_ENDPOINT", "http://gpu-02:8000")
	cfg, err = Load(ws)
	if err != nil {
		t.Fatalf("Load with env: %v", err)
	}
	if cfg.Mode != ModeAuto {
		t.Errorf("mode = %q, want env override auto", cfg.Mode)
	}
	if cfg.Endpoint != "http://gpu-02:8000" {
		t.Errorf("endpoint = %q, want env override", cfg.Endpoint)
	}
}

func TestLoadRejectsMalformedConfig(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	mkdirWrite(t, filepath.Join(ws, ".mkr", "config.json"), `{"endpoint": `)
	if _, err := Load(ws); err == nil {
		t.Fatal("expected an error for malformed JSON, got nil")
	}
}

func TestLoadRejectsBadEnvValues(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MKR_MAX_TURNS", "-3")
	if _, err := Load(ws); err == nil {
		t.Fatal("expected an error for a negative MKR_MAX_TURNS")
	}
}

func TestValidate(t *testing.T) {
	base := func() Config {
		c := Default()
		c.Workspace = mustAbs(t, "ws")
		c.Endpoint = "http://gpu-01:8000"
		return c
	}
	b := base()
	if err := b.Validate(); err != nil {
		t.Fatalf("baseline config should validate: %v", err)
	}

	tests := []struct {
		name  string
		mutar func(*Config)
	}{
		{"empty endpoint", func(c *Config) { c.Endpoint = "" }},
		{"bad scheme", func(c *Config) { c.Endpoint = "ftp://gpu-01:8000" }},
		{"no host", func(c *Config) { c.Endpoint = "http://" }},
		{"bad mode", func(c *Config) { c.Mode = "yolo" }},
		{"bad adapter", func(c *Config) { c.Adapter = "telepathy" }},
		{"zero turns", func(c *Config) { c.MaxTurns = 0 }},
		{"zero request timeout", func(c *Config) { c.RequestTimeout = 0 }},
		{"zero exec timeout", func(c *Config) { c.ExecTimeout = 0 }},
		{"empty workspace", func(c *Config) { c.Workspace = "" }},
		{"relative workspace", func(c *Config) { c.Workspace = "./ws" }},
	}
	for _, tt := range tests {
		c := base()
		tt.mutar(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: expected a validation error, got nil", tt.name)
		}
	}
}

func TestHostDefaultsPortFromScheme(t *testing.T) {
	tests := []struct {
		endpoint string
		want     string
	}{
		{"http://gpu-01:8000", "gpu-01:8000"},
		{"http://gpu-01", "gpu-01:80"},
		{"https://gpu-01", "gpu-01:443"},
		{"https://gpu-01:8443/", "gpu-01:8443"},
	}
	for _, tt := range tests {
		c := Default()
		c.Endpoint = tt.endpoint
		got, err := c.Host()
		if err != nil {
			t.Errorf("Host(%q): %v", tt.endpoint, err)
			continue
		}
		if got != tt.want {
			t.Errorf("Host(%q) = %q, want %q", tt.endpoint, got, tt.want)
		}
	}
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatalf("abs %q: %v", p, err)
	}
	return a
}
