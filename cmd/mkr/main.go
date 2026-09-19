// Command mkr is an agentic coding assistant for air-gapped environments.
//
// It drives a locally-hosted vLLM server over the OpenAI-compatible
// protocol. The binary is statically linked and depends on nothing outside
// the Go standard library, so it can be delivered as a single file on
// removable media and run without an installer.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/mkamranr/mkrcode/internal/config"
	"github.com/mkamranr/mkrcode/internal/netguard"
	"github.com/mkamranr/mkrcode/internal/provider"
)

// version is stamped at build time with -ldflags "-X main.version=...".
// Release builds set it; `go install` does not, so buildVersion falls back
// to the module version the toolchain records.
var version = "dev"

// buildVersion returns the version to report.
//
// The audit log records which build performed each action, so a binary that
// reports "dev" when it is actually a tagged release makes a log harder to
// interpret later. When the linker flag is absent, the module version
// embedded by the Go toolchain is authoritative.
func buildVersion() string {
	if version != "dev" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	// A local build from a checkout: report the revision if the toolchain
	// recorded one, which is more useful than a bare "dev".
	var rev, dirty string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if len(s.Value) >= 12 {
				rev = s.Value[:12]
			}
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if rev != "" {
		return "dev-" + rev + dirty
	}
	return version
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "\ninterrupted")
			os.Exit(130)
		}
		fmt.Fprintf(os.Stderr, "mkr: %v\n", err)
		os.Exit(1)
	}
}

// usage describes the command line. It is intentionally short; the manual
// lives in docs/.
const usage = `mkr - agentic coding assistant for air-gapped environments

Usage:
  mkr [flags] [prompt]        start an interactive session, or run one prompt
  mkr probe [flags]           report what the configured endpoint supports
  mkr audit [verify] [PATH]   verify the audit log's hash chain
  mkr selftest [flags]        check that this machine can run mkr correctly
  mkr config [show|path]      show the resolved configuration and its sources
  mkr config set KEY VALUE    save a setting to the user configuration
  mkr version                 print the build version

Flags:
  -endpoint URL     vLLM base URL (default from config, then MKR_ENDPOINT)
  -model ID         served model ID (default: whatever /v1/models reports)
  -mode MODE        plan | approve | auto   (default approve)
  -adapter NAME     auto | native | xml     (default auto)
  -C DIR            workspace directory (default: current directory)
  -p                print mode: run one prompt and exit, no interaction
  -resume ID        resume a session by ID, or "last"
  -skip-endpoint    selftest only: skip the endpoint check
  -json             selftest only: emit the report as JSON
  -no-redact        disable secret redaction (audited)
  -timeout DUR      per-request timeout (default 5m)

Permission modes:
  plan      read-only; the agent investigates and proposes, changing nothing
  approve   prompts before every write or command (default)
  auto      runs without prompting; deny rules and the workspace jail still apply
`

