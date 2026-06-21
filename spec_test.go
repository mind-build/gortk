package gortk

import (
	"strings"
	"testing"
)

// compile is a test helper that compiles a spec or fails.
func compile(t *testing.T, s Spec) Filter {
	t.Helper()
	f, err := s.Compile()
	if err != nil {
		t.Fatalf("compile %q: %v", s.Name, err)
	}
	return f
}

// --- match ------------------------------------------------------------------

func TestMatch(t *testing.T) {
	cases := []struct {
		name  string
		spec  Spec
		cmd   string
		args  []string
		match bool
	}{
		{"bare command", Spec{Match: MatchSpec{Command: "git"}}, "git", []string{"log"}, true},
		{"wrong command", Spec{Match: MatchSpec{Command: "git"}}, "go", []string{"log"}, false},
		{"abs path base", Spec{Match: MatchSpec{Command: "git"}}, "/usr/bin/git", []string{"log"}, true},
		{"subcommand hit", Spec{Match: MatchSpec{Command: "git", Subcommands: []string{"status"}}}, "git", []string{"status"}, true},
		{"subcommand then flags", Spec{Match: MatchSpec{Command: "git", Subcommands: []string{"status"}}}, "git", []string{"status", "--short"}, true},
		{"leading flags then subcommand", Spec{Match: MatchSpec{Command: "git", Subcommands: []string{"status"}}}, "git", []string{"--no-pager", "status"}, true},
		{"subcommand miss", Spec{Match: MatchSpec{Command: "git", Subcommands: []string{"status"}}}, "git", []string{"log"}, false},
		{"args contain hit", Spec{Match: MatchSpec{Command: "docker", ArgsContain: []string{"build"}}}, "docker", []string{"buildx", "build", "."}, true},
		{"args contain miss", Spec{Match: MatchSpec{Command: "docker", ArgsContain: []string{"build"}}}, "docker", []string{"ps"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.spec.Name = "t"
			tc.spec.Lines = &LineSpec{} // make it compilable
			f := compile(t, tc.spec)
			if got := f.Match(tc.cmd, tc.args); got != tc.match {
				t.Errorf("Match(%q, %v) = %v, want %v", tc.cmd, tc.args, got, tc.match)
			}
		})
	}
}

// --- line transforms --------------------------------------------------------

