package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mkrcode/internal/config"
)

// isolateConfig points the user config directory at a temporary location so
// tests never touch the developer's real settings.
func isolateConfig(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "local"))
	t.Setenv("APPDATA", filepath.Join(home, "roaming"))
	path, err := userConfigFile()
	if err != nil {
		t.Fatalf("userConfigFile: %v", err)
	}
	return path
}

func readBack(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("written config is not valid JSON: %v\n%s", err, b)
	}
	return m
}

func TestConfigSetWritesTypedValues(t *testing.T) {
	path := isolateConfig(t)

	for _, tc := range []struct{ key, value string }{
		{"endpoint", "http://gpu-01:8000"},
		{"max_model_len", "262144"},
		{"max_turns", "25"},
		{"redact", "false"},
		{"mode", "plan"},
		{"model", "Qwen/Qwen3-Coder-30B-A3B"},
	} {
		if err := configSet(tc.key, tc.value); err != nil {
			t.Fatalf("set %s: %v", tc.key, err)
		}
	}

	m := readBack(t, path)
	// Types must match what the loader expects, or the next run fails to
	// parse its own configuration.
	if m["endpoint"] != "http://gpu-01:8000" {
		t.Errorf("endpoint = %#v", m["endpoint"])
	}
	if n, ok := m["max_model_len"].(float64); !ok || n != 262144 {
		t.Errorf("max_model_len = %#v, want the number 262144", m["max_model_len"])
	}
	if b, ok := m["redact"].(bool); !ok || b {
		t.Errorf("redact = %#v, want the boolean false", m["redact"])
	}

	// The file the loader reads must round-trip cleanly.
	ws := t.TempDir()
	cfg, err := config.Load(ws)
	if err != nil {
		t.Fatalf("the written config does not load: %v", err)
	}
	if cfg.Endpoint != "http://gpu-01:8000" || cfg.MaxModelLen != 262144 || cfg.Redact {
		t.Errorf("loaded config does not match what was written: %+v", cfg)
	}
}

// Setting one key must not discard the others, or each command silently
// resets the workstation's configuration.
func TestConfigSetPreservesOtherKeys(t *testing.T) {
	path := isolateConfig(t)

	if err := configSet("endpoint", "http://gpu-01:8000"); err != nil {
		t.Fatal(err)
	}
	if err := configSet("mode", "auto"); err != nil {
		t.Fatal(err)
	}

	m := readBack(t, path)
	if m["endpoint"] != "http://gpu-01:8000" {
		t.Errorf("the endpoint was lost when mode was set: %#v", m)
	}
	if m["mode"] != "auto" {
		t.Errorf("mode = %#v", m["mode"])
	}
}

// A bad value must be refused at the moment it is typed, not at the next
// session when nobody is watching.
func TestConfigSetRejectsInvalidValues(t *testing.T) {
	isolateConfig(t)

	for _, tc := range []struct{ key, value, wantIn string }{
		{"endpoint", "gpu-01:8000", "scheme"},
		{"endpoint", "ftp://gpu-01", "scheme"},
		{"mode", "yolo", "unknown mode"},
		{"adapter", "telepathy", "unknown adapter"},
		{"max_turns", "0", "positive"},
		{"max_turns", "abc", "positive"},
		{"max_model_len", "-5", "positive"},
		{"redact", "maybe", "true or false"},
		{"nonexistent_key", "x", "unknown key"},
	} {
		err := configSet(tc.key, tc.value)
		if err == nil {
			t.Errorf("set %s=%q was accepted, want a refusal", tc.key, tc.value)
			continue
		}
		if !strings.Contains(strings.ToLower(err.Error()), tc.wantIn) {
			t.Errorf("set %s=%q error = %q, want it to mention %q", tc.key, tc.value, err, tc.wantIn)
		}
	}
}

func TestConfigSetNormalisesEndpointTrailingSlash(t *testing.T) {
	path := isolateConfig(t)
	if err := configSet("endpoint", "http://gpu-01:8000/"); err != nil {
		t.Fatal(err)
	}
	if got := readBack(t, path)["endpoint"]; got != "http://gpu-01:8000" {
		t.Errorf("endpoint = %#v, want the trailing slash removed", got)
	}
}

func TestConfigUnset(t *testing.T) {
	path := isolateConfig(t)
	configSet("endpoint", "http://gpu-01:8000")
	configSet("mode", "auto")

	if err := configUnset("mode"); err != nil {
		t.Fatalf("unset: %v", err)
	}
	m := readBack(t, path)
	if _, ok := m["mode"]; ok {
		t.Error("mode was not removed")
	}
	if m["endpoint"] != "http://gpu-01:8000" {
		t.Error("unset removed an unrelated key")
	}

	if err := configUnset("mode"); err == nil {
		t.Error("unsetting an absent key should report that it is not set")
	}
}

// A corrupt config file must be reported, not silently replaced: quietly
// discarding an operator's settings is worse than refusing to write.
func TestConfigSetRefusesToClobberCorruptFile(t *testing.T) {
	path := isolateConfig(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"endpoint": `), 0o600); err != nil {
		t.Fatal(err)
	}

	err := configSet("mode", "auto")
	if err == nil {
		t.Fatal("expected an error for a corrupt config file")
	}
	if !strings.Contains(err.Error(), "valid JSON") {
		t.Errorf("error = %q, want it to explain the file is malformed", err)
	}
}

func TestConfigWriteIsOwnerOnly(t *testing.T) {
	path := isolateConfig(t)
	if err := configSet("endpoint", "http://gpu-01:8000"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if isWindows() {
		return // covered by internal/secureio's DACL tests
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("config mode is %o; it may contain an API key and must not be group or world readable", perm)
	}
}

func TestConfigSetLeavesNoTempFile(t *testing.T) {
	path := isolateConfig(t)
	if err := configSet("endpoint", "http://gpu-01:8000"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("a temporary file was left behind: %s", e.Name())
		}
	}
}

func TestRunConfigRejectsUnknownAction(t *testing.T) {
	isolateConfig(t)
	cfg := config.Default()
	cfg.Workspace = t.TempDir()
	if err := runConfig(cfg, []string{"frobnicate"}); err == nil {
		t.Error("an unknown action should be refused")
	}
	if err := runConfig(cfg, []string{"set", "endpoint"}); err == nil {
		t.Error("set with no value should report the usage")
	}
}

// The subcommand must be recognised whether it precedes or follows the
// flags. Getting this wrong made "mkr -C /repo probe" silently start a chat
// whose prompt was the word "probe".
func TestIsSubcommand(t *testing.T) {
	for _, s := range []string{"probe", "version", "help", "audit", "selftest", "config"} {
		if !isSubcommand(s) {
			t.Errorf("isSubcommand(%q) = false", s)
		}
	}
	for _, s := range []string{"", "fix the login bug", "probe the database", "configure"} {
		if isSubcommand(s) {
			t.Errorf("isSubcommand(%q) = true, want false; ordinary prompts must not be mistaken for commands", s)
		}
	}
}

func TestMaskSecretDoesNotRevealTheKey(t *testing.T) {
	if got := maskSecret(""); got != "(not set)" {
		t.Errorf("maskSecret(\"\") = %q", got)
	}
	got := maskSecret("sk-supersecretvalue")
	if strings.Contains(got, "supersecret") {
		t.Errorf("maskSecret leaked the key: %q", got)
	}
}
