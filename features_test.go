package gortk

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loudFilter is a test filter that always matches and always drops a line, so
// every result is lossy (exercises the Sink/observer paths).
type loudFilter struct{}

func (loudFilter) Name() string                       { return "loud" }
func (loudFilter) Match(name string, _ []string) bool { return base(name) == "loud" }
func (loudFilter) Apply(cmd Command) Result {
	r := Result{Text: "kept\n", Filter: "loud"}
	r.Truncation.dropLines(3, "dropped 3")
	return r
}

func TestSavingsAccounting(t *testing.T) {
	reg := New(loudFilter{})
	res := reg.Compress(Command{Name: "loud", Stdout: []byte("a\nb\nc\nkept\n")})

	if res.InputBytes != len("a\nb\nc\nkept\n") {
		t.Fatalf("InputBytes = %d", res.InputBytes)
	}
	if res.OutputBytes != len("kept\n") {
		t.Fatalf("OutputBytes = %d", res.OutputBytes)
	}
	if got := res.SavedBytes(); got != res.InputBytes-res.OutputBytes {
		t.Fatalf("SavedBytes = %d", got)
	}
	if f := res.SavedFraction(); f <= 0 || f >= 1 {
		t.Fatalf("SavedFraction = %v, want in (0,1)", f)
	}
}

func TestSavingsNeverNegative(t *testing.T) {
	// A filter whose Text is longer than the input must not report negative savings.
	res := Result{Text: "padded out", InputBytes: 2, OutputBytes: 10}
	if got := res.SavedBytes(); got != 0 {
		t.Fatalf("SavedBytes = %d, want 0", got)
	}
	if got := res.SavedFraction(); got != 0 {
		t.Fatalf("SavedFraction = %v, want 0", got)
	}
}

func TestStatsAggregate(t *testing.T) {
	stats := &Stats{}
	reg := New(loudFilter{}).Observe(stats.Observe)
	for range 5 {
		reg.Compress(Command{Name: "loud", Stdout: []byte("a\nb\nc\nkept\n")})
	}
	rep := stats.Report()
	if rep.Commands != 5 {
		t.Fatalf("Commands = %d, want 5", rep.Commands)
	}
	if rep.SavedBytes != rep.InputBytes-rep.OutputBytes {
		t.Fatalf("SavedBytes = %d", rep.SavedBytes)
	}
	if rep.DroppedLines != 15 {
		t.Fatalf("DroppedLines = %d, want 15", rep.DroppedLines)
	}
	if !strings.Contains(rep.String(), "% saved") {
		t.Fatalf("String() = %q", rep.String())
	}
}

func TestDiscovery(t *testing.T) {
	disc := &Discovery{}
	reg := Default().Observe(disc.Observe)

	// "loud" isn't a real command, so these pass through (no filter) -> discovered.
	reg.Compress(Command{Name: "frobnicate", Args: []string{"build"}, Stdout: []byte("x\n")})
	reg.Compress(Command{Name: "frobnicate", Args: []string{"build"}, Stdout: []byte("y\n")})
	reg.Compress(Command{Name: "wibble", Stdout: []byte("z\n")})
	// A matched command must NOT be discovered.
	reg.Compress(Command{Name: "git", Args: []string{"status"}, Stdout: []byte("nothing\n")})

	top := disc.Top(10)
	if len(top) != 2 {
		t.Fatalf("Top len = %d (%v), want 2", len(top), top)
	}
	if top[0].Command != "frobnicate build" || top[0].Count != 2 {
		t.Fatalf("top[0] = %+v, want frobnicate build x2", top[0])
	}
	if top[1].Command != "wibble" {
		t.Fatalf("top[1] = %+v, want wibble", top[1])
	}
}

func TestFileSinkRecovery(t *testing.T) {
	dir := t.TempDir()
	reg := New(loudFilter{}).WithSink(FileSink{Dir: dir})

	res := reg.Compress(Command{
		Name:   "loud",
		Args:   []string{"--all"},
		Stdout: []byte("line1\nline2\nline3\nkept\n"),
		Stderr: []byte("a warning\n"),
	})

	if res.Truncation.FullRef == "" {
		t.Fatal("expected FullRef to be set on a lossy result")
	}
	data, err := os.ReadFile(res.Truncation.FullRef)
	if err != nil {
		t.Fatalf("read FullRef: %v", err)
	}
	full := string(data)
	for _, want := range []string{"$ loud --all", "a warning", "line1", "line2", "line3", "kept"} {
		if !strings.Contains(full, want) {
			t.Fatalf("recovered file missing %q:\n%s", want, full)
		}
	}
}

