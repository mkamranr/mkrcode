package netguard

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewRejectsMalformedHost(t *testing.T) {
	for _, in := range []string{"", "   ", "gpu-01", "http://gpu-01:8000", ":8000", "gpu-01:"} {
		if _, err := New(in); err == nil {
			t.Errorf("New(%q) succeeded, want an error", in)
		}
	}
}

func TestAllowedExactMatch(t *testing.T) {
	g, err := New("gpu-01:8000")
	if err != nil {
		t.Fatal(err)
	}
	if !g.Allowed("gpu-01:8000") {
		t.Error("the configured host must be allowed")
	}
	if !g.Allowed("GPU-01:8000") {
		t.Error("host comparison must be case-insensitive")
	}
}

// The security property: everything that is not the endpoint is refused.
func TestBlocksEverythingElse(t *testing.T) {
	g, err := New("gpu-01:8000")
	if err != nil {
		t.Fatal(err)
	}
	blocked := []string{
		"evil.example.com:443",
		"gpu-01:9000",          // right host, wrong port
		"gpu-01.evil.com:8000", // suffix-confusion attempt
		"127.0.0.1:8000",
		"169.254.169.254:80", // cloud metadata endpoint
		"localhost:8000",
		"[::1]:8000",
	}
	for _, addr := range blocked {
		if g.Allowed(addr) {
			t.Errorf("Allowed(%q) = true, want false", addr)
		}
		_, err := g.DialContext(context.Background(), "tcp", addr)
		if err == nil {
			t.Errorf("DialContext(%q) succeeded, want a refusal", addr)
			continue
		}
		if !errors.Is(err, ErrBlocked) {
			t.Errorf("DialContext(%q) error = %v, want ErrBlocked", addr, err)
		}
		if !strings.Contains(err.Error(), addr) {
			t.Errorf("error should name the attempted destination, got %q", err)
		}
	}
}

// An end-to-end check that the client reaches the permitted server and
// refuses a second one, with no test hooks in the path.
func TestClientReachesOnlyTheAllowedServer(t *testing.T) {
	allowed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer allowed.Close()
	forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("should never be reached"))
	}))
	defer forbidden.Close()

	host := strings.TrimPrefix(allowed.URL, "http://")
	client, guard, err := NewClient(host, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if guard.AllowedHost() == "" {
		t.Error("AllowedHost should report the permitted destination")
	}

	resp, err := client.Get(allowed.URL)
	if err != nil {
		t.Fatalf("request to the allowed endpoint failed: %v", err)
	}
	resp.Body.Close()

	if _, err := client.Get(forbidden.URL); err == nil {
		t.Fatal("request to a forbidden endpoint succeeded; egress lock is not holding")
	} else if !strings.Contains(err.Error(), "netguard") {
		t.Errorf("error = %v, want a netguard refusal", err)
	}
}

// A redirect is a standard way to walk a permitted request onto a
// forbidden host, so it must be refused explicitly.
func TestRedirectsAreRefused(t *testing.T) {
	var target string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target, http.StatusFound)
	}))
	defer srv.Close()
	target = "http://evil.example.com/steal"

	host := strings.TrimPrefix(srv.URL, "http://")
	client, _, err := NewClient(host, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Get(srv.URL); err == nil {
		t.Fatal("a redirect off the allowed host was followed")
	}
}

// A hostname endpoint must keep working once resolved to an IP, without
// that widening the policy to other ports.
func TestResolvedLoopbackIsAllowedForConfiguredHostname(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	g, err := New("localhost:" + port)
	if err != nil {
		t.Fatal(err)
	}
	if !g.Allowed("127.0.0.1:" + port) {
		t.Error("an IP that the configured hostname resolves to must be allowed")
	}
	if g.Allowed("127.0.0.1:1") {
		t.Error("resolution must not widen the policy to other ports")
	}
}

// Every hosted OpenAI-compatible endpoint is HTTPS, so the restricted
// transport has to work over TLS, not only over plaintext. These tests use
// the real NewClient path rather than a hand-built transport, so a change to
// how the client is constructed is caught here.
func TestHTTPSToAllowedHostSucceeds(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil {
			t.Error("request did not arrive over TLS")
		}
		w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "https://")
	client, _, err := NewClient(host, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	trustTestServer(t, client, srv)

	resp, err := client.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatalf("HTTPS request to the allowed host failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// The destination check must happen at dial time, before any TLS handshake,
// so a blocked host is refused without a connection being established.
func TestHTTPSToBlockedHostIsRefusedBeforeHandshake(t *testing.T) {
	allowed := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer allowed.Close()
	forbidden := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the forbidden server was reached")
	}))
	defer forbidden.Close()

	host := strings.TrimPrefix(allowed.URL, "https://")
	client, _, err := NewClient(host, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	trustTestServer(t, client, allowed)

	_, err = client.Get(forbidden.URL)
	if err == nil {
		t.Fatal("an HTTPS request to a forbidden host succeeded")
	}
	if !errors.Is(err, ErrBlocked) {
		t.Errorf("err = %v, want ErrBlocked", err)
	}
}

// A hostname endpoint on the default HTTPS port must be permitted without
// the port having to be written out, since that is how people configure a
// hosted API.
func TestImplicitHTTPSPortIsAllowed(t *testing.T) {
	g, err := New("api.example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	if !g.Allowed("api.example.com:443") {
		t.Error("the configured host on 443 must be allowed")
	}
	if g.Allowed("api.example.com:80") {
		t.Error("a different port must not be allowed")
	}
	if g.Allowed("other.example.com:443") {
		t.Error("a different host must not be allowed")
	}
}

// trustTestServer adds the test server's self-signed certificate to the
// restricted client, so the TLS path is exercised for real rather than
// being skipped with InsecureSkipVerify.
func trustTestServer(t *testing.T, client *http.Client, srv *httptest.Server) {
	t.Helper()
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client transport is %T, want *http.Transport", client.Transport)
	}
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	tr.TLSClientConfig = &tls.Config{RootCAs: pool}
}
