// Package permission decides whether a tool call may proceed.
//
// Three modes are supported, in increasing order of autonomy:
//
//	plan     every mutating tool is refused; the agent investigates only
//	approve  every mutating tool is put to the operator (the default)
//	auto     mutating tools run unprompted
//
// Independently of mode, an allow/deny rule set constrains which paths may
// be written and which commands may be run. Deny always wins, in every
// mode including auto, so there is no posture in which a forbidden command
// becomes permitted.
package permission

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"

	"mkrcode/internal/config"
)

// Decision is the outcome of evaluating a tool call.
type Decision int

const (
	// Allow permits the call without asking.
	Allow Decision = iota
	// Ask requires operator confirmation.
	Ask
	// Deny refuses the call outright.
	Deny
)

func (d Decision) String() string {
	switch d {
	case Allow:
		return "allow"
	case Ask:
		return "ask"
	case Deny:
		return "deny"
	}
	return "unknown"
}

// Request describes a tool call awaiting a decision.
type Request struct {
	// Tool is the tool name.
	Tool string
	// Mutating reports whether the tool changes state.
	Mutating bool
	// Summary is the one-line description shown to the operator.
	Summary string
	// Paths are the workspace-relative paths the call would write.
	Paths []string
	// Command is the command line, for the exec tool.
	Command string
}

// Outcome records a decision and why it was reached, for the audit log.
type Outcome struct {
	Decision Decision
	// Reason explains the decision in terms the operator can act on.
	Reason string
	// Rule names the matching rule, when one applied.
	Rule string
}

// Prompter asks the operator to confirm a request. It is an interface so
// the engine can be tested without a terminal.
type Prompter interface {
	// Confirm returns whether to proceed, and whether the answer should be
	// remembered for the rest of the session.
	Confirm(req Request) (allow bool, remember bool, err error)
}

// ErrDenied is returned when the operator refuses a request.
var ErrDenied = errors.New("refused by the operator")

// Rules is the allow/deny policy.
type Rules struct {
	// DenyCommands are regular expressions matched against a command line.
	// A match refuses the call in every mode.
	DenyCommands []string `json:"deny_commands"`
	// AllowCommands, when non-empty, restricts commands to those matching.
	AllowCommands []string `json:"allow_commands"`
	// DenyPaths are globs matched against workspace-relative write paths.
	DenyPaths []string `json:"deny_paths"`
	// AutoAllowTools are tools that never prompt, even in approve mode.
	AutoAllowTools []string `json:"auto_allow_tools"`
}

// DefaultRules is the policy applied when no rules file exists.
//
// The command deny list exists because the exec tool cannot be network
// jailed: a command the operator approves inherits the user's own access.
// Refusing the common egress utilities narrows that gap, and the refusal
// holds in auto mode where nobody is watching. It is a mitigation, not a
// guarantee; host firewall policy is what actually enforces egress.
func DefaultRules() Rules {
	return Rules{
		DenyCommands: []string{
			// Network egress.
			`(?i)\b(curl|wget|nc|ncat|netcat|telnet|ftp|tftp)\b`,
			`(?i)\bInvoke-(WebRequest|RestMethod)\b`,
			`(?i)\b(iwr|irm|wget|curl)\b`,
			`(?i)\bbitsadmin\b`,
			`(?i)\bStart-BitsTransfer\b`,
			`(?i)\b(ssh|scp|sftp|rsync)\b`,
			`(?i)\bNew-Object\s+System\.Net\.WebClient\b`,
			`(?i)\bSystem\.Net\.Sockets\b`,
			// Credential access.
			`(?i)\bGet-Credential\b`,
			`(?i)\bmimikatz\b`,
			`(?i)\breg\s+save\b.*\b(sam|security|system)\b`,
			// Destructive operations outside the agent's remit.
			`(?i)\bformat\s+[a-z]:`,
			`(?i)\bRemove-Item\b.*\b-Recurse\b.*\b[A-Z]:\\`,
			`(?i)\brm\s+-rf\s+/(\s|$)`,
			`(?i)\b(shutdown|Restart-Computer|Stop-Computer)\b`,
			// Security control tampering.
			`(?i)\bSet-MpPreference\b`,
			`(?i)\bSet-ExecutionPolicy\b`,
			`(?i)\bnetsh\s+(advfirewall|firewall)\b`,
			`(?i)\bDisable-WindowsOptionalFeature\b`,
		},
		DenyPaths: []string{
			// mkr's own trust anchors must not be rewritable by the agent.
			// Skills are instructions the agent then follows, so letting it
			// write its own would defeat the point of reviewing them.
			".mkr/rules.json",
			".mkr/config.json",
			".mkr/skills/**",
			"**/.git/hooks/**",
			"**/.ssh/**",
			"**/*.pem",
			"**/*.key",
			"**/*.pfx",
			"**/id_rsa*",
		},
		AutoAllowTools: []string{"read_file", "list_dir", "glob", "grep"},
	}
}

