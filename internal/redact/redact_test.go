package redact

import (
	"strings"
	"testing"
)

// The corpus is realistically shaped so the detectors are exercised the way
// they will be in a real repository.
func TestRedactsRealisticSecrets(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		secret string
		kind   Kind
	}{
		{
			"private key block",
			"config:\n-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA3Tz2mr7SZiAMfQyuvBjM9Oi\n-----END RSA PRIVATE KEY-----\ndone",
			"MIIEowIBAAKCAQEA3Tz2mr7SZiAMfQyuvBjM9Oi",
			KindPrivateKey,
		},
		{
			"aws access key",
			"AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE",
			"AKIAIOSFODNN7EXAMPLE",
			KindAWSAccessKey,
		},
		{
			"aws secret key",
			"aws_secret_access_key = wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
			"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
			KindAWSSecretKey,
		},
		{
			"github token",
			"git remote set-url origin https://ghp_1234567890abcdefghijklmnopqrstuvwxyz@github.com/x/y",
			"ghp_1234567890abcdefghijklmnopqrstuvwxyz",
			KindGitHubToken,
		},
		{
			"slack token",
			"SLACK=xoxb-123456789012-abcdefghijklmnop",
			"xoxb-123456789012-abcdefghijklmnop",
			KindSlackToken,
		},
		{
			"google api key",
			"key: AIzaSyD-1234567890abcdefghijklmnopqrstu",
			"AIzaSyD-1234567890abcdefghijklmnopqrstu",
			KindGoogleAPIKey,
		},
		{
			"jwt",
			"Authorization header used eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk",
			"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9",
			KindJWT,
		},
		{
			"bearer token",
			"curl -H 'Authorization: Bearer sk_live_abcdef1234567890ABCDEF'",
			"sk_live_abcdef1234567890ABCDEF",
			KindBearerToken,
		},
		{
			"connection string password",
			"Server=db01;Database=app;User Id=sa;Password=P@ssw0rd!Long;",
			"P@ssw0rd!Long",
			KindConnectionPwd,
		},
		{
			"env assignment",
			"DB_PASSWORD=sup3rs3cr3tvalue\nOTHER=fine",
			"sup3rs3cr3tvalue",
			KindAssignedCred,
		},
		{
			"client secret",
			`CLIENT_SECRET: "Xq9-vBn2LkPzR4tYuIoP"`,
			"Xq9-vBn2LkPzR4tYuIoP",
			KindAssignedCred,
		},
		{
			"azure sas",
			"https://acct.blob.core.windows.net/c?sv=2021&sig=abcdefghijklmnopqrstuvwxyz1234567890%3D",
			"abcdefghijklmnopqrstuvwxyz1234567890%3D",
			KindAzureSAS,
		},
	}

	r := New(true)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, findings := r.Redact(c.input)
			if strings.Contains(out, c.secret) {
				t.Errorf("secret survived redaction.\ninput:  %s\noutput: %s", c.input, out)
			}
			if len(findings) == 0 {
				t.Fatalf("no findings reported for %s", c.name)
			}
			if !strings.Contains(out, "[REDACTED:") {
				t.Errorf("no redaction marker in output: %s", out)
			}
		})
	}
}

// The whole point of the boundary: nothing from the corpus may survive into
// the text that would be sent to the model.
func TestNoSecretSurvivesTheCorpus(t *testing.T) {
	secrets := []string{
		"AKIAIOSFODNN7EXAMPLE",
		"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		"ghp_1234567890abcdefghijklmnopqrstuvwxyz",
		"xoxb-123456789012-abcdefghijklmnop",
		"sup3rs3cr3tvalue",
		"P@ssw0rd!Long",
	}
	doc := `# config
AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE
aws_secret_access_key = wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY
GITHUB_TOKEN=ghp_1234567890abcdefghijklmnopqrstuvwxyz
SLACK_TOKEN=xoxb-123456789012-abcdefghijklmnop
DB_PASSWORD=sup3rs3cr3tvalue
conn = "Server=db;Password=P@ssw0rd!Long;"
`
	out, findings := New(true).Redact(doc)
	for _, s := range secrets {
		if strings.Contains(out, s) {
			t.Errorf("secret %q survived:\n%s", s, out)
		}
	}
	if len(findings) < len(secrets) {
		t.Errorf("found %d secrets, want at least %d", len(findings), len(secrets))
	}
}

// Redaction must not damage ordinary source code, or the agent becomes
// useless on real repositories.
func TestOrdinaryCodeIsUntouched(t *testing.T) {
	code := `package main

import "fmt"

// tokenCount returns the number of tokens.
func tokenCount(s string) int {
	password := promptForPassword()
	fmt.Println("api_key is required")
	return len(s)
}

const maxTokens = 4096
var SECRET_NAME = ""
`
	out, findings := New(true).Redact(code)
	if out != code {
		t.Errorf("ordinary code was modified.\nwant:\n%s\ngot:\n%s", code, out)
	}
	if len(findings) != 0 {
		t.Errorf("false positives in ordinary code: %v", findings)
	}
}

// Placeholder values carry no information and redacting them only makes
// output harder to read.
func TestPlaceholdersAreNotRedacted(t *testing.T) {
	for _, in := range []string{
		"PASSWORD=changeme",
		"API_KEY=your_api_key",
		"DB_PASSWORD=placeholder",
		"TOKEN=REDACTED",
	} {
		out, _ := New(true).Redact(in)
		if strings.Contains(out, "[REDACTED:") {
			t.Errorf("placeholder was redacted: %q -> %q", in, out)
		}
	}
}

// Overlapping detectors must produce one clean replacement, not nested
// markers.
func TestOverlappingMatchesProduceOneMarker(t *testing.T) {
	in := "AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	out, _ := New(true).Redact(in)
	if n := strings.Count(out, "[REDACTED:"); n != 1 {
		t.Errorf("got %d markers, want 1: %s", n, out)
	}
}

func TestDisabledIsANoOp(t *testing.T) {
	in := "AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE"
	out, findings := New(false).Redact(in)
	if out != in {
		t.Errorf("disabled redactor modified the text: %q", out)
	}
	if findings != nil {
		t.Errorf("disabled redactor reported findings: %v", findings)
	}
}

func TestSummaryCountsWithoutRevealing(t *testing.T) {
	_, findings := New(true).Redact("A=AKIAIOSFODNN7EXAMPLE\nB=AKIAIOSFODNN7EXAMPLF\nDB_PASSWORD=longsecretvalue")
	s := Summary(findings)
	if !strings.Contains(s, "aws_access_key") {
		t.Errorf("summary = %q, want it to name the kind", s)
	}
	if strings.Contains(s, "AKIA") {
		t.Errorf("summary leaked a secret: %q", s)
	}
}

func TestEmptyInput(t *testing.T) {
	out, findings := New(true).Redact("")
	if out != "" || findings != nil {
		t.Error("empty input should pass through unchanged")
	}
}
