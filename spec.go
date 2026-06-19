package gortk

import (
	"embed"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"text/template"
)

// This file is the schema-driven half of gortk. A Spec describes a filter as
// data — match rules + line/JSON transforms — so compression can be tweaked,
// loaded from disk, or shipped as config without recompiling. Rich structured
// parsers (e.g. `go test -json`) stay as hand-written Filters in filters.go;
// the long tail of line- and JSON-shaped commands lives here as Specs.

// Built-in specs live in specs/<ecosystem>.json, one JSON array per file, so the
// catalog scales by ecosystem (go, python, js, git, …). All are embedded.
//
//go:embed specs/*.json
var specsFS embed.FS

// Spec is a declarative filter definition. Exactly one of JSON or Lines should
// drive the transform; if both are set, JSON is tried first and Lines is the
// fallback when stdout isn't valid JSON.
type Spec struct {
	Name  string    `json:"name"`
	Match MatchSpec `json:"match"`

	// MatchOutput collapses the whole output to a one-line message when a
	// pattern matches the full blob — e.g. a successful `git push` becomes
	// "ok (pushed)". Borrowed from rtk's most effective compression stage. The
	// first matching rule wins; a rule is skipped if its Unless pattern also
	// matches (so error/warning output is never hidden behind an "ok"). Checked
	// before JSON/Lines.
	MatchOutput []OutputRule `json:"match_output,omitempty"`

	JSON      *JSONSpec  `json:"json,omitempty"`
	Lines     *LineSpec  `json:"lines,omitempty"`
	Log       *LogSpec   `json:"log,omitempty"` // parse a log stream into levelled Records
	Limit     *LimitSpec `json:"limit,omitempty"`
	EmptyText string     `json:"empty_text,omitempty"` // text to emit when nothing survives
}

// OutputRule is one whole-blob collapse rule for Spec.MatchOutput.
type OutputRule struct {
	Pattern string `json:"pattern"`          // regex against the full output blob
	Unless  string `json:"unless,omitempty"` // if this also matches, skip the rule
	Message string `json:"message"`          // emitted when Pattern matches and Unless doesn't
}

// MatchSpec decides which commands a Spec applies to. Use either the structured
// fields (Command + Subcommands/ArgsContain) or CommandRegex — the regex form
// mirrors rtk's match_command and is the simplest way to port its filters.
type MatchSpec struct {
	Command     string   `json:"command,omitempty"`      // base program name, e.g. "git"
	Subcommands []string `json:"subcommands,omitempty"`  // first positional must be one of these
	ArgsContain []string `json:"args_contain,omitempty"` // every string must appear in args

	// CommandRegex matches against the reconstructed command line
	// "<base-name> <arg> <arg> …" (e.g. "uv sync --frozen"). When set, it is the
	// sole match condition. Equivalent to rtk's match_command.
	CommandRegex string `json:"command_regex,omitempty"`
}

// LineSpec is a line-oriented transform: drop known-noise lines, keep the rest.
// Keep rules win over drop rules, so you can drop broadly and rescue specifics.
type LineSpec struct {
	Source        string   `json:"source,omitempty"`     // "stdout"(default)|"stderr"|"both"
	StripANSI     bool     `json:"strip_ansi,omitempty"` // remove ANSI color/escape codes first
	TrimSpace     bool     `json:"trim_space,omitempty"`
	DropBlank     bool     `json:"drop_blank,omitempty"`
	DedupAdjacent bool     `json:"dedup_adjacent,omitempty"`
	DropPrefixes  []string `json:"drop_prefixes,omitempty"`
	DropRegexps   []string `json:"drop_regexps,omitempty"`
	KeepRegexps   []string `json:"keep_regexps,omitempty"` // whitelist: keep ONLY matching lines (drop rules still apply on top)

	// TruncateLinesAt caps each surviving line to N runes (adds "…"). 0 = off.
	// Equivalent to rtk's truncate_lines_at; good for tools that emit very long
	// single lines (minified diffs, embedded data).
	TruncateLinesAt int `json:"truncate_lines_at,omitempty"`
}

