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
	"strings"
	"syscall"
	"time"

	"mkrcode/internal/config"
	"mkrcode/internal/netguard"
	"mkrcode/internal/provider"
)

// version is stamped at build time with -ldflags "-X main.version=...".
var version = "dev"

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
  mkr version                 print the build version

Flags:
  -endpoint URL     vLLM base URL (default from config, then MKR_ENDPOINT)
  -model ID         served model ID (default: whatever /v1/models reports)
  -mode MODE        plan | approve | auto   (default approve)
  -adapter NAME     auto | native | xml     (default auto)
  -C DIR            workspace directory (default: current directory)
  -p                print mode: run one prompt and exit, no interaction
  -resume ID        resume a session by ID, or "last"
  -no-redact        disable secret redaction (audited)
  -timeout DUR      per-request timeout (default 5m)

Permission modes:
  plan      read-only; the agent investigates and proposes, changing nothing
  approve   prompts before every write or command (default)
  auto      runs without prompting; deny rules and the workspace jail still apply
`

func run(args []string) error {
	// Split the subcommand out before flag parsing so that flags may follow
	// it, which is what people type.
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "probe", "version", "help", "audit":
			sub, args = args[0], args[1:]
		}
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
		timeout   = fs.Duration("timeout", 0, "per-request timeout")
		resume    = fs.String("resume", "", "resume a session by ID, or \"last\"")
		showVer   = fs.Bool("version", false, "print version")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if sub == "help" {
		fmt.Print(usage)
		return nil
	}
	if sub == "version" || *showVer {
		fmt.Printf("mkr %s\n", version)
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
		return runAuditVerify(cfg, args)
	default:
		return runChat(ctx, cfg, chatOptions{
			Prompt: strings.TrimSpace(strings.Join(fs.Args(), " ")),
			Print:  *printMode,
			Resume: *resume,
		})
	}
}

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
	// The stream timeout is enforced per-request via context, not on the
	// http.Client, because a long streaming turn is legitimate.
	hc, _, err := netguard.NewClient(host, 0)
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