func run(args []string) error {
	// A subcommand may appear before the flags ("mkr probe -endpoint X") or
	// after them ("mkr -C /repo probe"). Both are natural to type, so both
	// are accepted. The leading case is split out here, before parsing,
	// because the flag package stops at the first non-flag argument; the
	// trailing case is picked up from the residual arguments afterwards.
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") && isSubcommand(args[0]) {
		sub, args = args[0], args[1:]
	}

	fs := flag.NewFlagSet("mkr", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }

	var (
		endpoint  = fs.String("endpoint", "", "vLLM base URL")
		model     = fs.String("model", "", "served model ID")
		modeFlag  = fs.String("mode", "", "plan | approve | auto")
		adapter   = fs.String("adapter", "", "auto | native | xml")
		workdir   = fs.String("C", "", "workspace directory")
		printMode = fs.Bool("p", false, "run one prompt and exit")
		noRedact  = fs.Bool("no-redact", false, "disable secret redaction")
		skipEndpt = fs.Bool("skip-endpoint", false, "selftest: skip the endpoint check")
		jsonOut   = fs.Bool("json", false, "selftest: emit the report as JSON")
		timeout   = fs.Duration("timeout", 0, "per-request timeout")
		resume    = fs.String("resume", "", "resume a session by ID, or \"last\"")
		showVer   = fs.Bool("version", false, "print version")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Recover a subcommand that followed the flags, then continue parsing
	// what came after it.
	//
	// The flag package stops at the first non-flag argument, so in
	// "mkr -C /repo selftest --skip-endpoint" it would parse -C, stop at
	// "selftest", and leave --skip-endpoint unparsed — silently ignoring a
	// flag the operator typed. Lifting the subcommand out and parsing the
	// remainder makes flags work on either side of it.
	rest := fs.Args()
	if sub == "" && len(rest) > 0 && isSubcommand(rest[0]) {
		sub = rest[0]
		if err := fs.Parse(rest[1:]); err != nil {
			return err
		}
		rest = fs.Args()
	}
	if sub == "help" {
		fmt.Print(usage)
		return nil
	}
	if sub == "version" || *showVer {
		fmt.Printf("mkr %s\n", buildVersion())
		return nil
	}

	ws, err := resolveWorkspace(*workdir)
	if err != nil {
		return err
	}
	cfg, err := config.Load(ws)
	if err != nil {
		return err
	}

	// Flags are the highest-precedence layer.
	if *endpoint != "" {
		cfg.Endpoint = *endpoint
	}
	if *model != "" {
		cfg.Model = *model
	}
	if *modeFlag != "" {
		m, err := config.ParseMode(*modeFlag)
		if err != nil {
			return err
		}
		cfg.Mode = m
	}
	if *adapter != "" {
		a, err := config.ParseAdapter(*adapter)
		if err != nil {
			return err
		}
		cfg.Adapter = a
	}
	if *noRedact {
		cfg.Redact = false
	}
	if *timeout > 0 {
		cfg.RequestTimeout = config.Duration(*timeout)
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch sub {
	case "probe":
		return runProbe(ctx, cfg)
	case "audit":
		return runAuditVerify(cfg, rest)
	case "selftest":
		return runSelfTest(ctx, cfg, *skipEndpt, *jsonOut)
	case "config":
		return runConfig(cfg, rest)
	default:
		return runChat(ctx, cfg, chatOptions{
			Prompt: strings.TrimSpace(strings.Join(rest, " ")),
			Print:  *printMode,
			Resume: *resume,
		})
	}
}

// subcommands are the verbs mkr accepts in place of a prompt.
var subcommands = map[string]bool{
	"probe": true, "version": true, "help": true,
	"audit": true, "selftest": true, "config": true,
}

// isSubcommand reports whether s names a subcommand.
func isSubcommand(s string) bool { return subcommands[s] }

// resolveWorkspace turns the -C flag into an absolute, symlink-resolved
// directory. Everything the agent may touch is anchored to this path.
func resolveWorkspace(dir string) (string, error) {
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("determine working directory: %w", err)
		}
		dir = wd
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve workspace %q: %w", dir, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("workspace %q: %w", abs, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace %q is not a directory", abs)
	}
	// Resolving symlinks here means the jail compares like with like.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return abs, nil
}

// newProvider builds the egress-restricted client and the vLLM provider.
func newProvider(cfg config.Config) (*provider.Client, error) {
	host, err := cfg.Host()
	if err != nil {
		return nil, err
	}
	var extraCAs []byte
	if cfg.CACert != "" {
		extraCAs, err = os.ReadFile(cfg.CACert)
		if err != nil {
			return nil, fmt.Errorf("read ca_cert %s: %w", cfg.CACert, err)
		}
	}
	// The stream timeout is enforced per-request via context, not on the
	// http.Client, because a long streaming turn is legitimate.
	hc, _, err := netguard.NewClient(netguard.Options{
		AllowedHost:  host,
		ExtraRootCAs: extraCAs,
	})
	if err != nil {
		return nil, err
	}
	return provider.NewClient(cfg.BaseURL(), cfg.APIKey, hc)
}

// runProbe reports what the endpoint supports and which adapter will be used.
func runProbe(ctx context.Context, cfg config.Config) error {
	c, err := newProvider(cfg)
	if err != nil {
		return err
	}
	host, _ := cfg.Host()
	fmt.Printf("endpoint:     %s\n", cfg.BaseURL())
	fmt.Printf("egress:       restricted to %s\n", host)

	start := time.Now()
	caps, err := provider.Probe(ctx, c, string(cfg.Adapter), cfg.Model)
	if err != nil {
		return err
	}
	fmt.Print(caps.Summary())
	fmt.Printf("probed in:    %s\n", time.Since(start).Round(time.Millisecond))
	return nil
}
