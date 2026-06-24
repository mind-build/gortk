package gortk

import (
	"math"
	"regexp"
	"strings"
)

// This file adds two registry-wide passes that run on EVERY result, including
// lossless passthrough — because the commands most likely to leak a secret
// (`env`, `printenv`, `cat .env`, `aws configure list`) are exactly the ones
// with no dedicated filter:
//
//   - redaction    masks credentials so they never enter the model's context.
//   - normalization collapses volatile high-cardinality tokens (UUIDs,
//     timestamps, hashes, IPs) to stable markers, cutting tokens and helping
//     dedup. It is lossy of identity, so it is opt-in and separate.
//
// Both are substitutions, not deletions: they replace a span with a marker, so
// the "something was here" signal survives. They are reported via
// Truncation.Masked, not Truncation.Happened.

// redactRule is one secret matcher. repl may reference capture groups ($1, ${2})
// so a rule can keep the key and mask only the value.
type redactRule struct {
	re   *regexp.Regexp
	repl string
}

// defaultRedactRules are high-precision credential patterns — chosen to almost
// never fire on ordinary build/test output. Ordered so that key=value masking
// runs before bare-token matchers (avoids double-masking an already-hidden
// value).
var defaultRedactRules = []redactRule{
	// PEM private key blocks (multi-line).
	{regexp.MustCompile(`(?s)-----BEGIN[ A-Z]*PRIVATE KEY-----.*?-----END[ A-Z]*PRIVATE KEY-----`), "[REDACTED:private-key]"},
	// URL embedded credentials: scheme://user:pass@host -> mask the password.
	{regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://[^/\s:@]+):[^/\s:@]+@`), "$1:[REDACTED]@"},
	// key=value / "key": "value" for secret-ish keys. The key may be a larger
	// identifier ending in a secret word (e.g. AWS_SECRET_ACCESS_KEY), so the
	// secret word is matched as a suffix rather than with a \b that underscores
	// would defeat. Groups: 1=full key, 3=separator, 4=value.
	{regexp.MustCompile(`(?i)([A-Za-z0-9_.\-]*(password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|secret[_-]?key|client[_-]?secret|auth[_-]?token|private[_-]?key))(["']?\s*[:=]\s*["']?)([^\s"',;]+)`), "${1}${3}[REDACTED]"},
	// Provider-specific bare tokens.
	{regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), "[REDACTED:aws-key]"},
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`), "[REDACTED:github-token]"},
	{regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}\b`), "[REDACTED:slack-token]"},
	{regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`), "[REDACTED:google-key]"},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]+\.eyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+`), "[REDACTED:jwt]"},
	{regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._\-]{8,}`), "Bearer [REDACTED]"},
}

// RedactOptions tunes the redactor. The zero value (used by WithRedaction) is
// the high-precision pattern set with no entropy scanning.
type RedactOptions struct {
	// Entropy turns on the catch-all high-entropy token scanner. It is more
	// aggressive and can mis-fire on opaque-but-harmless tokens, so it is off by
	// default; the allowlist (UUIDs, hex hashes) keeps common identifiers safe.
	Entropy bool
	// MinEntropyLen is the shortest token the entropy scanner considers. 0 -> 24.
	MinEntropyLen int
	// EntropyThreshold is the minimum Shannon entropy (bits/char) to redact. 0 -> 4.0.
	EntropyThreshold float64
	// Extra are additional regexes; each whole match is replaced with [REDACTED].
	Extra []string
}

// Redactor masks credentials in text. Build one with WithRedaction /
// WithRedactionOptions; it is immutable and safe for concurrent use.
type Redactor struct {
	rules            []redactRule
	entropy          bool
	minEntropyLen    int
	entropyThreshold float64
}

func newRedactor(opts RedactOptions) (*Redactor, error) {
	r := &Redactor{
		rules:            defaultRedactRules,
		entropy:          opts.Entropy,
		minEntropyLen:    opts.MinEntropyLen,
		entropyThreshold: opts.EntropyThreshold,
	}
	if r.minEntropyLen == 0 {
		r.minEntropyLen = 24
	}
	if r.entropyThreshold == 0 {
		r.entropyThreshold = 4.0
	}
	for _, p := range opts.Extra {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, err
		}
		r.rules = append(r.rules, redactRule{re: re, repl: "[REDACTED]"})
	}
	return r, nil
}

// redact masks secrets in text and returns the new text and the number of
// substitutions made.
func (r *Redactor) redact(text string) (string, int) {
	count := 0
	for _, rule := range r.rules {
		matches := rule.re.FindAllStringIndex(text, -1)
		if len(matches) == 0 {
			continue
		}
		count += len(matches)
		text = rule.re.ReplaceAllString(text, rule.repl)
	}
	if r.entropy {
		var n int
		text, n = r.redactEntropy(text)
		count += n
	}
	return text, count
}

// entropyTokenRE finds base64/hex-ish runs that could be opaque secrets.
var entropyTokenRE = regexp.MustCompile(`[A-Za-z0-9+/=_\-]{16,}`)

func (r *Redactor) redactEntropy(text string) (string, int) {
	n := 0
	out := entropyTokenRE.ReplaceAllStringFunc(text, func(tok string) string {
		if len(tok) < r.minEntropyLen || isCommonIdentifier(tok) {
			return tok
		}
		if shannonEntropy(tok) >= r.entropyThreshold {
			n++
			return "[REDACTED:high-entropy]"
		}
		return tok
	})
	return out, n
}

// isCommonIdentifier reports whether tok is a benign high-cardinality identifier
// (UUID or hex digest) that the entropy scanner should leave alone — these read
// as random but are routinely safe to show.
func isCommonIdentifier(tok string) bool {
	if uuidRE.MatchString(tok) {
		return true
	}
	// Pure hex of a common digest length (git sha, md5, sha1, sha256, ...).
	if len(tok) >= 7 && len(tok) <= 64 && isHex(tok) {
		return true
	}
	return false
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// shannonEntropy returns the Shannon entropy of s in bits per character.
func shannonEntropy(s string) float64 {
	var freq [256]float64
	for i := 0; i < len(s); i++ {
		freq[s[i]]++
	}
	n := float64(len(s))
	h := 0.0
	for _, c := range freq {
		if c == 0 {
			continue
		}
		p := c / n
		h -= p * math.Log2(p)
	}
	return h
}

// --- normalization ---------------------------------------------------------

var (
	uuidRE = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)
	tsRE   = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+\-]\d{2}:?\d{2})?\b`)
	ipv4RE = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	hexRE  = regexp.MustCompile(`\b[0-9a-fA-F]{7,64}\b`)
)

// normalizeText replaces volatile high-cardinality tokens with stable markers,
// returning the new text and the number of substitutions. Order matters: UUIDs
// before bare hex (a UUID's segments are hex), timestamps before their numeric
// pieces can be touched.
func normalizeText(text string) (string, int) {
	n := 0
	repl := func(re *regexp.Regexp, marker string) {
		matches := re.FindAllStringIndex(text, -1)
		if len(matches) > 0 {
			n += len(matches)
			text = re.ReplaceAllString(text, marker)
		}
	}
	repl(uuidRE, "<uuid>")
	repl(tsRE, "<ts>")
	repl(ipv4RE, "<ip>")
	// Hex digests, but skip pure-decimal runs (line numbers, counts) by requiring
	// at least one hex letter — done in a func since RE2 has no lookahead.
	text = hexRE.ReplaceAllStringFunc(text, func(tok string) string {
		if strings.ContainsAny(tok, "abcdefABCDEF") {
			n++
			return "<hash>"
		}
		return tok
	})
	return text, n
}
