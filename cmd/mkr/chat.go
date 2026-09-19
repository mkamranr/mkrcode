package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"strings"

	"mkrcode/internal/agent"
	"mkrcode/internal/audit"
	"mkrcode/internal/config"
	"mkrcode/internal/fsjail"
	"mkrcode/internal/permission"
	"mkrcode/internal/provider"
	"mkrcode/internal/redact"
	"mkrcode/internal/session"
	"mkrcode/internal/skills"
	"mkrcode/internal/tools"
	"mkrcode/internal/ui"
)

// chatOptions carries the invocation-specific settings for a session.
type chatOptions struct {
	// Prompt is a one-shot request. Empty starts the interactive loop.
	Prompt string
	// Print runs one prompt and exits without the interactive loop.
	Print bool
	// Resume names a session to continue. "last" continues the newest.
	Resume string
}

// runChat builds every component and runs the session.
func runChat(ctx context.Context, cfg config.Config, opt chatOptions) (err error) {
	render := ui.NewTerminal()

	jail, err := fsjail.New(cfg.Workspace)
	if err != nil {
		return err
	}

	// The shell is resolved once at startup so a missing pwsh is reported
	// immediately rather than on the first command the model runs.
	if cfg.Shell == "" {
		cfg.Shell = tools.DetectShell()
	}

	client, err := newProvider(cfg)
	if err != nil {
		return err
	}
	caps, err := provider.Probe(ctx, client, string(cfg.Adapter), cfg.Model)
	if err != nil {
		return fmt.Errorf("%w\n\nThe endpoint could not be reached. Check that vLLM is running and that %s is permitted by the firewall.", err, cfg.Endpoint)
	}
	adapter, err := provider.SelectAdapter(caps.Adapter)
	if err != nil {
		return err
	}

	// Skills are discovered before the tool registry is built, because the
	// skill tool is only registered when there is something to load.
	userCfgDir, _ := config.UserConfigDir()
	skillSet, skillProblems := skills.Discover(cfg.Workspace, userCfgDir)
	for _, p := range skillProblems {
		// A malformed skill in a shared repository must not stop a session.
		render.Warn("skipping skill: %v", p)
	}

	rules, err := permission.LoadRules(cfg.RulesPath)
	if err != nil {
		return err
	}
	perms, err := permission.NewEngine(cfg.Mode, rules, render)
	if err != nil {
		return err
	}

	sessionID := session.NewID()
	var history []provider.Message
	if opt.Resume != "" {
		id := opt.Resume
		if id == "last" {
			latest, ok, err := session.Latest(cfg.SessionDir)
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("no previous session to resume")
			}
			id = latest.ID
		}
		history, err = session.Load(cfg.SessionDir, id)
		if err != nil {
			return err
		}
		sessionID = id
		render.Info("resumed session %s (%d messages)", id, len(history))
	}

	sess, err := session.Create(cfg.SessionDir, sessionID)
	if err != nil {
		return err
	}
	defer sess.Close()

	auditLog, err := audit.Open(audit.Options{
		Path:      cfg.AuditPath,
		Session:   sessionID,
		User:      currentUser(),
		Host:      hostname(),
		Workspace: cfg.Workspace,
	})
	if err != nil {
		return err
	}
	defer auditLog.Close()

	auditLog.Log(audit.Record{
		Event:   audit.EventSessionStart,
		Mode:    string(cfg.Mode),
		Model:   caps.Model,
		Adapter: caps.Adapter,
		Message: fmt.Sprintf("mkr %s, endpoint %s, redaction %t", version, cfg.BaseURL(), cfg.Redact),
	})
	defer func() {
		auditLog.Log(audit.Record{Event: audit.EventSessionEnd, Mode: string(perms.Mode())})
	}()

	if !cfg.Redact {
		// Disabling redaction is a decision worth seeing in the log and on
		// screen, because it changes what can leave the workspace.
		render.Warn("secret redaction is DISABLED for this session")
		auditLog.Log(audit.Record{Event: audit.EventError, Message: "redaction disabled via -no-redact"})
	}

	ag := agent.New(agent.Options{
		Config:  cfg,
		Client:  client,
		Adapter: adapter,
		Registry: tools.Standard(tools.Options{
			Jail:               jail,
			Shell:              cfg.Shell,
			ExecTimeoutSeconds: int(cfg.ExecTimeout.D().Seconds()),
			Skills:             skillSet,
		}),
		EnableTasks: true,
		Skills:      skillSet,
		Perms:       perms,
		Redactor:    redact.New(cfg.Redact),
		Audit:       auditLog,
		Session:     sess,
		Renderer:    render,
		Model:       caps.Model,
		History:     history,
		MaxModelLen: caps.MaxModelLen,
	})

	if opt.Print || (opt.Prompt != "" && !isInteractive()) {
		if opt.Prompt == "" {
			return errors.New("print mode needs a prompt: mkr -p \"your request\"")
		}
		return ag.Run(ctx, opt.Prompt)
	}

	render.Banner(
		fmt.Sprintf("mkr %s  ·  %s  ·  adapter %s", version, caps.Model, caps.Adapter),
		fmt.Sprintf("workspace %s", cfg.Workspace),
		fmt.Sprintf("mode %s  ·  redaction %s  ·  skills %d", perms.Mode(), onOff(cfg.Redact), skillSet.Len()),
		fmt.Sprintf("audit %s", cfg.AuditPath),
		"/help for commands, /mode to change permissions, Ctrl+C to interrupt, Ctrl+D to exit",
	)
	for _, n := range caps.Notes {
		render.Warn("%s", n)
	}

	return repl(ctx, ag, perms, render, cfg, opt.Prompt)
}