func TestLineDropPrefix(t *testing.T) {
	f := compile(t, Spec{
		Name: "t", Match: MatchSpec{Command: "x"},
		Lines: &LineSpec{DropPrefixes: []string{"NOISE"}},
	})
	res := f.Apply(Command{Stdout: []byte("NOISE a\nkeep b\nNOISE c\n")})
	if strings.Contains(res.Text, "NOISE") {
		t.Errorf("prefix not dropped:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "keep b") {
		t.Errorf("kept line missing:\n%s", res.Text)
	}
	if res.Truncation.DroppedLines != 2 {
		t.Errorf("DroppedLines = %d, want 2", res.Truncation.DroppedLines)
	}
}

func TestLineDropRegexp(t *testing.T) {
	f := compile(t, Spec{
		Name: "t", Match: MatchSpec{Command: "x"},
		Lines: &LineSpec{DropRegexps: []string{`^\s*\d+%\s*$`}},
	})
	res := f.Apply(Command{Stdout: []byte("  42%  \nresult\n100%\n")})
	if strings.Contains(res.Text, "%") {
		t.Errorf("percent lines not dropped:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "result") {
		t.Errorf("result dropped:\n%s", res.Text)
	}
}

func TestLineKeepWhitelist(t *testing.T) {
	// keep_regexps is a whitelist: only matching lines survive.
	f := compile(t, Spec{
		Name: "t", Match: MatchSpec{Command: "x"},
		Lines: &LineSpec{KeepRegexps: []string{`ERROR|FAIL`}},
	})
	res := f.Apply(Command{Stdout: []byte("INFO starting\nERROR boom\nDEBUG noise\nFAIL bad\n")})
	if !strings.Contains(res.Text, "ERROR boom") || !strings.Contains(res.Text, "FAIL bad") {
		t.Errorf("whitelisted lines missing:\n%s", res.Text)
	}
	if strings.Contains(res.Text, "INFO") || strings.Contains(res.Text, "DEBUG") {
		t.Errorf("non-whitelisted lines should be dropped:\n%s", res.Text)
	}
}

func TestLineKeepThenDrop(t *testing.T) {
	// Drop rules apply on top of the whitelist.
	f := compile(t, Spec{
		Name: "t", Match: MatchSpec{Command: "x"},
		Lines: &LineSpec{
			KeepRegexps: []string{`^\s+at `},
			DropRegexps: []string{`^\s+at java\.`},
		},
	})
	res := f.Apply(Command{Stdout: []byte("  at com.app.Foo\n  at java.base.Bar\nrandom line\n")})
	if !strings.Contains(res.Text, "at com.app.Foo") {
		t.Errorf("kept frame missing:\n%s", res.Text)
	}
	if strings.Contains(res.Text, "java.base") || strings.Contains(res.Text, "random line") {
		t.Errorf("dropped/non-whitelisted lines present:\n%s", res.Text)
	}
}

func TestLineDropBlankAndTrim(t *testing.T) {
	f := compile(t, Spec{
		Name: "t", Match: MatchSpec{Command: "x"},
		Lines: &LineSpec{TrimSpace: true, DropBlank: true},
	})
	res := f.Apply(Command{Stdout: []byte("  a  \n\n   \n  b\n")})
	if res.Text != "a\nb\n" {
		t.Errorf("trim+dropBlank = %q, want %q", res.Text, "a\nb\n")
	}
	if res.Truncation.DroppedLines != 2 {
		t.Errorf("DroppedLines = %d, want 2 (two blank lines)", res.Truncation.DroppedLines)
	}
}

func TestLineDedupAdjacent(t *testing.T) {
	f := compile(t, Spec{
		Name: "t", Match: MatchSpec{Command: "x"},
		Lines: &LineSpec{DedupAdjacent: true},
	})
	res := f.Apply(Command{Stdout: []byte("a\na\na\nb\nb\na\n")})
	if res.Text != "a\nb\na\n" {
		t.Errorf("dedup = %q, want %q", res.Text, "a\nb\na\n")
	}
	if res.Truncation.DroppedLines != 3 {
		t.Errorf("DroppedLines = %d, want 3", res.Truncation.DroppedLines)
	}
}

func TestLineEmptyText(t *testing.T) {
	f := compile(t, Spec{
		Name: "t", Match: MatchSpec{Command: "x"}, EmptyText: "clean",
		Lines: &LineSpec{DropPrefixes: []string{"x"}},
	})
	res := f.Apply(Command{Stdout: []byte("x1\nx2\n")})
	if res.Text != "clean\n" {
		t.Errorf("empty_text = %q, want %q", res.Text, "clean\n")
	}
}

func TestLineSourceStderrAndBoth(t *testing.T) {
	cmd := Command{Stdout: []byte("out\n"), Stderr: []byte("err\n")}

	fErr := compile(t, Spec{Name: "t", Match: MatchSpec{Command: "x"}, Lines: &LineSpec{Source: "stderr"}})
	if got := fErr.Apply(cmd).Text; got != "err\n" {
		t.Errorf("stderr source = %q, want %q", got, "err\n")
	}

	fBoth := compile(t, Spec{Name: "t", Match: MatchSpec{Command: "x"}, Lines: &LineSpec{Source: "both"}})
	if got := fBoth.Apply(cmd).Text; got != "err\nout\n" {
		t.Errorf("both source = %q, want %q", got, "err\nout\n")
	}
}

// --- limits -----------------------------------------------------------------

func TestLimitTailAndHead(t *testing.T) {
	in := Command{Stdout: []byte("1\n2\n3\n4\n5\n")}

	tail := compile(t, Spec{Name: "t", Match: MatchSpec{Command: "x"},
		Lines: &LineSpec{}, Limit: &LimitSpec{MaxLines: 2}})
	if got := tail.Apply(in).Text; got != "4\n5\n" {
		t.Errorf("tail limit = %q, want %q", got, "4\n5\n")
	}

	head := compile(t, Spec{Name: "t", Match: MatchSpec{Command: "x"},
		Lines: &LineSpec{}, Limit: &LimitSpec{MaxLines: 2, Keep: "head"}})
	res := head.Apply(in)
	if res.Text != "1\n2\n" {
		t.Errorf("head limit = %q, want %q", res.Text, "1\n2\n")
	}
	if res.Truncation.DroppedLines != 3 {
		t.Errorf("DroppedLines = %d, want 3", res.Truncation.DroppedLines)
	}
}

// --- JSON transforms --------------------------------------------------------

func TestJSONNestedPathsAndCount(t *testing.T) {
	f := compile(t, Spec{
		Name: "t", Match: MatchSpec{Command: "x"},
		JSON: &JSONSpec{
			ArrayField:      "Issues",
			ItemTemplate:    "{Pos.Filename}:{Pos.Line} [{Linter}] {Text}",
			SummaryTemplate: "total {count}",
		},
	})
	in := `{"Issues":[
		{"Linter":"a","Text":"first","Pos":{"Filename":"f.go","Line":3}},
		{"Linter":"b","Text":"second","Pos":{"Filename":"g.go","Line":9}}
	]}`
	res := f.Apply(Command{Stdout: []byte(in)})
	if !strings.Contains(res.Text, "f.go:3 [a] first") {
		t.Errorf("item 1 wrong:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "g.go:9 [b] second") {
		t.Errorf("item 2 wrong:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "total 2") {
		t.Errorf("summary count wrong:\n%s", res.Text)
	}
}

func TestJSONIntegralNumbersHaveNoDecimal(t *testing.T) {
	f := compile(t, Spec{
		Name: "t", Match: MatchSpec{Command: "x"},
		JSON: &JSONSpec{ArrayField: "xs", ItemTemplate: "{n}"},
	})
	res := f.Apply(Command{Stdout: []byte(`{"xs":[{"n":42}]}`)})
	if !strings.Contains(res.Text, "42\n") || strings.Contains(res.Text, "42.0") {
		t.Errorf("integral number formatting wrong: %q", res.Text)
	}
}

func TestJSONMissingPathRendersEmpty(t *testing.T) {
	f := compile(t, Spec{
		Name: "t", Match: MatchSpec{Command: "x"},
		JSON: &JSONSpec{ArrayField: "xs", ItemTemplate: "[{missing}]"},
	})
	res := f.Apply(Command{Stdout: []byte(`{"xs":[{"n":1}]}`)})
	if !strings.Contains(res.Text, "[]") {
		t.Errorf("missing path should render empty: %q", res.Text)
	}
}

func TestJSONFallsBackToLines(t *testing.T) {
	// JSON present but stdout isn't JSON → Lines transform runs.
	f := compile(t, Spec{
		Name: "t", Match: MatchSpec{Command: "x"},
		JSON:  &JSONSpec{ArrayField: "xs", ItemTemplate: "{n}"},
		Lines: &LineSpec{DropPrefixes: []string{"noise"}},
	})
	res := f.Apply(Command{Stdout: []byte("noise line\nreal line\n")})
	if strings.Contains(res.Text, "noise") || !strings.Contains(res.Text, "real line") {
		t.Errorf("expected line fallback:\n%s", res.Text)
	}
}

func TestJSONFallsBackToPassthroughWhenNoLines(t *testing.T) {
	f := compile(t, Spec{
		Name: "t", Match: MatchSpec{Command: "x"},
		JSON: &JSONSpec{ArrayField: "xs", ItemTemplate: "{n}"},
	})
	res := f.Apply(Command{Stdout: []byte("not json at all\n")})
	if res.Filter != "passthrough" || !strings.Contains(res.Text, "not json") {
		t.Errorf("expected passthrough fallback, got filter=%q text=%q", res.Filter, res.Text)
	}
}

// --- strip ansi -------------------------------------------------------------

func TestStripANSI(t *testing.T) {
	f := compile(t, Spec{
		Name: "t", Match: MatchSpec{Command: "x"},
		Lines: &LineSpec{StripANSI: true},
	})
	in := "\x1b[31mFAIL\x1b[0m: TestX\n\x1b[1;32mok\x1b[0m\n"
	res := f.Apply(Command{Stdout: []byte(in)})
	if strings.Contains(res.Text, "\x1b") {
		t.Errorf("ANSI not stripped: %q", res.Text)
	}
	if !strings.Contains(res.Text, "FAIL: TestX") || !strings.Contains(res.Text, "ok") {
		t.Errorf("content lost stripping ANSI: %q", res.Text)
	}
}

func TestStripANSIUnitHelper(t *testing.T) {
	if got := StripANSI("plain"); got != "plain" {
		t.Errorf("plain text changed: %q", got)
	}
	if got := StripANSI("\x1b[33mwarn\x1b[0m"); got != "warn" {
		t.Errorf("color codes not stripped: %q", got)
	}
}

func TestStripANSIFullControlSet(t *testing.T) {
	esc := "\x1b"
	cases := map[string]string{
		"a" + esc + "[2Jb":                 "ab",  // CSI cursor/erase
		esc + "[?1049h" + "x":              "x",   // alt-screen (CSI ?)
		esc + "[1;40r" + "x":               "x",   // scroll region
		esc + "[6n":                        "",    // DSR query
		esc + "]0;title" + "\x07" + "x":    "x",   // OSC BEL-terminated
		esc + "]11;?" + esc + "\\" + "x":   "x",   // OSC ST-terminated
		esc + "(B" + "x":                   "x",   // charset designation
		esc + "=" + "y":                    "y",   // keypad mode
		esc + "[32mok" + esc + "[6n" + "!": "ok!", // color kept-stripped + DSR removed
	}
	for in, want := range cases {
		if got := StripANSI(in); got != want {
			t.Errorf("StripANSI(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- match_output (whole-blob collapse) -------------------------------------

func TestMatchOutputCollapses(t *testing.T) {
	f := compile(t, Spec{
		Name: "t", Match: MatchSpec{Command: "git"},
		MatchOutput: []OutputRule{
			{Pattern: `(?m)^To .+\n.+->.+`, Unless: `(?i)error|rejected`, Message: "ok (pushed)"},
		},
		Lines: &LineSpec{}, // fallback if no collapse
	})
	pushOut := "Enumerating objects: 5, done.\nTo github.com:me/repo.git\n   abc123..def456  main -> main\n"
	res := f.Apply(Command{Stderr: []byte(pushOut)})
	if res.Text != "ok (pushed)\n" {
		t.Errorf("collapse failed: %q", res.Text)
	}
	if res.Lossless() {
		t.Errorf("collapse should record truncation")
	}
}

func TestMatchOutputUnlessGuard(t *testing.T) {
	f := compile(t, Spec{
		Name: "t", Match: MatchSpec{Command: "git"},
		MatchOutput: []OutputRule{
			{Pattern: `To `, Unless: `(?i)rejected`, Message: "ok (pushed)"},
		},
		Lines: &LineSpec{Source: "both"}, // fall through to raw-ish output on guard
	})
	rejected := "To github.com:me/repo.git\n ! [rejected] main -> main (fetch first)\n"
	res := f.Apply(Command{Stderr: []byte(rejected)})
	if res.Text == "ok (pushed)\n" {
		t.Fatalf("unless guard failed: error output was hidden")
	}
	if !strings.Contains(res.Text, "rejected") {
		t.Errorf("rejection detail should survive: %q", res.Text)
	}
}

func TestMatchOutputFirstRuleWins(t *testing.T) {
	f := compile(t, Spec{
		Name: "t", Match: MatchSpec{Command: "x"},
		MatchOutput: []OutputRule{
			{Pattern: `done`, Message: "first"},
			{Pattern: `done`, Message: "second"},
		},
		Lines: &LineSpec{},
	})
	res := f.Apply(Command{Stdout: []byte("all done\n")})
	if res.Text != "first\n" {
		t.Errorf("first rule should win: %q", res.Text)
	}
}

func TestMatchOutputValidation(t *testing.T) {
	bad := []Spec{
		{Name: "t", Match: MatchSpec{Command: "x"}, MatchOutput: []OutputRule{{Pattern: "p"}}},                            // no message
		{Name: "t", Match: MatchSpec{Command: "x"}, MatchOutput: []OutputRule{{Message: "m"}}},                            // no pattern
		{Name: "t", Match: MatchSpec{Command: "x"}, MatchOutput: []OutputRule{{Pattern: "(", Message: "m"}}},              // bad regex
		{Name: "t", Match: MatchSpec{Command: "x"}, MatchOutput: []OutputRule{{Pattern: "p", Unless: "[", Message: "m"}}}, // bad unless
	}
	for i, s := range bad {
		if err := s.Validate(); err == nil {
			t.Errorf("case %d: expected validation error", i)
		}
	}
}

// --- validation -------------------------------------------------------------

func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name string
		spec Spec
	}{
		{"empty name", Spec{Match: MatchSpec{Command: "x"}, Lines: &LineSpec{}}},
		{"empty command", Spec{Name: "t", Lines: &LineSpec{}}},
		{"no transform", Spec{Name: "t", Match: MatchSpec{Command: "x"}}},
		{"json missing fields", Spec{Name: "t", Match: MatchSpec{Command: "x"}, JSON: &JSONSpec{}}},
		{"bad drop regexp", Spec{Name: "t", Match: MatchSpec{Command: "x"}, Lines: &LineSpec{DropRegexps: []string{"("}}}},
		{"bad keep regexp", Spec{Name: "t", Match: MatchSpec{Command: "x"}, Lines: &LineSpec{KeepRegexps: []string{"["}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.spec.Validate(); err == nil {
				t.Errorf("expected validation error for %s", tc.name)
			}
		})
	}
}

func TestValidateAccepts(t *testing.T) {
	s := Spec{Name: "t", Match: MatchSpec{Command: "x"}, Lines: &LineSpec{DropRegexps: []string{`^ok`}}}
	if err := s.Validate(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- loading / round-trip ---------------------------------------------------

func TestLoadSpecsRoundTrip(t *testing.T) {
	data := []byte(`[{"name":"a","match":{"command":"x"},"lines":{"drop_prefixes":["z"]}}]`)
	specs, err := LoadSpecs(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 || specs[0].Name != "a" || specs[0].Lines == nil {
		t.Fatalf("round trip wrong: %+v", specs)
	}
	if _, err := specs[0].Compile(); err != nil {
		t.Errorf("compile loaded spec: %v", err)
	}
}

func TestLoadSpecsBadJSON(t *testing.T) {
	if _, err := LoadSpecs([]byte("{not an array")); err == nil {
		t.Error("expected decode error")
	}
}

// --- embedded defaults -------------------------------------------------------

func TestEmbeddedDefaultsAllCompile(t *testing.T) {
	specs := DefaultSpecs()
	if len(specs) == 0 {
		t.Fatal("no embedded specs")
	}
	for _, s := range specs {
		if err := s.Validate(); err != nil {
			t.Errorf("embedded spec %q invalid: %v", s.Name, err)
		}
	}
}

func TestRegisterSpecOnRegistry(t *testing.T) {
	r := New()
	if err := r.RegisterSpec(Spec{
		Name: "echo-noise", Match: MatchSpec{Command: "noisy"},
		Lines: &LineSpec{DropPrefixes: []string{"DEBUG"}},
	}); err != nil {
		t.Fatal(err)
	}
	res := r.Compress(Command{Name: "noisy", Stdout: []byte("DEBUG x\nreal\n")})
	if res.Filter != "echo-noise" || strings.Contains(res.Text, "DEBUG") {
		t.Errorf("custom spec not applied: filter=%q text=%q", res.Filter, res.Text)
	}
}

func TestRegisterSpecRejectsInvalid(t *testing.T) {
	if err := New().RegisterSpec(Spec{Name: "bad"}); err == nil {
		t.Error("expected error registering invalid spec")
	}
}

// --- default registry integration -------------------------------------------

func TestDefaultGitPushCollapses(t *testing.T) {
	out := "Enumerating objects: 12, done.\nCounting objects: 100% (12/12), done.\nTo github.com:me/repo.git\n   abc..def  main -> main\n"
	res := Default().Compress(Command{Name: "git", Args: []string{"push"}, Stderr: []byte(out)})
	if res.Filter != "git-push" || res.Text != "git push: ok\n" {
		t.Errorf("git push not collapsed: filter=%q text=%q", res.Filter, res.Text)
	}
}

func TestDefaultGitPushRejectionSurvives(t *testing.T) {
	out := "To github.com:me/repo.git\n ! [rejected] main -> main (non-fast-forward)\nerror: failed to push some refs\n"
	res := Default().Compress(Command{Name: "git", Args: []string{"push"}, Stderr: []byte(out)})
	if !strings.Contains(res.Text, "rejected") || !strings.Contains(res.Text, "failed to push") {
		t.Errorf("push failure must not be hidden: %q", res.Text)
	}
}