// LoadRules reads a rules file, returning the defaults when it is absent.
func LoadRules(path string) (Rules, error) {
	r := DefaultRules()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, fmt.Errorf("read rules %s: %w", path, err)
	}
	// A rules file replaces the defaults it names and keeps the rest, so
	// an operator adding one deny rule does not silently lose the others.
	var loaded Rules
	if err := json.Unmarshal(b, &loaded); err != nil {
		return r, fmt.Errorf("parse rules %s: %w", path, err)
	}
	r.DenyCommands = append(r.DenyCommands, loaded.DenyCommands...)
	r.DenyPaths = append(r.DenyPaths, loaded.DenyPaths...)
	r.AllowCommands = append(r.AllowCommands, loaded.AllowCommands...)
	if len(loaded.AutoAllowTools) > 0 {
		r.AutoAllowTools = loaded.AutoAllowTools
	}
	return r, nil
}

// Engine evaluates requests against a mode and a rule set.
type Engine struct {
	mu   sync.RWMutex
	mode config.Mode

	denyCmd  []*regexp.Regexp
	allowCmd []*regexp.Regexp
	denyPath []*regexp.Regexp
	autoTool map[string]bool

	prompter Prompter
	// remembered holds session-scoped approvals keyed by tool and summary.
	remembered map[string]bool
}

// NewEngine compiles rules and returns an engine in the given mode.
func NewEngine(mode config.Mode, rules Rules, p Prompter) (*Engine, error) {
	e := &Engine{
		mode:       mode,
		prompter:   p,
		autoTool:   map[string]bool{},
		remembered: map[string]bool{},
	}
	var err error
	if e.denyCmd, err = compileAll(rules.DenyCommands, "deny_commands"); err != nil {
		return nil, err
	}
	if e.allowCmd, err = compileAll(rules.AllowCommands, "allow_commands"); err != nil {
		return nil, err
	}
	for _, g := range rules.DenyPaths {
		re, err := globToRegexp(g)
		if err != nil {
			return nil, fmt.Errorf("deny_paths %q: %w", g, err)
		}
		e.denyPath = append(e.denyPath, re)
	}
	for _, t := range rules.AutoAllowTools {
		e.autoTool[t] = true
	}
	return e, nil
}

func compileAll(exprs []string, field string) ([]*regexp.Regexp, error) {
	out := make([]*regexp.Regexp, 0, len(exprs))
	for _, e := range exprs {
		re, err := regexp.Compile(e)
		if err != nil {
			return nil, fmt.Errorf("%s %q: %w", field, e, err)
		}
		out = append(out, re)
	}
	return out, nil
}

// Mode returns the current mode.
func (e *Engine) Mode() config.Mode {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.mode
}

// SetMode changes the mode at runtime, as the /mode command does.
//
// Session approvals are cleared on every change: an "always allow" granted
// under approve must not silently carry into a later session, and dropping
// to plan mode should not leave standing permissions behind.
func (e *Engine) SetMode(m config.Mode) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.mode = m
	e.remembered = map[string]bool{}
}