func TestFileSinkSkippedWhenLossless(t *testing.T) {
	dir := t.TempDir()
	// passthrough of small output is lossless -> no sink call, no file written.
	reg := New().WithSink(FileSink{Dir: dir})
	res := reg.Compress(Command{Name: "echo", Stdout: []byte("hi\n")})
	if res.Truncation.FullRef != "" {
		t.Fatalf("FullRef set on lossless result: %q", res.Truncation.FullRef)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("sink wrote %d files for a lossless result", len(entries))
	}
}

func TestCompactCollapsesBlankRuns(t *testing.T) {
	reg := New().WithCompact()
	in := "\n\nalpha\n\n\nbeta\n\n\n"
	res := reg.Compress(Command{Name: "x", Stdout: []byte(in)})
	if res.Text != "alpha\n\nbeta\n" {
		t.Fatalf("compact = %q, want %q", res.Text, "alpha\n\nbeta\n")
	}
	if !res.Truncation.Happened {
		t.Fatal("expected compact to record dropped blank lines")
	}
}

func TestCompactKeepsSignalOnly(t *testing.T) {
	reg := New().WithCompact()
	// No blank lines -> nothing to compact, nothing dropped.
	res := reg.Compress(Command{Name: "x", Stdout: []byte("a\nb\nc\n")})
	if res.Text != "a\nb\nc\n" {
		t.Fatalf("compact altered non-blank text: %q", res.Text)
	}
}

func TestGroupByDirectory(t *testing.T) {
	spec := Spec{
		Name:  "find-group",
		Match: MatchSpec{Command: "find"},
		Group: &GroupSpec{
			KeyRegex: `^(.*)/[^/]+$`,
			Line:     "{key}/ ({count} files)",
		},
	}
	f, err := spec.Compile()
	if err != nil {
		t.Fatal(err)
	}
	out := f.Apply(Command{
		Name:   "find",
		Stdout: []byte("src/a.go\nsrc/b.go\nsrc/c.go\ndocs/x.md\ndocs/y.md\n"),
	})
	got := out.Text
	if !strings.Contains(got, "src/ (3 files)") {
		t.Fatalf("missing src group:\n%s", got)
	}
	if !strings.Contains(got, "docs/ (2 files)") {
		t.Fatalf("missing docs group:\n%s", got)
	}
	if out.Truncation.DroppedLines != 5 {
		t.Fatalf("DroppedLines = %d, want 5", out.Truncation.DroppedLines)
	}
}

func TestGroupExamplesAndPassthrough(t *testing.T) {
	spec := Spec{
		Name:  "warn-group",
		Match: MatchSpec{Command: "lint"},
		Group: &GroupSpec{
			KeyRegex: `\[(\w+)\]`,
			Examples: 1,
		},
	}
	f, err := spec.Compile()
	if err != nil {
		t.Fatal(err)
	}
	out := f.Apply(Command{
		Name:   "lint",
		Stdout: []byte("Summary header\nfile1 [unused]\nfile2 [unused]\nfile3 [shadow]\n"),
	})
	got := out.Text
	// The non-matching header line passes through.
	if !strings.HasPrefix(got, "Summary header\n") {
		t.Fatalf("expected header passthrough first:\n%s", got)
	}
	if !strings.Contains(got, "unused (2)") {
		t.Fatalf("missing unused group:\n%s", got)
	}
	// One example kept, indented.
	if !strings.Contains(got, "  file1 [unused]") {
		t.Fatalf("missing indented example:\n%s", got)
	}
}

func TestGroupRequiresCapture(t *testing.T) {
	spec := Spec{
		Name:  "bad-group",
		Match: MatchSpec{Command: "x"},
		Group: &GroupSpec{KeyRegex: `no-capture-here`},
	}
	if err := spec.Validate(); err == nil {
		t.Fatal("expected error for group key_regex without a capture group")
	}
}

func TestSinkAndCompactComposeWithDefault(t *testing.T) {
	dir := t.TempDir()
	reg := Default().WithCompact().WithSink(FileSink{Dir: dir})
	// go test -json with a failing case is lossy; just ensure the pipeline runs.
	res := reg.Compress(Command{
		Name:   "go",
		Args:   []string{"test", "./..."},
		Stdout: []byte(`{"Action":"output","Test":"TestX","Output":"boom\n"}` + "\n" + `{"Action":"fail","Test":"TestX"}` + "\n"),
	})
	if res.Filter != "go-test" {
		t.Fatalf("Filter = %q, want go-test", res.Filter)
	}
	if res.Truncation.FullRef != "" {
		if _, err := os.Stat(res.Truncation.FullRef); err != nil {
			t.Fatalf("FullRef points at missing file: %v", err)
		}
		if filepath.Dir(res.Truncation.FullRef) != dir {
			t.Fatalf("FullRef not under sink dir: %s", res.Truncation.FullRef)
		}
	}
}