// JSONSpec flattens a JSON array into one compact line per element.
//
// Templates support two syntaxes, auto-detected: if a template contains "{{" it
// is a full Go text/template (powerful — conditionals, range, printf), executed
// against the element (a map[string]any); otherwise it uses the lightweight
// {dotted.path} placeholder form. SummaryTemplate additionally gets {count} /
// {{.count}}.
type JSONSpec struct {
	ArrayField      string `json:"array_field"`   // top-level field holding the array
	ItemTemplate    string `json:"item_template"` // e.g. "{Pos.Filename}:{Pos.Line} {Text}"
	SummaryTemplate string `json:"summary_template,omitempty"`
}

// LimitSpec caps the post-transform size and records the loss.
type LimitSpec struct {
	MaxLines int    `json:"max_lines,omitempty"`
	Keep     string `json:"keep,omitempty"` // "tail"(default)|"head"
}

// LoadSpecs decodes a JSON array of Specs.
func LoadSpecs(data []byte) ([]Spec, error) {
	var specs []Spec
	if err := json.Unmarshal(data, &specs); err != nil {
		return nil, fmt.Errorf("gortk: decode specs: %w", err)
	}
	return specs, nil
}

// DefaultSpecs returns the built-in specs across all embedded ecosystem files,
// in a deterministic order (filename, then declaration order within a file).
func DefaultSpecs() []Spec {
	entries, err := specsFS.ReadDir("specs")
	if err != nil {
		panic("gortk: cannot read embedded specs: " + err.Error())
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)

	var all []Spec
	for _, name := range names {
		data, err := specsFS.ReadFile("specs/" + name)
		if err != nil {
			panic("gortk: cannot read embedded specs/" + name + ": " + err.Error())
		}
		specs, err := LoadSpecs(data)
		if err != nil {
			panic("gortk: embedded specs/" + name + " is invalid: " + err.Error())
		}
		all = append(all, specs...)
	}
	return all
}

// Validate reports whether a Spec is well-formed (regexps compile, required
// fields present) without building a filter.
func (s Spec) Validate() error {
	_, err := s.Compile()
	return err
}

// Compile turns a Spec into a runnable Filter, precompiling its regexps.
func (s Spec) Compile() (Filter, error) {
	if s.Name == "" {
		return nil, fmt.Errorf("gortk: spec has empty name")
	}
	if s.Match.Command == "" && s.Match.CommandRegex == "" {
		return nil, fmt.Errorf("gortk: spec %q needs match.command or match.command_regex", s.Name)
	}
	if s.JSON == nil && s.Lines == nil && s.Log == nil && len(s.MatchOutput) == 0 {
		return nil, fmt.Errorf("gortk: spec %q has no transform (json, lines, log, or match_output)", s.Name)
	}
	f := &specFilter{spec: s}
	if s.Match.CommandRegex != "" {
		re, err := regexp.Compile(s.Match.CommandRegex)
		if err != nil {
			return nil, fmt.Errorf("gortk: spec %q command_regex %q: %w", s.Name, s.Match.CommandRegex, err)
		}
		f.cmdRE = re
	}
	for i, r := range s.MatchOutput {
		if r.Pattern == "" || r.Message == "" {
			return nil, fmt.Errorf("gortk: spec %q match_output[%d] needs pattern and message", s.Name, i)
		}
		pat, err := regexp.Compile(r.Pattern)
		if err != nil {
			return nil, fmt.Errorf("gortk: spec %q match_output[%d] pattern %q: %w", s.Name, i, r.Pattern, err)
		}
		cr := compiledOutRule{pattern: pat, message: r.Message}
		if r.Unless != "" {
			cr.unless, err = regexp.Compile(r.Unless)
			if err != nil {
				return nil, fmt.Errorf("gortk: spec %q match_output[%d] unless %q: %w", s.Name, i, r.Unless, err)
			}
		}
		f.matchOut = append(f.matchOut, cr)
	}
	if s.Log != nil {
		lp, err := s.Log.Compile()
		if err != nil {
			return nil, fmt.Errorf("gortk: spec %q: %w", s.Name, err)
		}
		f.logParser = lp
	}
	if s.JSON != nil {
		// array_field may be empty (whole document is the array); item_template
		// is always required.
		if s.JSON.ItemTemplate == "" {
			return nil, fmt.Errorf("gortk: spec %q json needs item_template", s.Name)
		}
		if isGoTemplate(s.JSON.ItemTemplate) {
			t, err := newGoTemplate(s.Name+"/item", s.JSON.ItemTemplate)
			if err != nil {
				return nil, fmt.Errorf("gortk: spec %q item_template: %w", s.Name, err)
			}
			f.itemTmpl = t
		}
		if isGoTemplate(s.JSON.SummaryTemplate) {
			t, err := newGoTemplate(s.Name+"/summary", s.JSON.SummaryTemplate)
			if err != nil {
				return nil, fmt.Errorf("gortk: spec %q summary_template: %w", s.Name, err)
			}
			f.summaryTmpl = t
		}
	}
	if s.Lines != nil {
		for _, p := range s.Lines.DropRegexps {
			re, err := regexp.Compile(p)
			if err != nil {
				return nil, fmt.Errorf("gortk: spec %q drop_regexp %q: %w", s.Name, p, err)
			}
			f.dropRE = append(f.dropRE, re)
		}
		for _, p := range s.Lines.KeepRegexps {
			re, err := regexp.Compile(p)
			if err != nil {
				return nil, fmt.Errorf("gortk: spec %q keep_regexp %q: %w", s.Name, p, err)
			}
			f.keepRE = append(f.keepRE, re)
		}
	}
	return f, nil
}

