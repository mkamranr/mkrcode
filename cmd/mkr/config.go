package main

import (
	"crypto/x509"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"mkrcode/internal/config"
	"mkrcode/internal/secureio"
)

// The config subcommand exists for rollout. Asking every developer to hand-
// edit JSON under %APPDATA% is how a fleet ends up with a dozen slightly
// different configurations, and a mistyped endpoint produces a connection
// error that looks like a firewall problem.
//
// It also answers the most common question during deployment: not "what is
// the setting" but "where is this value coming from", which matters once a
// value can arrive from a file, the environment, or a flag.

// settableKeys are the fields `mkr config set` accepts, with a short
// description used by the help output.
var settableKeys = map[string]string{
	"endpoint":      "vLLM base URL, for example http://gpu-01:8000",
	"model":         "served model ID; empty means whatever /v1/models reports",
	"api_key":       "bearer token, if vLLM was started with --api-key",
	"ca_cert":       "PEM file of certificate authorities to trust, for an https endpoint using internal PKI",
	"mode":          "starting permission mode: plan, approve or auto",
	"adapter":       "tool-calling strategy: auto, native or xml",
	"shell":         "executable used by the exec tool",
	"max_model_len": "context window to assume; lower it to keep sessions fast",
	"max_turns":     "maximum tool-use turns per request",
	"redact":        "true or false; disabling is audited",
	"audit_path":    "audit log destination",
}

// runConfig implements: mkr config [show | path | set KEY VALUE | unset KEY]
func runConfig(cfg config.Config, args []string) error {
	action := "show"
	if len(args) > 0 {
		action = strings.ToLower(args[0])
		args = args[1:]
	}

	switch action {
	case "show":
		return configShow(cfg)
	case "path":
		return configPath()
	case "set":
		if len(args) < 2 {
			return fmt.Errorf("usage: mkr config set KEY VALUE\n\n%s", settableKeyHelp())
		}
		return configSet(args[0], strings.Join(args[1:], " "))
	case "unset":
		if len(args) != 1 {
			return fmt.Errorf("usage: mkr config unset KEY\n\n%s", settableKeyHelp())
		}
		return configUnset(args[0])
	default:
		return fmt.Errorf("unknown config action %q; want show, path, set or unset", action)
	}
}

func settableKeyHelp() string {
	keys := make([]string, 0, len(settableKeys))
	for k := range settableKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString("settable keys:\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "  %-14s %s\n", k, settableKeys[k])
	}
	return strings.TrimRight(b.String(), "\n")
}

// configShow prints the resolved configuration and, for the values most
// often wrong during deployment, where each one came from.
func configShow(cfg config.Config) error {
	userFile, err := userConfigFile()
	if err != nil {
		return err
	}
	projectFile := filepath.Join(cfg.Workspace, ".mkr", "config.json")

	fmt.Println("resolved configuration")
	fmt.Println()
	fmt.Printf("  endpoint       %s   %s\n", cfg.Endpoint, originOf("endpoint", "MKR_ENDPOINT", userFile, projectFile))
	fmt.Printf("  model          %s\n", orDefault(cfg.Model, "(from /v1/models)"))
	fmt.Printf("  mode           %s   %s\n", cfg.Mode, originOf("mode", "MKR_MODE", userFile, projectFile))
	fmt.Printf("  adapter        %s\n", cfg.Adapter)
	fmt.Printf("  shell          %s\n", cfg.Shell)
	fmt.Printf("  max_model_len  %s\n", orDefault(itoa(cfg.MaxModelLen), "(from the endpoint)"))
	fmt.Printf("  max_turns      %d\n", cfg.MaxTurns)
	fmt.Printf("  redact         %t\n", cfg.Redact)
	fmt.Printf("  api_key        %s\n", maskSecret(cfg.APIKey))
	fmt.Printf("  ca_cert        %s\n", orDefault(cfg.CACert, "(system trust store)"))
	fmt.Println()
	fmt.Printf("  workspace      %s\n", cfg.Workspace)
	fmt.Printf("  audit log      %s\n", cfg.AuditPath)
	fmt.Printf("  sessions       %s\n", cfg.SessionDir)
	fmt.Printf("  rules          %s\n", cfg.RulesPath)
	fmt.Println()
	fmt.Println("  files are read in order: user config, then project config,")
	fmt.Println("  then MKR_* environment variables, then command-line flags")
	return nil
}

// originOf reports which layer supplied a value, which is the question that
// actually gets asked when a setting is not what someone expects.
func originOf(key, envVar, userFile, projectFile string) string {
	if _, ok := os.LookupEnv(envVar); ok {
		return "(from " + envVar + ")"
	}
	if fileHasKey(projectFile, key) {
		return "(from the project config)"
	}
	if fileHasKey(userFile, key) {
		return "(from the user config)"
	}
	return "(default)"
}

