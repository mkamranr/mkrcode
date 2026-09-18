// Package config resolves mkr's runtime configuration from layered sources.
//
// Precedence, lowest to highest: built-in defaults, user config file,
// workspace config file, environment variables, command-line flags.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Mode is the permission posture the agent runs under.
type Mode string

const (
	// ModePlan forbids every mutating tool. Reads are still allowed.
	ModePlan Mode = "plan"
	// ModeApprove prompts the operator before each mutating tool call.
	ModeApprove Mode = "approve"
	// ModeAuto runs mutating tools without prompting. Deny rules still apply.
	ModeAuto Mode = "auto"
)

// ValidModes lists every accepted Mode, in order of increasing autonomy.
var ValidModes = []Mode{ModePlan, ModeApprove, ModeAuto}

// ParseMode validates s and returns the corresponding Mode.
func ParseMode(s string) (Mode, error) {
	m := Mode(strings.ToLower(strings.TrimSpace(s)))
	for _, v := range ValidModes {
		if m == v {
			return m, nil
		}
	}
	return "", fmt.Errorf("unknown mode %q (want one of: %s)", s, joinModes(ValidModes))
}

// Mutating reports whether the mode permits tools that change state.
func (m Mode) Mutating() bool { return m == ModeApprove || m == ModeAuto }

// Prompts reports whether the mode asks the operator before mutating.
func (m Mode) Prompts() bool { return m == ModeApprove }

func joinModes(ms []Mode) string {
	parts := make([]string, len(ms))
	for i, m := range ms {
		parts[i] = string(m)
	}
	return strings.Join(parts, ", ")
}

// Adapter selects how tool calls are exchanged with the model.
type Adapter string

const (
	// AdapterAuto probes the endpoint and picks native or xml.
	AdapterAuto Adapter = "auto"
	// AdapterNative uses OpenAI-style tools/tool_calls fields.
	AdapterNative Adapter = "native"
	// AdapterXML injects schemas into the prompt and parses tags from text.
	AdapterXML Adapter = "xml"
)

// ParseAdapter validates s and returns the corresponding Adapter.
func ParseAdapter(s string) (Adapter, error) {
	switch Adapter(strings.ToLower(strings.TrimSpace(s))) {
	case AdapterAuto:
		return AdapterAuto, nil
	case AdapterNative:
		return AdapterNative, nil
	case AdapterXML:
		return AdapterXML, nil
	}
	return "", fmt.Errorf("unknown adapter %q (want one of: auto, native, xml)", s)
}

// Config is the fully resolved runtime configuration.
type Config struct {
	// Endpoint is the base URL of the vLLM OpenAI-compatible server,
	// without the /v1 suffix. It is the only network destination the
	// binary is permitted to reach.
	Endpoint string `json:"endpoint"`
	// APIKey is sent as a bearer token when vLLM is started with --api-key.
	APIKey string `json:"api_key,omitempty"`
	// Model is the served model ID. Empty means "use whatever /v1/models reports".
	Model string `json:"model,omitempty"`

	// Adapter selects the tool-calling strategy.
	Adapter Adapter `json:"adapter"`
	// MaxModelLen caps the prompt budget. Zero means "ask the endpoint".
	MaxModelLen int `json:"max_model_len,omitempty"`
	// Temperature and TopP control sampling.
	Temperature float64 `json:"temperature"`
	TopP        float64 `json:"top_p"`
	// MaxTokens caps a single completion. Zero lets the server decide.
	MaxTokens int `json:"max_tokens,omitempty"`

	// Mode is the starting permission posture.
	Mode Mode `json:"mode"`
	// MaxTurns bounds tool-use iterations within one user request.
	MaxTurns int `json:"max_turns"`
	// RequestTimeout bounds a single completion request.
	RequestTimeout Duration `json:"request_timeout"`
	// ExecTimeout bounds a single shell command.
	ExecTimeout Duration `json:"exec_timeout"`

	// Workspace is the jail root. Every file path must resolve beneath it.
	Workspace string `json:"-"`
	// Shell is the executable used by the exec tool.
	Shell string `json:"shell,omitempty"`

	// Redact enables secret scrubbing of tool output. Default true.
	Redact bool `json:"redact"`
	// AuditPath is the append-only JSONL audit log destination.
	AuditPath string `json:"audit_path,omitempty"`
	// AuditPrompts records full prompt bodies, not just hashes. Default false.
	AuditPrompts bool `json:"audit_prompts"`
	// SessionDir holds transcript files.
	SessionDir string `json:"session_dir,omitempty"`
	// RulesPath points at the allow/deny rules file.
	RulesPath string `json:"rules_path,omitempty"`
}