type specFilter struct {
	spec        Spec
	cmdRE       *regexp.Regexp
	dropRE      []*regexp.Regexp
	keepRE      []*regexp.Regexp
	matchOut    []compiledOutRule
	itemTmpl    *template.Template // non-nil when ItemTemplate is a Go template
	summaryTmpl *template.Template // non-nil when SummaryTemplate is a Go template
	logParser   *LogParser         // non-nil when the spec has a log block
}

type compiledOutRule struct {
	pattern *regexp.Regexp
	unless  *regexp.Regexp
	message string
}

func (f *specFilter) Name() string { return f.spec.Name }

func (f *specFilter) Match(name string, args []string) bool {
	if f.cmdRE != nil {
		line := base(name)
		if len(args) > 0 {
			line += " " + strings.Join(args, " ")
		}
		return f.cmdRE.MatchString(line)
	}
	if base(name) != f.spec.Match.Command {
		return false
	}
	if len(f.spec.Match.Subcommands) > 0 && !firstPositionalIn(args, f.spec.Match.Subcommands) {
		return false
	}
	for _, want := range f.spec.Match.ArgsContain {
		if !contains(args, want) {
			return false
		}
	}
	return true
}

func (f *specFilter) Apply(cmd Command) Result {
	if res, ok := f.applyMatchOutput(cmd); ok {
		return res
	}
	if f.logParser != nil {
		return f.applyLog(cmd)
	}
	if f.spec.JSON != nil {
		if res, ok := f.applyJSON(cmd); ok {
			return res
		}
		// stdout wasn't JSON; fall through to Lines if available.
	}
	if f.spec.Lines != nil {
		return f.applyLines(cmd)
	}
	return passthrough(cmd)
}

// applyMatchOutput collapses the whole output to a one-line message if a rule
// fires. Runs against stdout+stderr so an error on either stream can trip an
// Unless guard. Returns ok=false when no rule matches.
func (f *specFilter) applyMatchOutput(cmd Command) (Result, bool) {
	if len(f.matchOut) == 0 {
		return Result{}, false
	}
	blob := lineSource(cmd, "both")
	for _, r := range f.matchOut {
		if !r.pattern.MatchString(blob) {
			continue
		}
		if r.unless != nil && r.unless.MatchString(blob) {
			continue // errors/warnings present — don't hide them behind a summary
		}
		res := Result{Text: r.message + "\n", Filter: f.spec.Name}
		dropped := strings.Count(strings.TrimRight(blob, "\n"), "\n") + 1
		res.Truncation.dropLines(dropped, "collapsed "+itoa(dropped)+" line(s) to summary")
		return res, true
	}
	return Result{}, false
}

