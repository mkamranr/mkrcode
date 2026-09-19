package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/mkamranr/mkrcode/internal/audit"
	"github.com/mkamranr/mkrcode/internal/config"
	"github.com/mkamranr/mkrcode/internal/fsjail"
	"github.com/mkamranr/mkrcode/internal/provider"
	"github.com/mkamranr/mkrcode/internal/redact"
	"github.com/mkamranr/mkrcode/internal/skills"
	"github.com/mkamranr/mkrcode/internal/tools"
)

// selftest validates that this binary works in the environment it has been
// carried into.
//
// It exists because the client is delivered on removable media to machines
// the developers cannot reach. Rather than asking an operator to describe a
// failure over the phone, they run one command and carry back a report.
// Every check exercises the real production code path, not a simulation of
// it, so a pass here means the corresponding feature actually works.

// checkResult is the outcome of one check.
type checkResult struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	// Fatal marks a failure that makes the tool unusable, as opposed to one
	// that degrades it.
	Fatal bool `json:"fatal"`
}

// check is one validation step. Returning an error marks it failed.
type check struct {
	name  string
	fatal bool
	run   func(ctx context.Context, cfg config.Config) (string, error)
}

// runSelfTest executes the checks and prints a report.
func runSelfTest(ctx context.Context, cfg config.Config, skipEndpoint bool, asJSON bool) error {
	checks := []check{
		{"platform", true, checkPlatform},
		{"config paths", true, checkConfigPaths},
		{"shell", true, checkShell},
		{"terminal", false, checkTerminal},
		{"workspace jail", true, checkJail},
		{"audit log", true, checkAudit},
		{"redaction", true, checkRedaction},
		{"skills", false, checkSkills},
	}
	if !skipEndpoint {
		checks = append(checks, check{"endpoint", true, checkEndpoint})
	}

	results := make([]checkResult, 0, len(checks))
	failed, fatalFailed := 0, 0

	for _, c := range checks {
		detail, err := c.run(ctx, cfg)
		r := checkResult{Name: c.name, OK: err == nil, Detail: detail, Fatal: c.fatal}
		if err != nil {
			r.Detail = err.Error()
			failed++
			if c.fatal {
				fatalFailed++
			}
		}
		results = append(results, r)
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(map[string]any{
			"version": version,
			"time":    time.Now().UTC().Format(time.RFC3339),
			"checks":  results,
			"passed":  len(results) - failed,
			"failed":  failed,
		}); err != nil {
			return err
		}
	} else {
		fmt.Printf("mkr %s self-test\n\n", version)
		for _, r := range results {
			mark := "PASS"
			if !r.OK {
				mark = "FAIL"
				if !r.Fatal {
					mark = "WARN"
				}
			}
			fmt.Printf("  [%s] %-16s %s\n", mark, r.Name, r.Detail)
		}
		fmt.Printf("\n%d of %d checks passed\n", len(results)-failed, len(results))
	}

	if fatalFailed > 0 {
		return fmt.Errorf("%d required check(s) failed", fatalFailed)
	}
	return nil
}

func checkPlatform(context.Context, config.Config) (string, error) {
	return fmt.Sprintf("%s/%s, go %s", runtime.GOOS, runtime.GOARCH, runtime.Version()), nil
}

// checkConfigPaths confirms the per-user directories resolve and can be
// written. On Windows this is where %LOCALAPPDATA% is exercised.
func checkConfigPaths(_ context.Context, cfg config.Config) (string, error) {
	cfgDir, err := config.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("cannot resolve the user config directory: %w", err)
	}
	dataDir, err := config.UserDataDir()
	if err != nil {
		return "", fmt.Errorf("cannot resolve the user data directory: %w", err)
	}
	// Writability is what actually matters; a resolvable but unwritable
	// directory fails later, during a session, which is worse.
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", fmt.Errorf("cannot create %s: %w", dataDir, err)
	}
	probe := filepath.Join(dataDir, ".write-probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		return "", fmt.Errorf("cannot write to %s: %w", dataDir, err)
	}
	os.Remove(probe)
	return fmt.Sprintf("config %s, data %s", cfgDir, dataDir), nil
}

// checkShell resolves the exec shell and actually runs a command through it.
// Detection alone is not enough: on a locked-down machine pwsh may resolve
// but refuse to execute.
func checkShell(ctx context.Context, cfg config.Config) (string, error) {
	shell := cfg.Shell
	if shell == "" {
		shell = tools.DetectShell()
	}

	dir, err := os.MkdirTemp("", "mkr-selftest-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)

	jail, err := fsjail.New(dir)
	if err != nil {
		return "", err
	}
	reg := tools.Standard(tools.Options{Jail: jail, Shell: shell, ExecTimeoutSeconds: 30})
	exec, ok := reg.Get("exec")
	if !ok {
		return "", errors.New("the exec tool is not registered")
	}

	const sentinel = "mkr-selftest-ok"
	cmd := "echo " + sentinel
	if runtime.GOOS == "windows" {
		cmd = "Write-Output " + sentinel
	}
	args, _ := json.Marshal(map[string]string{"command": cmd})

	res, err := exec.Run(ctx, args)
	if err != nil {
		return "", fmt.Errorf("running a command through %s failed: %w", shell, err)
	}
	if !strings.Contains(res.Content, sentinel) {
		return "", fmt.Errorf("%s ran but produced unexpected output: %s", shell, firstLine(res.Content))
	}
	return shell + " runs commands", nil
}

// checkTerminal reports whether output will be styled. This is advisory: a
// console without VT support is ugly, not broken.
func checkTerminal(context.Context, config.Config) (string, error) {
	info, err := os.Stdout.Stat()
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		return "output is redirected; colour disabled", nil
	}
	if os.Getenv("NO_COLOR") != "" {
		return "NO_COLOR is set; colour disabled", nil
	}
	return "interactive terminal", nil
}