// Duration is a time.Duration that marshals as a Go duration string.
type Duration time.Duration

// D returns the underlying time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// MarshalJSON renders the duration as a string such as "2m30s".
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON accepts either a duration string or a count of seconds.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		v, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("parse duration %q: %w", s, err)
		}
		*d = Duration(v)
		return nil
	}
	var secs float64
	if err := json.Unmarshal(b, &secs); err != nil {
		return fmt.Errorf("duration must be a string or number, got %s", string(b))
	}
	*d = Duration(time.Duration(secs * float64(time.Second)))
	return nil
}

// Default returns the built-in configuration, before any file, environment
// or flag layering is applied.
func Default() Config {
	return Config{
		Endpoint:       "http://localhost:8000",
		Adapter:        AdapterAuto,
		Temperature:    0.2,
		TopP:           0.95,
		Mode:           ModeApprove,
		MaxTurns:       40,
		RequestTimeout: Duration(5 * time.Minute),
		ExecTimeout:    Duration(2 * time.Minute),
		Redact:         true,
		AuditPrompts:   false,
		Shell:          defaultShell(),
	}
}

// defaultShell picks the exec target for the host platform. The Windows
// fleet standardises on PowerShell 7; detection of the actual binary
// happens in the tools package at startup.
func defaultShell() string {
	if runtime.GOOS == "windows" {
		return "pwsh"
	}
	return "bash"
}

// UserConfigDir returns the per-user configuration directory.
func UserConfigDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config dir: %w", err)
	}
	return filepath.Join(base, "mkr"), nil
}

// UserDataDir returns the per-user data directory used for sessions and
// audit logs. On Windows this is %LOCALAPPDATA%\mkr.
func UserDataDir() (string, error) {
	if runtime.GOOS == "windows" {
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			return filepath.Join(local, "mkr"), nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home dir: %w", err)
	}
	return filepath.Join(home, ".mkr"), nil
}

// Load resolves configuration for the given workspace. Missing config files
// are not an error; malformed ones are.
func Load(workspace string) (Config, error) {
	cfg := Default()

	if dir, err := UserConfigDir(); err == nil {
		if err := mergeFile(&cfg, filepath.Join(dir, "config.json")); err != nil {
			return cfg, err
		}
	}
	if workspace != "" {
		if err := mergeFile(&cfg, filepath.Join(workspace, ".mkr", "config.json")); err != nil {
			return cfg, err
		}
	}
	if err := mergeEnv(&cfg); err != nil {
		return cfg, err
	}

	cfg.Workspace = workspace
	if err := cfg.applyDerivedDefaults(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// mergeFile overlays JSON at path onto cfg. A missing file is ignored.
func mergeFile(cfg *Config, path string) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read config %s: %w", path, err)
	}
	// Decode onto the existing value so absent keys keep their current
	// setting rather than reverting to the zero value.
	if err := json.Unmarshal(b, cfg); err != nil {
		return fmt.Errorf("parse config %s: %w", path, err)
	}
	return nil
}

// envPrefix namespaces every environment override.
const envPrefix = "MKR_"

