package gortk

import (
	"bufio"
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

func itoa(n int) string { return strconv.Itoa(n) }

// ---------------------------------------------------------------------------
// go test
// ---------------------------------------------------------------------------

// GoTest compresses `go test` output. It prefers the structured `-json` stream
// (the same structured source a Go test runner consumes) and falls back to
// scanning plain text. It keeps failures, build errors, and the final summary; it drops
// per-package "ok" lines and "=== RUN"/"--- PASS" chatter, which dominate the
// byte count on a green run.
type GoTest struct{}

func (GoTest) Name() string { return "go-test" }

func (GoTest) Match(name string, args []string) bool {
	if base(name) != "go" {
		return false
	}
	for _, a := range args {
		if a == "test" {
			return true
		}
		if !strings.HasPrefix(a, "-") {
			return false // first positional wasn't "test"
		}
	}
	return false
}

// goTestEvent is a subset of the `go test -json` event schema.
type goTestEvent struct {
	Action  string `json:"Action"` // run|pass|fail|skip|output
	Package string `json:"Package"`
	Test    string `json:"Test"`
	Output  string `json:"Output"`
}

func (g GoTest) Apply(cmd Command) Result {
	if r, ok := g.applyJSON(cmd); ok {
		return r
	}
	return g.applyText(cmd)
}

func (GoTest) applyJSON(cmd Command) (Result, bool) {
	sc := bufio.NewScanner(bytes.NewReader(cmd.Stdout))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var saw bool
	var pass, fail, skip int
	// Collect output lines only for tests that ultimately fail.
	output := map[string][]string{} // pkg/test -> output lines
	var failedKeys []string

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] != '{' {
			return Result{}, false // not a -json stream
		}
		var ev goTestEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return Result{}, false
		}
		saw = true
		key := ev.Package + "." + ev.Test
		switch ev.Action {
		case "output":
			if ev.Test != "" {
				output[key] = append(output[key], strings.TrimRight(ev.Output, "\n"))
			}
		case "pass":
			if ev.Test != "" {
				pass++
				delete(output, key)
			}
		case "skip":
			if ev.Test != "" {
				skip++
				delete(output, key)
			}
		case "fail":
			if ev.Test != "" {
				fail++
				failedKeys = append(failedKeys, key)
			}
		}
	}
	if !saw {
		return Result{}, false
	}

	var res Result
	res.Filter = "go-test"
	var b strings.Builder
	sort.Strings(failedKeys)
	for _, k := range failedKeys {
		for _, l := range output[k] {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}
	b.WriteString("--- go test: ")
	b.WriteString(itoa(pass) + " passed, " + itoa(fail) + " failed, " + itoa(skip) + " skipped")
	if cmd.ExitCode != 0 && fail == 0 {
		b.WriteString(" (build/setup error — see stderr)")
		if s := strings.TrimSpace(string(cmd.Stderr)); s != "" {
			b.WriteString("\n")
			b.WriteString(s)
		}
	}
	b.WriteByte('\n')
	res.Text = b.String()
	if pass+skip > 0 {
		res.Truncation.dropLines(pass+skip, "kept "+itoa(fail)+" failing tests, dropped "+itoa(pass+skip)+" passing/skipped")
	}
	return res, true
}

func (GoTest) applyText(cmd Command) Result {
	var res Result
	res.Filter = "go-test"
	var kept []string
	var dropped int
	sc := bufio.NewScanner(bytes.NewReader(cmd.Stdout))
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "ok "),
			strings.HasPrefix(trimmed, "=== RUN"),
			strings.HasPrefix(trimmed, "=== PAUSE"),
			strings.HasPrefix(trimmed, "=== CONT"),
			strings.HasPrefix(trimmed, "--- PASS"),
			strings.HasPrefix(trimmed, "PASS"),
			trimmed == "":
			dropped++
		default:
			kept = append(kept, line)
		}
	}
	res.Text = strings.Join(kept, "\n")
	if s := strings.TrimSpace(string(cmd.Stderr)); s != "" {
		res.Text += "\n" + s
	}
	res.Text = strings.TrimSpace(res.Text) + "\n"
	res.Truncation.dropLines(dropped, "dropped "+itoa(dropped)+" pass/progress lines")
	return res
}

// ---------------------------------------------------------------------------

// base returns the last path element of a program name, so "/usr/bin/go" and
// "go" both match.
func base(name string) string {
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		return name[i+1:]
	}
	return name
}
