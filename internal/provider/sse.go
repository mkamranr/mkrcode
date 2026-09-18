package provider

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strings"
)

// sseDoneSentinel is the literal payload OpenAI-compatible servers send to
// mark the end of a stream.
const sseDoneSentinel = "[DONE]"

// maxSSELineBytes bounds a single event payload. vLLM chunks are small, but
// a malformed or hostile response must not be able to exhaust memory.
const maxSSELineBytes = 8 << 20 // 8 MiB

// sseReader decodes a text/event-stream body into its successive data payloads.
//
// It implements only the subset of the SSE grammar that OpenAI-compatible
// servers use: `data:` fields, blank-line event separation, and `:` comments.
// Multi-line data fields are joined with newlines per the specification.
type sseReader struct {
	sc   *bufio.Scanner
	data bytes.Buffer
}

// newSSEReader wraps an HTTP response body.
func newSSEReader(r io.Reader) *sseReader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxSSELineBytes)
	return &sseReader{sc: sc}
}

// Next returns the next event payload. It returns io.EOF when the stream
// ends, either by closing or by the [DONE] sentinel.
func (r *sseReader) Next() ([]byte, error) {
	r.data.Reset()
	for r.sc.Scan() {
		line := strings.TrimRight(r.sc.Text(), "\r")

		// A blank line dispatches the accumulated event.
		if line == "" {
			if r.data.Len() == 0 {
				continue // stray separator; keep reading
			}
			payload := strings.TrimSpace(r.data.String())
			r.data.Reset()
			if payload == sseDoneSentinel {
				return nil, io.EOF
			}
			return []byte(payload), nil
		}

		// Comments and heartbeats.
		if strings.HasPrefix(line, ":") {
			continue
		}

		field, value, found := strings.Cut(line, ":")
		if !found {
			// A field with no colon has an empty value; nothing to collect.
			continue
		}
		// A single leading space after the colon is part of the framing.
		value = strings.TrimPrefix(value, " ")

		if field != "data" {
			// event:, id: and retry: carry no payload we act on.
			continue
		}
		if r.data.Len() > 0 {
			r.data.WriteByte('\n')
		}
		r.data.WriteString(value)
	}

	if err := r.sc.Err(); err != nil {
		if err == bufio.ErrTooLong {
			return nil, fmt.Errorf("server-sent event exceeded %d bytes", maxSSELineBytes)
		}
		return nil, fmt.Errorf("read event stream: %w", err)
	}

	// The body ended without a trailing blank line; flush what we have.
	if r.data.Len() > 0 {
		payload := strings.TrimSpace(r.data.String())
		r.data.Reset()
		if payload != sseDoneSentinel && payload != "" {
			return []byte(payload), nil
		}
	}
	return nil, io.EOF
}
