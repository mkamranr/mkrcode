// Package session persists conversation transcripts so a session can be
// resumed after the process exits.
//
// Transcripts are JSONL rather than a database: the format is greppable,
// survives partial writes, and keeps the binary free of cgo, which is what
// allows it to ship as a single static file.
package session

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mkamranr/mkrcode/internal/provider"
	"github.com/mkamranr/mkrcode/internal/secureio"
)

// Entry is one line of a transcript.
type Entry struct {
	Time    string           `json:"time"`
	Message provider.Message `json:"message"`
	// Meta carries anything useful for later inspection, such as the mode
	// in force or token usage.
	Meta map[string]string `json:"meta,omitempty"`
}

// Session is an append-only transcript on disk.
type Session struct {
	id   string
	path string
	f    *os.File
	w    *bufio.Writer
}

// ID returns the session identifier.
func (s *Session) ID() string { return s.id }

// Path returns the transcript file path.
func (s *Session) Path() string { return s.path }

// NewID returns a sortable, filesystem-safe session identifier.
func NewID() string {
	return time.Now().UTC().Format("20060102-150405") + "-" + randSuffix()
}

// randSuffix returns a short random suffix that distinguishes sessions
// started within the same second.
//
// It uses crypto/rand rather than deriving from the clock. A clock-derived
// suffix collides for calls made close together, because the low bits of the
// nanosecond counter barely change; two sessions would then share an ID and
// append to the same transcript file, interleaving them.
func randSuffix() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand does not fail in practice; if it ever does, a
		// clock-derived suffix is still better than a fixed one.
		n := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(n >> (8 * i))
		}
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// Create opens a new transcript in dir.
func Create(dir, id string) (*Session, error) {
	// Transcripts contain source code and prompts, so they get the same
	// owner-only protection as the audit log.
	if err := secureio.MkdirAllPrivate(dir); err != nil {
		return nil, fmt.Errorf("session: create %s: %w", dir, err)
	}
	path := filepath.Join(dir, id+".jsonl")
	f, err := secureio.OpenAppend(path)
	if err != nil {
		return nil, fmt.Errorf("session: open %s: %w", path, err)
	}
	return &Session{id: id, path: path, f: f, w: bufio.NewWriter(f)}, nil
}

// Append writes one message to the transcript.
func (s *Session) Append(m provider.Message, meta map[string]string) error {
	if s == nil {
		return nil
	}
	e := Entry{Time: time.Now().UTC().Format(time.RFC3339Nano), Message: m, Meta: meta}
	b, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("session: encode entry: %w", err)
	}
	if _, err := s.w.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("session: write entry: %w", err)
	}
	// Flushing per message means an interrupted session is still resumable.
	return s.w.Flush()
}

// Close flushes and closes the transcript.
func (s *Session) Close() error {
	if s == nil {
		return nil
	}
	if err := s.w.Flush(); err != nil {
		s.f.Close()
		return err
	}
	return s.f.Close()
}

// Load reads the messages of an existing transcript.
func Load(dir, id string) ([]provider.Message, error) {
	path := filepath.Join(dir, id+".jsonl")
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("session %q not found in %s", id, dir)
	}
	if err != nil {
		return nil, fmt.Errorf("session: open %s: %w", path, err)
	}
	defer f.Close()

	var msgs []provider.Message
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			// A truncated final line is expected after a hard kill; stop
			// there and return what was recovered rather than failing.
			break
		}
		msgs = append(msgs, e.Message)
	}
	return msgs, sc.Err()
}

// Info summarises a stored session for listing.
type Info struct {
	ID       string
	Path     string
	Modified time.Time
	Size     int64
}

// List returns the stored sessions, newest first.
func List(dir string) ([]Info, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("session: list %s: %w", dir, err)
	}
	var out []Info
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Info{
			ID:       strings.TrimSuffix(e.Name(), ".jsonl"),
			Path:     filepath.Join(dir, e.Name()),
			Modified: info.ModTime(),
			Size:     info.Size(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out, nil
}

// Latest returns the most recently modified session, if any.
func Latest(dir string) (Info, bool, error) {
	all, err := List(dir)
	if err != nil || len(all) == 0 {
		return Info{}, false, err
	}
	return all[0], true, nil
}
