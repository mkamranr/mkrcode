// Package redact removes credential-shaped strings from tool output before
// that output enters the conversation sent to the model.
//
// The boundary matters: redaction runs between tool execution and the
// message history, so a secret read off disk never reaches the prompt, the
// transcript, or the audit log. The audit log records that a redaction
// happened and of what kind, never the value.
//
// Detection is heuristic. It is a meaningful reduction in exposure, not a
// guarantee, and it is not a substitute for keeping credentials out of the
// workspace.
package redact

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Kind names a class of secret.
type Kind string

// The classes recognised.
const (
	KindPrivateKey    Kind = "private_key"
	KindAWSAccessKey  Kind = "aws_access_key"
	KindAWSSecretKey  Kind = "aws_secret_key"
	KindGitHubToken   Kind = "github_token"
	KindSlackToken    Kind = "slack_token"
	KindGoogleAPIKey  Kind = "google_api_key"
	KindJWT           Kind = "jwt"
	KindBearerToken   Kind = "bearer_token"
	KindConnectionPwd Kind = "connection_password"
	KindAssignedCred  Kind = "assigned_credential"
	KindAzureSAS      Kind = "azure_sas"
	KindBase64Cert    Kind = "certificate"
)

// Finding records one redaction, without the secret.
type Finding struct {
	Kind Kind
	// Offset is the byte offset in the original text.
	Offset int
	// Length is the number of bytes replaced.
	Length int
}

// rule is one detector.
type rule struct {
	kind Kind
	re   *regexp.Regexp
	// group is the submatch index to replace. Zero replaces the whole
	// match; a positive value replaces only the secret, keeping the
	// surrounding key name readable so the model still understands the
	// shape of what it read.
	group int
}

// rules are ordered from most to least specific. A private key block must
// be matched before any pattern that could match a fragment of one.
var rules = []rule{
	{KindPrivateKey, regexp.MustCompile(`(?s)-----BEGIN[ A-Z]*PRIVATE KEY-----.*?-----END[ A-Z]*PRIVATE KEY-----`), 0},
	{KindBase64Cert, regexp.MustCompile(`(?s)-----BEGIN CERTIFICATE-----.*?-----END CERTIFICATE-----`), 0},

	// Provider tokens with distinctive prefixes.
	{KindGitHubToken, regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}\b`), 0},
	{KindSlackToken, regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}\b`), 0},
	{KindGoogleAPIKey, regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`), 0},
	{KindAWSAccessKey, regexp.MustCompile(`\b(?:AKIA|ASIA|AGPA|AIDA|AROA|ANPA|ANVA)[0-9A-Z]{16}\b`), 0},

	// A JWT is three base64url segments separated by dots.
	{KindJWT, regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`), 0},

	// Azure shared access signatures.
	{KindAzureSAS, regexp.MustCompile(`(?i)\bsig=([A-Za-z0-9%+/=]{20,})`), 1},

	// Header and scheme forms.
	{KindBearerToken, regexp.MustCompile(`(?i)\b(?:authorization\s*:\s*)?bearer\s+([A-Za-z0-9._\-+/=]{16,})`), 1},

	// AWS secret keys are only identifiable by their key name.
	{KindAWSSecretKey, regexp.MustCompile(`(?i)\baws_secret_access_key\b\s*[:=]\s*["']?([A-Za-z0-9/+=]{30,})["']?`), 1},

	// Connection strings.
	{KindConnectionPwd, regexp.MustCompile(`(?i)\b(?:password|pwd)\s*=\s*([^;\s"']{4,})`), 1},

	// Generic assignment of a credential-named variable. Deliberately last
	// and deliberately narrow: it requires a credential-shaped name and a
	// value long enough not to be a placeholder.
	{KindAssignedCred, regexp.MustCompile(`(?i)\b[A-Z0-9_]*(?:SECRET|PASSWORD|PASSWD|TOKEN|APIKEY|API_KEY|PRIVATE_KEY|ACCESS_KEY|CLIENT_SECRET)[A-Z0-9_]*\b\s*[:=]\s*["']?([^\s"',;]{8,})["']?`), 1},
}

// placeholders are values that look like secrets but carry no information,
// so redacting them only makes output harder to read.
var placeholders = map[string]bool{
	"changeme": true, "password": true, "your_password": true, "xxxxxxxx": true,
	"placeholder": true, "redacted": true, "example": true, "none": true,
	"null": true, "todo": true, "secret": true, "your-token-here": true,
	"<password>": true, "${password}": true, "your_api_key": true,
}

// Redactor scrubs text.
type Redactor struct {
	enabled bool
}

// New returns a Redactor. When enabled is false, Redact is a no-op, which
// is what the audited -no-redact flag selects.
func New(enabled bool) *Redactor { return &Redactor{enabled: enabled} }

// Enabled reports whether redaction is active.
func (r *Redactor) Enabled() bool { return r.enabled }

// Redact returns s with detected secrets replaced, and the findings.
func (r *Redactor) Redact(s string) (string, []Finding) {
	if !r.enabled || s == "" {
		return s, nil
	}

	type span struct {
		start, end int
		kind       Kind
	}
	var spans []span

	for _, rule := range rules {
		for _, m := range rule.re.FindAllStringSubmatchIndex(s, -1) {
			start, end := m[0], m[1]
			if rule.group > 0 && len(m) > 2*rule.group+1 && m[2*rule.group] >= 0 {
				start, end = m[2*rule.group], m[2*rule.group+1]
			}
			if start < 0 || end <= start {
				continue
			}
			if placeholders[strings.ToLower(strings.Trim(s[start:end], `"'`))] {
				continue
			}
			spans = append(spans, span{start, end, rule.kind})
		}
	}
	if len(spans) == 0 {
		return s, nil
	}

	// Earlier rules are more specific, so on overlap the first wins.
	sort.SliceStable(spans, func(i, j int) bool {
		if spans[i].start != spans[j].start {
			return spans[i].start < spans[j].start
		}
		return spans[i].end > spans[j].end
	})

	var (
		b        strings.Builder
		findings []Finding
		last     int
	)
	for _, sp := range spans {
		if sp.start < last {
			continue // already covered by a more specific match
		}
		b.WriteString(s[last:sp.start])
		fmt.Fprintf(&b, "[REDACTED:%s]", sp.kind)
		findings = append(findings, Finding{Kind: sp.kind, Offset: sp.start, Length: sp.end - sp.start})
		last = sp.end
	}
	b.WriteString(s[last:])
	return b.String(), findings
}

// Summary renders findings for the operator and the audit log, counting by
// kind and never reproducing a value.
func Summary(findings []Finding) string {
	if len(findings) == 0 {
		return ""
	}
	counts := map[Kind]int{}
	for _, f := range findings {
		counts[f.Kind]++
	}
	kinds := make([]string, 0, len(counts))
	for k, n := range counts {
		kinds = append(kinds, fmt.Sprintf("%s x%d", k, n))
	}
	sort.Strings(kinds)
	return "redacted " + strings.Join(kinds, ", ")
}