func (f *specFilter) applyJSON(cmd Command) (Result, bool) {
	arr, ok := f.jsonArray(cmd.Stdout)
	if !ok {
		return Result{}, false
	}
	var b strings.Builder
	for _, el := range arr {
		b.WriteString(render(f.spec.JSON.ItemTemplate, f.itemTmpl, el))
		b.WriteByte('\n')
	}
	res := Result{Filter: f.spec.Name}
	if f.spec.JSON.SummaryTemplate != "" {
		summary := map[string]any{"count": len(arr)}
		b.WriteString(render(f.spec.JSON.SummaryTemplate, f.summaryTmpl, summary))
		b.WriteByte('\n')
	}
	res.Text = b.String()
	if res.Text == "" && f.spec.EmptyText != "" {
		res.Text = f.spec.EmptyText + "\n"
	}
	res = f.limit(res)
	return res, true
}

// jsonArray extracts the array a JSON spec iterates over. With an empty (or "$"
// / ".") ArrayField the whole document must be a JSON array (ruff, eslint); a
// dotted ArrayField resolves into a top-level object (golangci-lint "Issues",
// pytest "tests"). Returns ok=false when stdout isn't the expected JSON shape,
// so the caller can fall back to Lines/passthrough.
func (f *specFilter) jsonArray(stdout []byte) ([]any, bool) {
	field := f.spec.JSON.ArrayField
	if field == "" || field == "$" || field == "." {
		var arr []any
		if err := json.Unmarshal(stdout, &arr); err != nil {
			return nil, false
		}
		return arr, true
	}
	var doc map[string]any
	if err := json.Unmarshal(stdout, &doc); err != nil {
		return nil, false
	}
	v, ok := resolvePath(doc, field)
	if !ok {
		return nil, false
	}
	arr, ok := v.([]any)
	return arr, ok
}

func (f *specFilter) applyLines(cmd Command) Result {
	ls := f.spec.Lines
	raw := lineSource(cmd, ls.Source)
	lines := strings.Split(raw, "\n")
	// strings.Split leaves a trailing "" for text ending in \n; drop it so we
	// don't count a phantom blank line.
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}

	var kept []string
	var dropped int
	var prev string
	havePrev := false
	for _, line := range lines {
		v := line
		if ls.StripANSI {
			v = stripANSI(v)
		}
		if ls.TrimSpace {
			v = strings.TrimSpace(v)
		}
		if f.dropLine(v, ls) {
			dropped++
			continue
		}
		if ls.DedupAdjacent && havePrev && v == prev {
			dropped++
			continue
		}
		if ls.TruncateLinesAt > 0 {
			v = truncateRunes(v, ls.TruncateLinesAt)
		}
		kept = append(kept, v)
		prev = v
		havePrev = true
	}

	text := strings.Join(kept, "\n")
	if text == "" && f.spec.EmptyText != "" {
		text = f.spec.EmptyText
	}
	res := Result{Filter: f.spec.Name}
	if text != "" {
		res.Text = text + "\n"
	}
	res.Truncation.dropLines(dropped, "dropped "+itoa(dropped)+" noise line(s)")
	return f.limit(res)
}

func (f *specFilter) dropLine(v string, ls *LineSpec) bool {
	// keep_regexps is a whitelist (rtk's keep_lines_matching): when present,
	// only lines matching at least one keep pattern survive. Drop rules then
	// apply on top of the whitelist, so you can keep a class of lines and still
	// drop a noisy subset.
	if len(f.keepRE) > 0 {
		whitelisted := false
		for _, re := range f.keepRE {
			if re.MatchString(v) {
				whitelisted = true
				break
			}
		}
		if !whitelisted {
			return true
		}
	}
	if ls.DropBlank && strings.TrimSpace(v) == "" {
		return true
	}
	for _, p := range ls.DropPrefixes {
		if strings.HasPrefix(v, p) {
			return true
		}
	}
	for _, re := range f.dropRE {
		if re.MatchString(v) {
			return true
		}
	}
	return false
}