// repl is the interactive loop.
func repl(ctx context.Context, ag *agent.Agent, perms *permission.Engine, render *ui.Terminal, cfg config.Config, first string) error {
	pending := first
	for {
		line := pending
		pending = ""
		if line == "" {
			var err error
			line, err = render.Prompt(string(perms.Mode()))
			if err != nil {
				if errors.Is(err, io.EOF) {
					render.Info("\ngoodbye")
					return nil
				}
				return err
			}
		}
		if strings.TrimSpace(line) == "" {
			continue
		}

		if strings.HasPrefix(line, "/") {
			stop, err := command(line, ag, perms, render, cfg)
			if err != nil {
				render.Errorf("%v", err)
			}
			if stop {
				return nil
			}
			continue
		}

		// Each request gets its own cancellable context so Ctrl+C aborts
		// the turn without ending the session.
		turnCtx, cancel := context.WithCancel(ctx)
		err := ag.Run(turnCtx, line)
		cancel()

		switch {
		case errors.Is(err, context.Canceled):
			// The parent context is cancelled only by a signal, which ends
			// the program; a turn-level cancel returns to the prompt.
			if ctx.Err() != nil {
				render.Info("\ninterrupted")
				return nil
			}
			render.Info("interrupted")
		case err != nil:
			render.Errorf("%v", err)
		}
	}
}

// command handles a slash command, returning true when the session ends.
func command(line string, ag *agent.Agent, perms *permission.Engine, render *ui.Terminal, cfg config.Config) (bool, error) {
	fields := strings.Fields(line)
	cmd := strings.ToLower(fields[0])
	args := fields[1:]

	switch cmd {
	case "/exit", "/quit":
		return true, nil

	case "/help":
		render.Info(`commands:
  /mode [plan|approve|auto]   show or change the permission mode
  /tools                      list the available tools
  /skills                     list the installed skills
  /cost                       show token usage and context window use
  /compact                    reduce the transcript to free context
  /audit                      show the audit log path and verify its chain
  /clear                      start a fresh conversation
  /help                       this list
  /exit                       end the session`)
		return false, nil

	case "/mode":
		if len(args) == 0 {
			render.Info("mode is %s (plan, approve, auto)", perms.Mode())
			return false, nil
		}
		m, err := config.ParseMode(args[0])
		if err != nil {
			return false, err
		}
		ag.SetMode(m)
		render.Info("mode is now %s", m)
		return false, nil

	case "/tools":
		var b strings.Builder
		b.WriteString("tools:\n")
		for _, t := range ag.Registry().All() {
			kind := "read-only"
			if t.Mutating() {
				kind = "mutating"
			}
			fmt.Fprintf(&b, "  %-12s %s\n", t.Name(), kind)
		}
		render.Info("%s", strings.TrimRight(b.String(), "\n"))
		return false, nil

	case "/skills":
		all := ag.Skills().All()
		if len(all) == 0 {
			render.Info("no skills installed\n" +
				"  project skills: <workspace>/.mkr/skills/<name>/SKILL.md\n" +
				"  personal skills: <user config>/mkr/skills/<name>/SKILL.md")
			return false, nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%d skill(s):\n", len(all))
		for _, sk := range all {
			fmt.Fprintf(&b, "  %-24s %s\n    %s\n", sk.Name, "("+string(sk.Source)+")", sk.Description)
		}
		render.Info("%s", strings.TrimRight(b.String(), "\n"))
		return false, nil

	case "/cost":
		u := ag.Usage()
		render.Info("tokens: %d prompt, %d completion, %d total",
			u.PromptTokens, u.CompletionTokens, u.TotalTokens)
		if used, window := ag.ContextUsage(); window > 0 {
			render.Info("context: ~%d of %d tokens (%d%%)", used, window, used*100/window)
		} else {
			render.Info("context: ~%d tokens; the server did not report a window size", used)
		}
		return false, nil

	case "/compact":
		ag.Compact()
		return false, nil

	case "/audit":
		res, err := audit.Verify(cfg.AuditPath)
		if err != nil {
			return false, err
		}
		if res.OK {
			render.Info("audit log %s: %d records, chain intact", cfg.AuditPath, res.Records)
		} else {
			render.Errorf("audit log %s FAILED verification at record %d: %s",
				cfg.AuditPath, res.BrokenAt, res.Problem)
		}
		return false, nil

	case "/clear":
		ag.Reset()
		render.Info("conversation cleared")
		return false, nil

	default:
		return false, fmt.Errorf("unknown command %q; try /help", cmd)
	}
}

// runAuditVerify implements the audit subcommand.
func runAuditVerify(cfg config.Config, args []string) error {
	// Both "mkr audit /path" and "mkr audit verify /path" are natural to
	// type, so a leading "verify" is optional.
	if len(args) > 0 && strings.EqualFold(args[0], "verify") {
		args = args[1:]
	}
	path := cfg.AuditPath
	if len(args) > 0 && args[0] != "" {
		path = args[0]
	}
	res, err := audit.Verify(path)
	if err != nil {
		return err
	}
	if res.OK {
		fmt.Printf("audit log:    %s\nrecords:      %d\nchain:        intact\n", path, res.Records)
		return nil
	}
	fmt.Fprintf(os.Stderr, "audit log:    %s\nrecords read: %d\nchain:        BROKEN at record %d\nproblem:      %s\n",
		path, res.Records, res.BrokenAt, res.Problem)
	return errors.New("audit verification failed")
}

func currentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return os.Getenv("USERNAME")
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// isInteractive reports whether stdin is a terminal. When it is not, a
// piped prompt runs once rather than waiting at a prompt that will never
// be answered.
func isInteractive() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