// checkJail verifies the security boundary refuses real escape attempts on
// this platform. The Windows cases matter most, because their grammar is
// what a lexical prefix check misses.
func checkJail(context.Context, config.Config) (string, error) {
	dir, err := os.MkdirTemp("", "mkr-selftest-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)

	jail, err := fsjail.New(dir)
	if err != nil {
		return "", fmt.Errorf("cannot create a workspace jail: %w", err)
	}

	escapes := []string{
		"..",
		"../escape.txt",
		"a/../../escape.txt",
	}
	if runtime.GOOS == "windows" {
		escapes = append(escapes,
			`C:\Windows\System32\drivers\etc\hosts`,
			`\\?\C:\Windows\win.ini`,
			`\\server\share\file`,
			`C:notes.txt`,
			`CON`,
			`notes.txt:stream`,
			`evil.txt.`,
		)
	} else {
		escapes = append(escapes, "/etc/passwd")
	}

	for _, p := range escapes {
		if _, err := jail.Resolve(p); err == nil {
			return "", fmt.Errorf("SECURITY: the workspace jail accepted %q, which is outside the workspace", p)
		}
	}
	// A legitimate path must still work, or the jail is uselessly strict.
	if _, err := jail.Resolve("src/main.go"); err != nil {
		return "", fmt.Errorf("the jail refused a legitimate path: %w", err)
	}
	return fmt.Sprintf("%d escape attempts refused", len(escapes)), nil
}

// checkAudit writes a short chain and verifies it, exercising the same code
// a real session uses.
func checkAudit(context.Context, config.Config) (string, error) {
	dir, err := os.MkdirTemp("", "mkr-selftest-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, "audit.jsonl")
	log, err := audit.Open(audit.Options{Path: path, Session: "selftest", User: "selftest", Host: "selftest"})
	if err != nil {
		return "", fmt.Errorf("cannot open an audit log: %w", err)
	}
	for i := 0; i < 3; i++ {
		if err := log.Log(audit.Record{Event: audit.EventToolCall, Tool: "selftest"}); err != nil {
			log.Close()
			return "", fmt.Errorf("cannot append to the audit log: %w", err)
		}
	}
	if err := log.Close(); err != nil {
		return "", err
	}

	res, err := audit.Verify(path)
	if err != nil {
		return "", err
	}
	if !res.OK {
		return "", fmt.Errorf("a freshly written audit log failed verification: %s", res.Problem)
	}

	// Tampering must be detected, otherwise the log proves nothing.
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(raw), "selftest", "tampered", 1)), 0o600); err != nil {
		return "", err
	}
	if res, _ := audit.Verify(path); res.OK {
		return "", errors.New("SECURITY: a tampered audit log passed verification")
	}
	return "chain written, verified, and tampering detected", nil
}

// checkRedaction confirms a synthetic credential is scrubbed.
func checkRedaction(_ context.Context, cfg config.Config) (string, error) {
	if !cfg.Redact {
		return "disabled by configuration", nil
	}
	const secret = "AKIAIOSFODNN7EXAMPLE"
	out, findings := redact.New(true).Redact("AWS_ACCESS_KEY_ID=" + secret)
	if strings.Contains(out, secret) {
		return "", errors.New("SECURITY: a synthetic credential was not redacted")
	}
	if len(findings) == 0 {
		return "", errors.New("redaction reported no findings for a known credential")
	}
	return "synthetic credential scrubbed", nil
}

// checkSkills reports what instruction packs were discovered, and surfaces
// any that are malformed. Advisory: a broken skill degrades the session, it
// does not prevent one.
func checkSkills(_ context.Context, cfg config.Config) (string, error) {
	userCfgDir, _ := config.UserConfigDir()
	set, problems := skills.Discover(cfg.Workspace, userCfgDir)
	if len(problems) > 0 {
		return "", fmt.Errorf("%d skill(s) could not be loaded: %v", len(problems), problems[0])
	}
	if set.Len() == 0 {
		return "none installed", nil
	}
	return fmt.Sprintf("%d discovered: %s", set.Len(), strings.Join(set.Names(), ", ")), nil
}

// checkEndpoint runs the full capability probe against the configured server.
func checkEndpoint(ctx context.Context, cfg config.Config) (string, error) {
	client, err := newProvider(cfg)
	if err != nil {
		return "", err
	}
	caps, err := provider.Probe(ctx, client, string(cfg.Adapter), cfg.Model)
	if err != nil {
		return "", fmt.Errorf("%s unreachable: %w", cfg.BaseURL(), err)
	}
	detail := fmt.Sprintf("%s, %s adapter", caps.Model, caps.Adapter)
	if caps.MaxModelLen > 0 {
		detail += fmt.Sprintf(", %d token context", caps.MaxModelLen)
	}
	return detail, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120] + "..."
	}
	return s
}