// mergeEnv overlays MKR_* environment variables onto cfg.
func mergeEnv(cfg *Config) error {
	if v, ok := os.LookupEnv(envPrefix + "ENDPOINT"); ok {
		cfg.Endpoint = v
	}
	if v, ok := os.LookupEnv(envPrefix + "API_KEY"); ok {
		cfg.APIKey = v
	}
	if v, ok := os.LookupEnv(envPrefix + "MODEL"); ok {
		cfg.Model = v
	}
	if v, ok := os.LookupEnv(envPrefix + "SHELL"); ok {
		cfg.Shell = v
	}
	if v, ok := os.LookupEnv(envPrefix + "AUDIT_PATH"); ok {
		cfg.AuditPath = v
	}
	if v, ok := os.LookupEnv(envPrefix + "MODE"); ok {
		m, err := ParseMode(v)
		if err != nil {
			return fmt.Errorf("%sMODE: %w", envPrefix, err)
		}
		cfg.Mode = m
	}
	if v, ok := os.LookupEnv(envPrefix + "ADAPTER"); ok {
		a, err := ParseAdapter(v)
		if err != nil {
			return fmt.Errorf("%sADAPTER: %w", envPrefix, err)
		}
		cfg.Adapter = a
	}
	if v, ok := os.LookupEnv(envPrefix + "MAX_TURNS"); ok {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return fmt.Errorf("%sMAX_TURNS: want a positive integer, got %q", envPrefix, v)
		}
		cfg.MaxTurns = n
	}
	if v, ok := os.LookupEnv(envPrefix + "MAX_MODEL_LEN"); ok {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return fmt.Errorf("%sMAX_MODEL_LEN: want a positive integer, got %q", envPrefix, v)
		}
		cfg.MaxModelLen = n
	}
	if v, ok := os.LookupEnv(envPrefix + "REDACT"); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("%sREDACT: want a boolean, got %q", envPrefix, v)
		}
		cfg.Redact = b
	}
	return nil
}

// applyDerivedDefaults fills in paths that depend on the resolved
// workspace and the host's data directory.
func (c *Config) applyDerivedDefaults() error {
	if c.AuditPath != "" && c.SessionDir != "" && c.RulesPath != "" {
		return nil
	}
	dir, err := UserDataDir()
	if err != nil {
		return err
	}
	if c.AuditPath == "" {
		c.AuditPath = filepath.Join(dir, "audit", "mkr-audit.jsonl")
	}
	if c.SessionDir == "" {
		c.SessionDir = filepath.Join(dir, "sessions")
	}
	if c.RulesPath == "" {
		c.RulesPath = filepath.Join(dir, "rules.json")
	}
	return nil
}

// Validate reports whether the configuration is internally coherent and
// safe to run with. It is called after flag layering, immediately before use.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.Endpoint) == "" {
		return errors.New("endpoint is required: set it in config.json, MKR_ENDPOINT, or --endpoint")
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil {
		return fmt.Errorf("endpoint %q is not a valid URL: %w", c.Endpoint, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("endpoint scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("endpoint %q has no host", c.Endpoint)
	}
	if _, err := ParseMode(string(c.Mode)); err != nil {
		return err
	}
	if _, err := ParseAdapter(string(c.Adapter)); err != nil {
		return err
	}
	if c.MaxTurns <= 0 {
		return fmt.Errorf("max_turns must be positive, got %d", c.MaxTurns)
	}
	if c.RequestTimeout <= 0 {
		return errors.New("request_timeout must be positive")
	}
	if c.ExecTimeout <= 0 {
		return errors.New("exec_timeout must be positive")
	}
	if c.Workspace == "" {
		return errors.New("workspace is required")
	}
	if !filepath.IsAbs(c.Workspace) {
		return fmt.Errorf("workspace must be an absolute path, got %q", c.Workspace)
	}
	return nil
}

// BaseURL returns the endpoint with any trailing slash removed.
func (c *Config) BaseURL() string { return strings.TrimRight(c.Endpoint, "/") }

// Host returns the host:port the binary is allowed to dial, defaulting the
// port from the scheme when the URL omits it.
func (c *Config) Host() (string, error) {
	u, err := url.Parse(c.BaseURL())
	if err != nil {
		return "", fmt.Errorf("parse endpoint: %w", err)
	}
	host := u.Host
	if u.Port() == "" {
		if u.Scheme == "https" {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	return host, nil
}