// fileHasKey reports whether a config file sets key.
func fileHasKey(path, key string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil {
		return false
	}
	_, ok := m[key]
	return ok
}

func configPath() error {
	userFile, err := userConfigFile()
	if err != nil {
		return err
	}
	dataDir, err := config.UserDataDir()
	if err != nil {
		return err
	}
	fmt.Printf("user config    %s%s\n", userFile, existsNote(userFile))
	fmt.Printf("project config %s\n", filepath.Join(".mkr", "config.json"))
	fmt.Printf("data directory %s\n", dataDir)
	return nil
}

func existsNote(path string) string {
	if _, err := os.Stat(path); err == nil {
		return ""
	}
	return "   (not created yet)"
}

// configSet writes one key into the user configuration file, preserving
// every other key already present.
func configSet(key, value string) error {
	key = strings.ToLower(strings.TrimSpace(key))
	if _, ok := settableKeys[key]; !ok {
		return fmt.Errorf("unknown key %q\n\n%s", key, settableKeyHelp())
	}

	parsed, err := parseValue(key, value)
	if err != nil {
		return err
	}

	path, err := userConfigFile()
	if err != nil {
		return err
	}
	existing, err := readConfigMap(path)
	if err != nil {
		return err
	}
	existing[key] = parsed

	if err := writeConfigMap(path, existing); err != nil {
		return err
	}
	fmt.Printf("set %s in %s\n", key, path)

	// A mistyped endpoint is the most common deployment error, and it
	// surfaces much later as a confusing connection failure. Say so now.
	if key == "endpoint" {
		fmt.Println("\nverify it with:  mkr probe")
	}
	return nil
}

func configUnset(key string) error {
	key = strings.ToLower(strings.TrimSpace(key))
	path, err := userConfigFile()
	if err != nil {
		return err
	}
	existing, err := readConfigMap(path)
	if err != nil {
		return err
	}
	if _, ok := existing[key]; !ok {
		return fmt.Errorf("%q is not set in %s", key, path)
	}
	delete(existing, key)
	if err := writeConfigMap(path, existing); err != nil {
		return err
	}
	fmt.Printf("unset %s in %s\n", key, path)
	return nil
}

// parseValue converts a command-line string to the JSON type the field
// expects, so the file stays valid for the loader.
func parseValue(key, value string) (any, error) {
	value = strings.TrimSpace(value)
	switch key {
	case "redact":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return nil, fmt.Errorf("%s must be true or false, got %q", key, value)
		}
		return b, nil
	case "max_model_len", "max_turns":
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("%s must be a positive integer, got %q", key, value)
		}
		return n, nil
	case "mode":
		m, err := config.ParseMode(value)
		if err != nil {
			return nil, err
		}
		return string(m), nil
	case "adapter":
		a, err := config.ParseAdapter(value)
		if err != nil {
			return nil, err
		}
		return string(a), nil
	case "ca_cert":
		// Check the file now: a path typo would otherwise surface as a TLS
		// failure that looks like a certificate problem.
		abs, err := filepath.Abs(value)
		if err != nil {
			return nil, fmt.Errorf("ca_cert %q: %w", value, err)
		}
		pem, err := os.ReadFile(abs)
		if err != nil {
			return nil, fmt.Errorf("ca_cert %q: %w", abs, err)
		}
		if !x509.NewCertPool().AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca_cert %q contains no usable PEM certificates", abs)
		}
		return abs, nil
	case "endpoint":
		// Validate now rather than at the next session, when the person who
		// typed it is no longer watching.
		probe := config.Default()
		probe.Endpoint = value
		probe.Workspace = os.TempDir()
		if err := probe.Validate(); err != nil {
			return nil, err
		}
		return strings.TrimRight(value, "/"), nil
	default:
		return value, nil
	}
}

func readConfigMap(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	return m, nil
}

// writeConfigMap writes the file atomically, so an interrupted write cannot
// leave a configuration that no longer parses.
func writeConfigMap(path string, m map[string]any) error {
	if err := secureio.MkdirAllPrivate(filepath.Dir(path)); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')

	tmp := path + ".tmp"
	f, err := secureio.Create(tmp)
	if err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

func userConfigFile() (string, error) {
	dir, err := config.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// maskSecret renders an API key without disclosing it.
func maskSecret(s string) string {
	if s == "" {
		return "(not set)"
	}
	return fmt.Sprintf("(set, %d characters)", len(s))
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func itoa(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}