// limit applies the Spec's MaxLines cap, keeping head or tail.
func (f *specFilter) limit(res Result) Result {
	if f.spec.Limit == nil || f.spec.Limit.MaxLines <= 0 {
		return res
	}
	lines := strings.Split(strings.TrimRight(res.Text, "\n"), "\n")
	if len(lines) <= f.spec.Limit.MaxLines {
		return res
	}
	drop := len(lines) - f.spec.Limit.MaxLines
	var kept []string
	if f.spec.Limit.Keep == "head" {
		kept = lines[:f.spec.Limit.MaxLines]
	} else {
		kept = lines[drop:]
	}
	res.Text = strings.Join(kept, "\n") + "\n"
	res.Truncation.dropLines(drop, "capped at "+itoa(f.spec.Limit.MaxLines)+" lines")
	return res
}

// --- helpers ---------------------------------------------------------------

func lineSource(cmd Command, src string) string {
	switch src {
	case "stderr":
		return string(cmd.Stderr)
	case "both":
		out := string(cmd.Stderr)
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		return out + string(cmd.Stdout)
	default:
		return string(cmd.Stdout)
	}
}

func firstPositionalIn(args, set []string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		return contains(set, a)
	}
	return false
}

func contains(haystack []string, needle string) bool {
	return slices.Contains(haystack, needle)
}

// isGoTemplate reports whether a template string should be treated as a full Go
// text/template (rather than the simple {dotted.path} form).
func isGoTemplate(tmpl string) bool {
	return strings.Contains(tmpl, "{{")
}

// newGoTemplate compiles a Go text/template with a couple of convenience funcs.
// missingkey=zero keeps a missing field rendering as empty rather than erroring.
func newGoTemplate(name, text string) (*template.Template, error) {
	return template.New(name).
		Option("missingkey=zero").
		Funcs(template.FuncMap{
			// "field" resolves a dotted path against the element, mirroring the
			// simple syntax: {{field . "Pos.Filename"}}.
			"field": func(v any, path string) string {
				if r, ok := resolvePath(v, path); ok {
					return formatValue(r)
				}
				return ""
			},
		}).
		Parse(text)
}

// render picks the Go-template path when one was precompiled, else the simple
// {dotted.path} substitution.
func render(tmpl string, goTmpl *template.Template, item any) string {
	if goTmpl != nil {
		var b strings.Builder
		if err := goTmpl.Execute(&b, item); err != nil {
			return ""
		}
		return strings.TrimRight(b.String(), "\n")
	}
	return renderTemplate(tmpl, item)
}

var tmplPlaceholder = regexp.MustCompile(`\{([^}]+)\}`)

// renderTemplate substitutes {dotted.path} placeholders against a decoded JSON
// value. Unknown paths render as empty. Integral numbers print without a
// trailing ".0".
func renderTemplate(tmpl string, item any) string {
	return tmplPlaceholder.ReplaceAllStringFunc(tmpl, func(m string) string {
		path := m[1 : len(m)-1]
		v, ok := resolvePath(item, path)
		if !ok {
			return ""
		}
		return formatValue(v)
	})
}

func resolvePath(v any, path string) (any, bool) {
	cur := v
	for key := range strings.SplitSeq(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[key]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// ansiPattern matches CSI/OSC ANSI escape sequences (colors, cursor moves).
var ansiPattern = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]|\x1b\\][^\x07]*\x07")

// truncateRunes caps s to n runes, appending "…" when it had to cut.
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func stripANSI(s string) string {
	if !strings.ContainsRune(s, '\x1b') {
		return s
	}
	return ansiPattern.ReplaceAllString(s, "")
}

func formatValue(v any) string {
	switch n := v.(type) {
	case float64:
		if n == float64(int64(n)) {
			return itoa(int(n))
		}
		return fmt.Sprintf("%g", n)
	case string:
		return n
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}
