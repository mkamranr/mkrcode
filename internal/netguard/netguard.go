// Package netguard constructs the only http.Client the binary is permitted
// to use.
//
// The agent runs inside an air-gapped enclave where the sole legitimate
// network destination is the vLLM endpoint. Rather than trusting policy,
// the dialer refuses any other destination outright, so a prompt injection
// that convinces the model to fetch an external URL still cannot reach one.
//
// Scope, stated plainly: this constrains the agent process. It does not
// constrain commands the operator approves through the exec tool, which
// inherit the user's own network access. That gap is covered by the
// permission layer's default deny rules and, where genuine enforcement is
// required, by host firewall policy outside this program.
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ErrBlocked is returned when a dial targets anything but the allowed host.
var ErrBlocked = errors.New("netguard: destination blocked")

// BlockedError describes a refused destination.
type BlockedError struct {
	Attempted string
	Allowed   string
}

func (e *BlockedError) Error() string {
	return fmt.Sprintf("netguard: refused to dial %s; this build may only reach %s", e.Attempted, e.Allowed)
}

// Is lets errors.Is(err, ErrBlocked) match.
func (e *BlockedError) Is(target error) bool { return target == ErrBlocked }

// Guard holds the single permitted destination and the resolved addresses
// that satisfy it.
type Guard struct {
	// allowedHost is the canonical "host:port" from the configured endpoint.
	allowedHost string

	mu      sync.RWMutex
	allowed map[string]struct{} // resolved "ip:port" forms
}

// New returns a Guard permitting exactly one host:port.
func New(hostPort string) (*Guard, error) {
	hostPort = strings.TrimSpace(hostPort)
	if hostPort == "" {
		return nil, errors.New("netguard: an allowed host:port is required")
	}
	host, port, err := net.SplitHostPort(hostPort)
	if err != nil {
		return nil, fmt.Errorf("netguard: %q is not host:port: %w", hostPort, err)
	}
	if host == "" || port == "" {
		return nil, fmt.Errorf("netguard: %q is missing a host or port", hostPort)
	}
	g := &Guard{
		allowedHost: canonical(hostPort),
		allowed:     map[string]struct{}{canonical(hostPort): {}},
	}
	return g, nil
}

// canonical lowercases the host and strips an IPv6 zone so comparisons are
// stable across the forms Go hands the dialer.
func canonical(hostPort string) string {
	host, port, err := net.SplitHostPort(hostPort)
	if err != nil {
		return strings.ToLower(hostPort)
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	if i := strings.Index(host, "%"); i >= 0 {
		host = host[:i]
	}
	return net.JoinHostPort(host, port)
}

// Allowed reports whether addr may be dialled. It permits the configured
// host:port verbatim and any IP:port that host resolves to, so that a
// hostname endpoint keeps working without widening the policy.
func (g *Guard) Allowed(addr string) bool {
	c := canonical(addr)
	g.mu.RLock()
	_, ok := g.allowed[c]
	g.mu.RUnlock()
	if ok {
		return true
	}

	// The address may be a resolved IP for the allowed hostname. Resolve
	// once and cache the result, so a later dial to the same IP is cheap.
	host, port, err := net.SplitHostPort(c)
	if err != nil {
		return false
	}
	allowedHost, allowedPort, err := net.SplitHostPort(g.allowedHost)
	if err != nil || port != allowedPort {
		return false
	}
	ips, err := net.LookupHost(allowedHost)
	if err != nil {
		return false
	}
	for _, ip := range ips {
		if strings.EqualFold(ip, host) {
			g.mu.Lock()
			g.allowed[c] = struct{}{}
			g.mu.Unlock()
			return true
		}
	}
	return false
}

// AllowedHost returns the single permitted destination, for diagnostics.
func (g *Guard) AllowedHost() string { return g.allowedHost }

// DialContext is a net.Dialer-compatible dial function that refuses any
// destination other than the permitted one.
func (g *Guard) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if !g.Allowed(addr) {
		return nil, &BlockedError{Attempted: addr, Allowed: g.allowedHost}
	}
	d := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	return d.DialContext(ctx, network, addr)
}

// NewClient returns the http.Client the rest of the program must use. No
// other part of the binary constructs a Transport; that invariant is what
// makes the restriction meaningful.
func NewClient(hostPort string, timeout time.Duration) (*http.Client, *Guard, error) {
	g, err := New(hostPort)
	if err != nil {
		return nil, nil, err
	}
	tr := &http.Transport{
		DialContext:           g.DialContext,
		MaxIdleConns:          8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// Proxies would route around the destination check entirely.
		Proxy: nil,
	}
	c := &http.Client{
		Transport: tr,
		Timeout:   timeout,
		// Redirects are a classic way to walk a permitted request to a
		// forbidden host. The dialer would catch it, but refusing here
		// produces a far clearer error.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return fmt.Errorf("netguard: refused to follow a redirect to %s", req.URL.Host)
		},
	}
	return c, g, nil
}
