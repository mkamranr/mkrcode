// Package audit writes an append-only, tamper-evident record of everything
// the agent did.
//
// Each record carries the SHA-256 of the record before it, forming a chain
// from the first line of the file. Altering or deleting any record breaks
// every hash after it, which `mkr audit verify` detects. The chain does not
// prevent an administrator from rewriting the whole file, so it establishes
// tamper evidence rather than tamper resistance; pairing it with a
// write-only collector is what closes that gap.
//
// Records are written after redaction, so the audit log never becomes the
// place where a secret is preserved.
package audit

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"mkrcode/internal/secureio"
)

// Event names the kind of record.
type Event string

// The recorded event types.
const (
	EventSessionStart      Event = "session_start"
	EventSessionEnd        Event = "session_end"
	EventUserPrompt        Event = "user_prompt"
	EventModelRequest      Event = "model_request"
	EventToolCall          Event = "tool_call"
	EventPermission        Event = "permission"
	EventRedaction         Event = "redaction"
	EventModeChange        Event = "mode_change"
	EventContextCompaction Event = "context_compaction"
	EventError             Event = "error"
)

// Record is one line of the audit log.
//
// Field names are stable: downstream collectors parse them.
type Record struct {
	// Seq is the 1-based position in the chain.
	Seq int64 `json:"seq"`
	// Time is when the record was written, in RFC 3339 with nanoseconds.
	Time string `json:"time"`
	// PrevHash is the SHA-256 of the preceding record's canonical form.
	// The first record carries the empty-chain sentinel.
	PrevHash string `json:"prev_hash"`

	Event   Event  `json:"event"`
	Session string `json:"session"`
	User    string `json:"user"`
	Host    string `json:"host"`
	// Workspace is the jail root this session operated in.
	Workspace string `json:"workspace,omitempty"`
	// Mode is the permission mode in force.
	Mode string `json:"mode,omitempty"`

	// Tool and Args describe a tool call.
	Tool string `json:"tool,omitempty"`
	Args string `json:"args,omitempty"`
	// Summary is the human-readable one-liner.
	Summary string `json:"summary,omitempty"`
	// Decision and Reason record a permission outcome.
	Decision string `json:"decision,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Rule     string `json:"rule,omitempty"`
	// ExitCode is the exit status of an executed command.
	ExitCode *int `json:"exit_code,omitempty"`
	// Diff is a unified diff of a file mutation.
	Diff string `json:"diff,omitempty"`
	// Paths are the files a call touched.
	Paths []string `json:"paths,omitempty"`

	// PromptHash identifies a model request without storing its content.
	PromptHash string `json:"prompt_hash,omitempty"`
	// Prompt is the full request body, present only when audit_prompts is on.
	Prompt string `json:"prompt,omitempty"`
	// Tokens records usage for a model request.
	Tokens *Tokens `json:"tokens,omitempty"`
	// Model and Adapter identify how the request was served.
	Model   string `json:"model,omitempty"`
	Adapter string `json:"adapter,omitempty"`

	// Redactions summarises what was scrubbed, never the values.
	Redactions string `json:"redactions,omitempty"`
	// Message carries an error or a note.
	Message string `json:"message,omitempty"`

	// Hash is the SHA-256 of this record's canonical form. It is computed
	// over every other field, so it is excluded from its own input.
	Hash string `json:"hash"`
}

// Tokens is the usage of one model request.
type Tokens struct {
	Prompt     int `json:"prompt"`
	Completion int `json:"completion"`
	Total      int `json:"total"`
}

// genesisHash begins the chain. A fixed non-empty sentinel means an empty
// PrevHash can never be mistaken for a valid first record.
const genesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// Logger appends records to the audit file.
type Logger struct {
	mu       sync.Mutex
	f        *os.File
	w        *bufio.Writer
	seq      int64
	prevHash string

	session   string
	user      string
	host      string
	workspace string
}

// Options configures a Logger.
type Options struct {
	// Path is the audit file. Parent directories are created.
	Path string
	// Session identifies this run.
	Session string
	// User and Host identify who ran it.
	User string
	Host string
	// Workspace is the jail root.
	Workspace string
}

// Open opens or creates the audit log and resumes the existing hash chain.
func Open(opt Options) (*Logger, error) {
	if opt.Path == "" {
		return nil, errors.New("audit: a path is required")
	}
	if err := secureio.MkdirAllPrivate(filepath.Dir(opt.Path)); err != nil {
		return nil, fmt.Errorf("audit: create directory: %w", err)
	}

	// Resume the chain before opening for append, so a new process
	// continues the existing file rather than starting a second chain.
	seq, prev, err := tail(opt.Path)
	if err != nil {
		return nil, err
	}

	// The audit log records what was done in a controlled environment and
	// must not be readable by other users of the machine. secureio applies
	// owner-only access on both Unix and Windows; the mode argument to
	// os.OpenFile alone does not achieve this on Windows.
	f, err := secureio.OpenAppend(opt.Path)
	if err != nil {
		return nil, fmt.Errorf("audit: open %s: %w", opt.Path, err)
	}
	return &Logger{
		f:         f,
		w:         bufio.NewWriter(f),
		seq:       seq,
		prevHash:  prev,
		session:   opt.Session,
		user:      opt.User,
		host:      opt.Host,
		workspace: opt.Workspace,
	}, nil
}

// tail returns the sequence number and hash of the last record in path.
func tail(path string) (int64, string, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, genesisHash, nil
	}
	if err != nil {
		return 0, "", fmt.Errorf("audit: read %s: %w", path, err)
	}
	defer f.Close()

	var (
		seq  int64
		hash = genesisHash
	)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			return 0, "", fmt.Errorf("audit: %s contains a malformed record at line %d; verify the log before appending", path, seq+1)
		}
		seq, hash = r.Seq, r.Hash
	}
	if err := sc.Err(); err != nil {
		return 0, "", fmt.Errorf("audit: scan %s: %w", path, err)
	}
	return seq, hash, nil
}

// Log appends a record, filling in the chain fields.
func (l *Logger) Log(r Record) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	l.seq++
	r.Seq = l.seq
	r.Time = time.Now().UTC().Format(time.RFC3339Nano)
	r.PrevHash = l.prevHash
	r.Session = l.session
	r.User = l.user
	r.Host = l.host
	if r.Workspace == "" {
		r.Workspace = l.workspace
	}
	r.Hash = ""
	r.Hash = hashRecord(r)

	b, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("audit: encode record: %w", err)
	}
	if _, err := l.w.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("audit: write record: %w", err)
	}
	// Flushing every record means an interrupted session still leaves a
	// complete log, which is the point of an audit trail.
	if err := l.w.Flush(); err != nil {
		return fmt.Errorf("audit: flush: %w", err)
	}
	l.prevHash = r.Hash
	return nil
}

// Close flushes and closes the log.
func (l *Logger) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.w.Flush(); err != nil {
		l.f.Close()
		return err
	}
	return l.f.Close()
}

// hashRecord computes the canonical hash of a record with Hash cleared.
func hashRecord(r Record) string {
	r.Hash = ""
	// json.Marshal of a struct emits fields in declaration order, so the
	// encoding is deterministic for a given Go version and struct layout.
	b, err := json.Marshal(r)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// VerifyResult reports the outcome of checking a log.
type VerifyResult struct {
	// Records is how many records were read.
	Records int64
	// OK is true when the chain is intact.
	OK bool
	// Problem describes the first break found.
	Problem string
	// BrokenAt is the sequence number where verification failed.
	BrokenAt int64
}

// Verify walks the chain in path and reports the first inconsistency.
func Verify(path string) (VerifyResult, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return VerifyResult{OK: true}, nil
	}
	if err != nil {
		return VerifyResult{}, fmt.Errorf("audit: open %s: %w", path, err)
	}
	defer f.Close()
	return verify(f)
}

func verify(r io.Reader) (VerifyResult, error) {
	var (
		res  VerifyResult
		prev = genesisHash
		seq  int64
	)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		seq++
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			res.Problem = fmt.Sprintf("record %d is not valid JSON", seq)
			res.BrokenAt = seq
			res.Records = seq - 1
			return res, nil
		}
		if rec.Seq != seq {
			res.Problem = fmt.Sprintf("record %d declares sequence %d; a record was inserted or removed", seq, rec.Seq)
			res.BrokenAt = seq
			res.Records = seq - 1
			return res, nil
		}
		if rec.PrevHash != prev {
			res.Problem = fmt.Sprintf("record %d does not chain to its predecessor; a record was altered or removed", seq)
			res.BrokenAt = seq
			res.Records = seq - 1
			return res, nil
		}
		if want := hashRecord(rec); want != rec.Hash {
			res.Problem = fmt.Sprintf("record %d has been modified since it was written", seq)
			res.BrokenAt = seq
			res.Records = seq - 1
			return res, nil
		}
		prev = rec.Hash
	}
	if err := sc.Err(); err != nil {
		return res, fmt.Errorf("audit: scan: %w", err)
	}
	res.Records = seq
	res.OK = true
	return res, nil
}

// HashPrompt returns a stable identifier for a prompt body, so a request
// can be correlated without the log storing its content.
func HashPrompt(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
