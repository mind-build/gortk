package gortk

import (
	"strings"
	"testing"
)

func TestRedactionPatterns(t *testing.T) {
	reg := New().WithRedaction() // passthrough + redaction
	cases := []struct {
		name      string
		in        string
		mustHide  string // substring that must NOT survive
		mustKeep  string // substring that must survive (signal preserved)
		wantMarks bool
	}{
		{"aws key", "key = AKIAIOSFODNN7EXAMPLE here", "AKIAIOSFODNN7EXAMPLE", "here", true},
		{"github token", "token ghp_" + strings.Repeat("a", 36), "ghp_aaaa", "", true},
		{"jwt", "auth eyJhbGciOiJIUzI1Ni007.eyJzdWIiOiIxMjM0NTY3.SflKxwRJSMeKKF2QT4", "eyJhbGci", "", true},
		{"env assignment", "PASSWORD=hunter2supersecret", "hunter2supersecret", "PASSWORD", true},
		{"json secret", `{"api_key": "abcd1234efgh5678"}`, "abcd1234efgh5678", "api_key", true},
		{"url creds", "cloning https://user:p4ssw0rd@github.com/x/y", "p4ssw0rd", "github.com", true},
		{"no secret", "all tests passed in 1.2s", "", "all tests passed", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := reg.Compress(Command{Name: "echo", Stdout: []byte(tc.in + "\n")})
			if tc.mustHide != "" && strings.Contains(res.Text, tc.mustHide) {
				t.Fatalf("secret survived: %q in %q", tc.mustHide, res.Text)
			}
			if tc.mustKeep != "" && !strings.Contains(res.Text, tc.mustKeep) {
				t.Fatalf("signal lost: %q missing from %q", tc.mustKeep, res.Text)
			}
			if got := res.Truncation.Masked > 0; got != tc.wantMarks {
				t.Fatalf("Masked>0 = %v, want %v (text %q)", got, tc.wantMarks, res.Text)
			}
		})
	}
}

func TestRedactionRunsOnPassthrough(t *testing.T) {
	// The whole point: a command with NO filter (env dump) is still scrubbed.
	reg := New().WithRedaction()
	res := reg.Compress(Command{
		Name:   "env",
		Stdout: []byte("HOME=/root\nAWS_SECRET_ACCESS_KEY=wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY\nPATH=/usr/bin\n"),
	})
	if strings.Contains(res.Text, "wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY") {
		t.Fatalf("env secret survived passthrough redaction:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "HOME=/root") || !strings.Contains(res.Text, "PATH=/usr/bin") {
		t.Fatalf("non-secret env lines were lost:\n%s", res.Text)
	}
}

func TestEntropyRedactionAllowlistsHashes(t *testing.T) {
	reg, err := New().WithRedactionOptions(RedactOptions{Entropy: true})
	if err != nil {
		t.Fatal(err)
	}
	// A git SHA and a UUID must survive (benign identifiers); an opaque
	// high-entropy blob must not.
	sha := "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b"
	uuid := "550e8400-e29b-41d4-a716-446655440000"
	secret := "Zk3Jq9XvB2mNpLwRtYuI0aSdFgHjKcVe7n"
	in := "commit " + sha + " id " + uuid + " key " + secret + "\n"
	res := reg.Compress(Command{Name: "echo", Stdout: []byte(in)})
	if !strings.Contains(res.Text, sha) {
		t.Fatalf("git sha was redacted by entropy scan:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, uuid) {
		t.Fatalf("uuid was redacted by entropy scan:\n%s", res.Text)
	}
	if strings.Contains(res.Text, secret) {
		t.Fatalf("high-entropy secret survived:\n%s", res.Text)
	}
}

func TestEntropyOffByDefault(t *testing.T) {
	reg := New().WithRedaction() // no entropy
	secret := "Zk3Jq9XvB2mNpLwRtYuI0aSdFgHjKcVe7n"
	res := reg.Compress(Command{Name: "echo", Stdout: []byte("blob " + secret + "\n")})
	if !strings.Contains(res.Text, secret) {
		t.Fatalf("entropy redaction fired without being enabled:\n%s", res.Text)
	}
}

func TestNormalize(t *testing.T) {
	reg := New().WithNormalize()
	in := "req 550e8400-e29b-41d4-a716-446655440000 at 2026-06-24T10:00:00Z from 10.0.0.1 sha 9f86d081884c\n"
	res := reg.Compress(Command{Name: "log", Stdout: []byte(in)})
	for _, marker := range []string{"<uuid>", "<ts>", "<ip>", "<hash>"} {
		if !strings.Contains(res.Text, marker) {
			t.Fatalf("missing %s after normalize:\n%s", marker, res.Text)
		}
	}
	if res.Truncation.Masked < 4 {
		t.Fatalf("Masked = %d, want >= 4", res.Truncation.Masked)
	}
}

func TestNormalizeKeepsDecimals(t *testing.T) {
	// Pure-decimal runs (counts, line numbers) must not become <hash>.
	reg := New().WithNormalize()
	res := reg.Compress(Command{Name: "x", Stdout: []byte("processed 1234567 records\n")})
	if !strings.Contains(res.Text, "1234567") {
		t.Fatalf("decimal number was normalized:\n%s", res.Text)
	}
}

func TestRedactionLosslessSemantics(t *testing.T) {
	// Masking preserves the line, so Lossless() stays true (only Masked is set).
	reg := New().WithRedaction()
	res := reg.Compress(Command{Name: "echo", Stdout: []byte("token=ghp_" + strings.Repeat("a", 36) + "\n")})
	if !res.Lossless() {
		t.Fatal("redaction should not flip Lossless() (no content dropped)")
	}
	if res.Truncation.Masked == 0 {
		t.Fatal("expected Masked > 0")
	}
}
