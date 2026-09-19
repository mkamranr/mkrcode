package provider

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mkamranr/mkrcode/internal/netguard"
	"github.com/mkamranr/mkrcode/internal/provider/mock"
)

// tlsMock wraps the scriptable mock server in TLS, so the provider and the
// probe are exercised over https exactly as they are over http.
func tlsMock(t *testing.T, turns ...mock.Turn) (*httptest.Server, []byte) {
	t.Helper()
	inner := mock.New(turns...)
	t.Cleanup(inner.Close)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil {
			t.Error("request did not arrive over TLS")
		}
		req := r.Clone(r.Context())
		req.RequestURI = ""
		req.URL.Scheme = "http"
		req.URL.Host = inner.Host()
		resp, err := http.DefaultTransport.RoundTrip(req)
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
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				w.Write(buf[:n])
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
			if err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	// The test authority, in the form an operator would supply it.
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	return srv, caPEM
}

// httpsClient builds the production client, trusting the test authority the
// same way a real internal CA would be trusted.
func httpsClient(t *testing.T, srv *httptest.Server, caPEM []byte) *Client {
	t.Helper()
	host := strings.TrimPrefix(srv.URL, "https://")
	hc, _, err := netguard.NewClient(netguard.Options{
		AllowedHost:  host,
		Timeout:      30 * time.Second,
		ExtraRootCAs: caPEM,
	})
	if err != nil {
		t.Fatalf("netguard: %v", err)
	}
	c, err := NewClient(srv.URL, "", hc)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The whole client path must work over https, not only the dialer.
func TestProviderWorksOverHTTPS(t *testing.T) {
	srv, caPEM := tlsMock(t, mock.Turn{
		Text:      "reading the file. ",
		ToolCalls: []mock.ToolCall{{ID: "c1", Name: "read_file", Args: `{"path":"main.go"}`}},
	})
	c := httpsClient(t, srv, caPEM)

	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models over https: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("no models returned")
	}

	var text strings.Builder
	var calls []ToolCall
	err = c.Stream(context.Background(), ChatRequest{Model: "m"}, func(ev Event) error {
		switch ev.Kind {
		case EventText:
			text.WriteString(ev.Text)
		case EventToolCall:
			calls = append(calls, ev.ToolCall)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Stream over https: %v", err)
	}
	if text.String() != "reading the file. " {
		t.Errorf("text = %q", text.String())
	}
	if len(calls) != 1 || calls[0].Function.Name != "read_file" {
		t.Errorf("tool calls did not survive the TLS path: %+v", calls)
	}
}

// The capability probe must work over https too; it is the first thing an
// operator runs.
func TestProbeWorksOverHTTPS(t *testing.T) {
	srv, caPEM := tlsMock(t)
	caps, err := Probe(context.Background(), httpsClient(t, srv, caPEM), "auto", "")
	if err != nil {
		t.Fatalf("Probe over https: %v", err)
	}
	if caps.Adapter != "native" {
		t.Errorf("adapter = %q, want native", caps.Adapter)
	}
	if caps.Model == "" {
		t.Error("no model reported")
	}
}

// http must keep working; adding TLS support must not regress the default
// deployment, which is plain HTTP inside the enclave.
func TestProviderStillWorksOverHTTP(t *testing.T) {
	s := mock.New(mock.Turn{Text: "plain http still works"})
	defer s.Close()

	var got strings.Builder
	err := newTestClient(t, s).Stream(context.Background(), ChatRequest{Model: "m"}, func(ev Event) error {
		if ev.Kind == EventText {
			got.WriteString(ev.Text)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Stream over http: %v", err)
	}
	if got.String() != "plain http still works" {
		t.Errorf("text = %q", got.String())
	}
}

// Without the authority, an internal certificate must be rejected — and the
// error must say how to fix it rather than leaving an x509 message.
func TestUntrustedCertificateGivesActionableError(t *testing.T) {
	srv, _ := tlsMock(t)
	host := strings.TrimPrefix(srv.URL, "https://")
	hc, _, err := netguard.NewClient(netguard.Options{AllowedHost: host, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewClient(srv.URL, "", hc)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.Models(context.Background())
	if err == nil {
		t.Fatal("an untrusted certificate was accepted")
	}
	if !strings.Contains(err.Error(), "ca_cert") {
		t.Errorf("error should tell the operator how to supply the authority, got: %v", err)
	}
}

// Pointing https at a plaintext server is a common typo, and the raw error
// says nothing useful.
func TestHTTPSAgainstPlaintextServerIsExplained(t *testing.T) {
	s := mock.New()
	defer s.Close()

	hc, _, err := netguard.NewClient(netguard.Options{AllowedHost: s.Host(), Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately the wrong scheme for this server.
	c, err := NewClient("https://"+s.Host(), "", hc)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.Models(context.Background())
	if err == nil {
		t.Fatal("expected a failure")
	}
	if !strings.Contains(err.Error(), "http://") {
		t.Errorf("error should suggest the other scheme, got: %v", err)
	}
}

func TestExtraRootCAsRejectsGarbage(t *testing.T) {
	_, _, err := netguard.NewClient(netguard.Options{
		AllowedHost:  "gpu-01:8443",
		ExtraRootCAs: []byte("this is not a certificate"),
	})
	if err == nil {
		t.Fatal("a file containing no certificates must be refused")
	}
	if !strings.Contains(err.Error(), "PEM") {
		t.Errorf("error should explain the file is not PEM, got: %v", err)
	}
}

// Supplying an authority must extend the system store, not replace it, or a
// publicly-trusted endpoint would stop working the moment an internal CA is
// configured.
func TestExtraRootCAsExtendsRatherThanReplaces(t *testing.T) {
	srv, caPEM := tlsMock(t)
	host := strings.TrimPrefix(srv.URL, "https://")

	hc, _, err := netguard.NewClient(netguard.Options{AllowedHost: host, ExtraRootCAs: caPEM})
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := hc.Transport.(*http.Transport)
	if !ok || tr.TLSClientConfig == nil || tr.TLSClientConfig.RootCAs == nil {
		t.Fatal("no root pool was configured")
	}
	if tr.TLSClientConfig.MinVersion < tls.VersionTLS12 {
		t.Error("TLS 1.2 should be the minimum accepted version")
	}

	// The supplied authority must actually verify the server's certificate.
	// Subjects() is deprecated and returns nil for system pools on macOS and
	// Windows, so the pool's contents cannot be counted portably; verifying
	// a real certificate against it is the meaningful check.
	if _, err := srv.Certificate().Verify(x509.VerifyOptions{
		Roots:     tr.TLSClientConfig.RootCAs,
		DNSName:   "127.0.0.1",
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("the supplied authority does not verify the endpoint's certificate: %v", err)
	}
}