// Evaluate decides a request without prompting. Check returns Ask when the
// operator must be consulted.
func (e *Engine) Evaluate(req Request) Outcome {
	e.mu.RLock()
	mode := e.mode
	e.mu.RUnlock()

	// Deny rules are absolute and are checked before anything else, so no
	// mode and no remembered approval can reach past them.
	if req.Command != "" {
		for _, re := range e.denyCmd {
			if re.MatchString(req.Command) {
				return Outcome{Deny, "the command matches a deny rule and is refused in every mode", re.String()}
			}
		}
		if len(e.allowCmd) > 0 {
			ok := false
			for _, re := range e.allowCmd {
				if re.MatchString(req.Command) {
					ok = true
					break
				}
			}
			if !ok {
				return Outcome{Deny, "an allow list is configured and the command does not match it", ""}
			}
		}
	}
	for _, p := range req.Paths {
		norm := strings.TrimPrefix(strings.ReplaceAll(p, "\\", "/"), "./")
		for _, re := range e.denyPath {
			if re.MatchString(norm) {
				return Outcome{Deny, fmt.Sprintf("%s matches a protected path and may not be modified", p), re.String()}
			}
		}
	}

	if !req.Mutating {
		return Outcome{Allow, "read-only", ""}
	}
	if e.autoTool[req.Tool] {
		return Outcome{Allow, "tool is on the auto-allow list", ""}
	}

	switch mode {
	case config.ModePlan:
		return Outcome{Deny, "plan mode is read-only; propose the change instead of making it", "mode:plan"}
	case config.ModeAuto:
		return Outcome{Allow, "auto mode", "mode:auto"}
	default:
		e.mu.RLock()
		remembered := e.remembered[rememberKey(req)]
		e.mu.RUnlock()
		if remembered {
			return Outcome{Allow, "approved earlier in this session", ""}
		}
		return Outcome{Ask, "approve mode requires confirmation", "mode:approve"}
	}
}

// Check evaluates a request and, when required, prompts the operator.
//
// It returns nil when the call may proceed. A refusal is returned as an
// error whose message is handed to the model so it can adapt, rather than
// stalling on a permission it cannot obtain.
func (e *Engine) Check(req Request) (Outcome, error) {
	out := e.Evaluate(req)
	switch out.Decision {
	case Allow:
		return out, nil
	case Deny:
		return out, fmt.Errorf("%s: %s", req.Tool, out.Reason)
	}

	if e.prompter == nil {
		return Outcome{Deny, "confirmation is required but no prompter is available", ""},
			errors.New("confirmation required but this session is not interactive; run with -mode auto or approve the action interactively")
	}
	allow, remember, err := e.prompter.Confirm(req)
	if err != nil {
		return Outcome{Deny, "prompt failed: " + err.Error(), ""}, err
	}
	if !allow {
		return Outcome{Deny, "the operator declined this action", "operator"}, ErrDenied
	}
	if remember {
		e.mu.Lock()
		e.remembered[rememberKey(req)] = true
		e.mu.Unlock()
	}
	return Outcome{Allow, "approved by the operator", "operator"}, nil
}

// rememberKey identifies an approval for the rest of the session. It keys
// on the tool and its rendered summary, so "always allow" grants repetition
// of the same action rather than blanket use of the tool.
func rememberKey(req Request) string {
	return req.Tool + "\x00" + req.Summary
}

// globToRegexp converts a path glob into an anchored regular expression.
func globToRegexp(pattern string) (*regexp.Regexp, error) {
	p := strings.TrimPrefix(strings.ReplaceAll(pattern, "\\", "/"), "./")
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(p); i++ {
		switch c := p[i]; c {
		case '*':
			if i+1 < len(p) && p[i+1] == '*' {
				i++
				if i+1 < len(p) && p[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	// Paths are compared case-insensitively because the target filesystem
	// is, and a case-varied name must not slip past a protection rule.
	return regexp.Compile("(?i)" + b.String())
}
