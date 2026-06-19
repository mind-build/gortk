package gortk

import (
	"strings"
	"testing"
)

// pgLogSpec mirrors how postgres' pgLogWriter would be expressed declaratively:
// parse the stock log_line_prefix, map postgres severities, demote routine
// checkpoint chatter, render "msg" (timestamp dropped, pid kept as a field).
var pgLogSpec = LogSpec{
	Source:    "both",
	LineRegex: `^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}(?:\.\d+)? \S+ \[(?P<pid>\d+)\] (?P<level>\w+):\s*(?P<msg>.*)$`,
	LevelMap: map[string]string{
		"LOG": "info", "STATEMENT": "debug", "DETAIL": "debug", "HINT": "debug",
		"WARNING": "warn", "ERROR": "error", "FATAL": "fatal", "PANIC": "fatal",
	},
	DefaultLevel:   "info",
	DemotePatterns: []string{`^checkpoint (starting|complete)`, `^restartpoint`},
}

func TestLogParserStreaming(t *testing.T) {
	// The streaming path postgres uses: compile once, Parse line-by-line.
	p, err := pgLogSpec.Compile()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		line      string
		wantLevel string
		wantMsg   string
		wantPID   string
	}{
		{`2026-06-16 14:56:37.312 UTC [35801] LOG:  database system is ready`, "info", "database system is ready", "35801"},
		{`2026-06-16 14:56:38.001 UTC [35801] FATAL:  the database system is starting up`, "fatal", "the database system is starting up", "35801"},
		{`2026-06-16 14:56:39.100 UTC [42] WARNING:  could not open file`, "warn", "could not open file", "42"},
		{`2026-06-16 14:56:40.000 UTC [42] LOG:  checkpoint starting: time`, "debug", "checkpoint starting: time", "42"}, // demoted
		{`initdb: some non-prefixed line`, "info", "initdb: some non-prefixed line", ""},                                 // unmatched -> default
	}
	for _, tc := range cases {
		rec := p.Parse(tc.line)
		if rec.Level != tc.wantLevel {
			t.Errorf("%q: level = %q, want %q", tc.line, rec.Level, tc.wantLevel)
		}
		if rec.Fields["msg"] != tc.wantMsg {
			t.Errorf("%q: msg = %q, want %q", tc.line, rec.Fields["msg"], tc.wantMsg)
		}
		if tc.wantPID != "" && rec.Fields["pid"] != tc.wantPID {
			t.Errorf("%q: pid = %v, want %q", tc.line, rec.Fields["pid"], tc.wantPID)
		}
	}
}

func TestLogBatchProducesRecordsAndText(t *testing.T) {
	spec := Spec{Name: "pg", Match: MatchSpec{Command: "postgres"}, Log: &pgLogSpec}
	f := compile(t, spec)
	in := strings.Join([]string{
		`2026-06-16 14:56:37.312 UTC [1] LOG:  ready`,
		`2026-06-16 14:56:38.001 UTC [1] ERROR:  boom`,
	}, "\n")
	res := f.Apply(Command{Stderr: []byte(in)})

	if len(res.Records) != 2 {
		t.Fatalf("got %d records, want 2", len(res.Records))
	}
	if res.Records[0].Level != "info" || res.Records[1].Level != "error" {
		t.Errorf("levels wrong: %q %q", res.Records[0].Level, res.Records[1].Level)
	}
	// Default template renders just the message (timestamp/pid dropped from text).
	if res.Text != "ready\nboom\n" {
		t.Errorf("text = %q, want %q", res.Text, "ready\nboom\n")
	}
}

func TestLogMinLevelFilters(t *testing.T) {
	spec := pgLogSpec
	spec.MinLevel = "warn"
	spec.Template = "[{level}] {msg}"
	f := compile(t, Spec{Name: "pg", Match: MatchSpec{Command: "postgres"}, Log: &spec})
	in := strings.Join([]string{
		`2026-06-16 14:56:37.312 UTC [1] LOG:  routine info`,
		`2026-06-16 14:56:38.001 UTC [1] WARNING:  heads up`,
		`2026-06-16 14:56:39.001 UTC [1] ERROR:  broke`,
	}, "\n")
	res := f.Apply(Command{Stderr: []byte(in)})
	if strings.Contains(res.Text, "routine info") {
		t.Errorf("info should be filtered below min_level warn:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "[warn] heads up") || !strings.Contains(res.Text, "[error] broke") {
		t.Errorf("warn/error should survive:\n%s", res.Text)
	}
	if res.Lossless() {
		t.Errorf("dropping the info line should record truncation")
	}
}

func TestLogGoTemplate(t *testing.T) {
	spec := pgLogSpec
	spec.Template = `{{.level}} pid={{.pid}} {{.msg}}`
	f := compile(t, Spec{Name: "pg", Match: MatchSpec{Command: "postgres"}, Log: &spec})
	res := f.Apply(Command{Stderr: []byte(`2026-06-16 14:56:37.312 UTC [99] ERROR:  kaboom`)})
	if !strings.Contains(res.Text, "error pid=99 kaboom") {
		t.Errorf("go-template render wrong: %q", res.Text)
	}
}

func TestLogSpecValidation(t *testing.T) {
	if _, err := (LogSpec{}).Compile(); err == nil {
		t.Error("expected error for missing line_regex")
	}
	if _, err := (LogSpec{LineRegex: "("}).Compile(); err == nil {
		t.Error("expected error for bad line_regex")
	}
	if _, err := (LogSpec{LineRegex: ".", MinLevel: "loud"}).Compile(); err == nil {
		t.Error("expected error for non-canonical min_level")
	}
	if _, err := (LogSpec{LineRegex: ".", DemotePatterns: []string{"["}}).Compile(); err == nil {
		t.Error("expected error for bad demote_pattern")
	}
}
