// Package gortk compresses shell-command output before it reaches an LLM
// context window. It is a Go-native take on rtk (the Rust "token killer"),
// built for embedding inside an agent runtime rather than shelling out to an
// external binary.
//
// Design principles, in order of priority:
//
//  1. Lossless-by-default. The generic path only bounds size; it never drops
//     signal. A command without a dedicated filter is always safe to pass
//     through gortk.
//  2. Failure-preserving. Per-command filters drop *known noise* (progress
//     bars, "ok" lines, dependency download chatter) but never the lines an
//     agent needs to act on (failures, errors, file:line locations).
//  3. Honest about loss. Whenever a filter drops or truncates anything, it
//     records it in Truncation so the caller — and the agent — knows the view
//     is partial. This mirrors codefly's TestTruncation proto.
//
// The unit of work is a Command (what ran + what it produced) and the result
// is a Result (a compressed view + truncation metadata).
package gortk

import (
	"strings"
)

// Command is the input to compression: a finished command invocation and the
// bytes it produced. Stdout/Stderr are kept separate because most filters care
// about the distinction (e.g. test runners write results to stdout, diagnostics
// to stderr).
type Command struct {
	Name     string   // argv[0], e.g. "go", "git", "golangci-lint"
	Args     []string // argv[1:], e.g. ["test", "./..."]
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// Sub reports the first positional argument (the subcommand), e.g. "test" for
// `go test ./...`. Flags are skipped. Returns "" if there is none.
func (c Command) Sub() string {
	for _, a := range c.Args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

// Result is the compressed view of a Command's output plus a record of what was
// lost producing it.
type Result struct {
	// Text is the compressed output to hand to the model.
	Text string

	// Filter is the name of the filter that produced this Result, or "passthrough"
	// when no dedicated filter matched.
	Filter string

	// Truncation records what (if anything) was dropped. The zero value means
	// nothing was lost — the Text is a complete, faithful view.
	Truncation Truncation

	// Records carries structured output when a structured stage (a log spec)
	// produced it: one entry per surviving line, with its level and parsed
	// fields. nil for text-only filters. This is the "structured data out" path
	// — a caller can route records to a logger or consume fields directly
	// instead of (or alongside) reading Text.
	Records []Record
}

// Record is one parsed line of structured output: a canonical severity level,
// the named fields extracted from it, and the rendered text. Produced by log
// specs and reusable by streaming consumers (see LogParser).
type Record struct {
	// Level is the canonical severity: debug|info|warn|error|fatal (or "" when
	// the line carries no level).
	Level string
	// Fields are the named captures / parsed values for the line. Always
	// includes "msg" (the message) and "level".
	Fields map[string]any
	// Text is the rendered line (per the spec's template).
	Text string
}

// Lossless reports whether the Result preserved everything (nothing dropped or
// truncated).
func (r Result) Lossless() bool {
	return !r.Truncation.Happened
}

// Truncation describes loss introduced during compression. It is deliberately
// shaped like codefly's TestTruncation proto so callers can surface the same
// "this view is partial" signal they already do.
type Truncation struct {
	Happened bool

	// DroppedLines counts whole lines removed as known noise.
	DroppedLines int

	// DroppedBytes counts bytes removed by size bounding.
	DroppedBytes int

	// Note is a short human/agent-readable explanation, e.g.
	// "kept 3 failing tests, dropped 412 passing".
	Note string
}

func (t *Truncation) dropLines(n int, note string) {
	if n <= 0 {
		return
	}
	t.Happened = true
	t.DroppedLines += n
	if note != "" {
		t.Note = note
	}
}

func (t *Truncation) dropBytes(n int, note string) {
	if n <= 0 {
		return
	}
	t.Happened = true
	t.DroppedBytes += n
	if note != "" {
		t.Note = note
	}
}

// Filter compresses the output of one family of commands.
//
// Implementations should be pure (no I/O, no global state) so they are trivial
// to unit-test against captured fixtures.
type Filter interface {
	// Name identifies the filter in Result.Filter and in logs.
	Name() string

	// Match reports whether this filter handles the given command. It is given
	// the program name and args so it can key off subcommands
	// (e.g. only `go test`, not every `go` invocation).
	Match(name string, args []string) bool

	// Apply produces the compressed Result. It is only called when Match
	// returned true.
	Apply(cmd Command) Result
}
