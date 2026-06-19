package gortk

import (
	"fmt"
	"regexp"
	"strings"
	"text/template"
)

// Log parsing: parse → route → render, for log streams. A named-capture regex
// turns each line into fields, a level map normalizes severities, noise
// patterns demote to debug, an optional minimum level filters, and a template
// renders each kept line. This is the declarative form of hand-written log
// writers (e.g. postgres' pgLogWriter).
//
// The compiled form is LogParser, which parses ONE line at a time. That keeps
// it usable two ways from the same definition:
//
//   - batch: a Spec with a `log` block runs every line through it inside
//     Registry.Compress, returning compressed Text plus structured Records.
//   - streaming: a consumer compiles a LogSpec directly and calls Parse(line)
//     per line as a stream arrives, routing each Record to a logger at its
//     level — no buffering of the whole stream.

// LogSpec is the declarative log transform.
type LogSpec struct {
	Source         string            `json:"source,omitempty"`          // stdout|stderr|both (default both — logs usually go to stderr)
	LineRegex      string            `json:"line_regex"`                // named groups become fields; group "msg" is the message
	LevelField     string            `json:"level_field,omitempty"`     // capture holding the raw severity (default "level")
	LevelMap       map[string]string `json:"level_map,omitempty"`       // raw token -> canonical level (debug|info|warn|error|fatal)
	DefaultLevel   string            `json:"default_level,omitempty"`   // unmatched lines / unknown severities (default "info")
	DemotePatterns []string          `json:"demote_patterns,omitempty"` // message matches one -> level becomes "debug"
	MinLevel       string            `json:"min_level,omitempty"`       // drop records below this (default: keep all)
	Template       string            `json:"template,omitempty"`        // per-record render; {field} or Go template; default "{msg}"
}

// levelOrder ranks canonical levels for MinLevel comparison. Synonyms collapse
// to the same rank. "" (no level) sorts as info so unlevelled lines aren't
// dropped by a MinLevel filter.
var levelOrder = map[string]int{
	"": 1, "debug": 0, "trace": 0,
	"info": 1, "notice": 1,
	"warn": 2, "warning": 2,
	"error": 3, "err": 3,
	"fatal": 4, "panic": 4, "critical": 4,
}

// LogParser is a compiled LogSpec. It is read-only after construction (safe for
// concurrent use) and parses one line at a time, so it serves both batch
// compression and streaming consumers.
type LogParser struct {
	spec     LogSpec
	lineRE   *regexp.Regexp
	demoteRE []*regexp.Regexp
	tmpl     *template.Template // non-nil when Template is a Go template
	tmplStr  string
	lvlField string
	defLevel string
}

const logMsgField = "msg"

// Compile builds a LogParser, precompiling the regex, demote patterns, and
// template.
func (s LogSpec) Compile() (*LogParser, error) {
	if s.LineRegex == "" {
		return nil, fmt.Errorf("gortk: log spec needs line_regex")
	}
	re, err := regexp.Compile(s.LineRegex)
	if err != nil {
		return nil, fmt.Errorf("gortk: log line_regex %q: %w", s.LineRegex, err)
	}
	if s.MinLevel != "" {
		if _, ok := levelOrder[s.MinLevel]; !ok {
			return nil, fmt.Errorf("gortk: log min_level %q is not a canonical level", s.MinLevel)
		}
	}
	p := &LogParser{
		spec:     s,
		lineRE:   re,
		tmplStr:  orDefault(s.Template, "{"+logMsgField+"}"),
		lvlField: orDefault(s.LevelField, "level"),
		defLevel: orDefault(s.DefaultLevel, "info"),
	}
	for _, d := range s.DemotePatterns {
		dre, err := regexp.Compile(d)
		if err != nil {
			return nil, fmt.Errorf("gortk: log demote_pattern %q: %w", d, err)
		}
		p.demoteRE = append(p.demoteRE, dre)
	}
	if isGoTemplate(p.tmplStr) {
		t, err := newGoTemplate("log", p.tmplStr)
		if err != nil {
			return nil, fmt.Errorf("gortk: log template: %w", err)
		}
		p.tmpl = t
	}
	return p, nil
}

// Parse turns one raw line into a Record: named captures become fields, the
// level is mapped/demoted, and the template renders the text. A line that
// doesn't match line_regex becomes {level: default, fields:{msg: line}} — so
// nothing is silently dropped.
func (p *LogParser) Parse(line string) Record {
	fields := map[string]any{}
	level := p.defLevel
	msg := line

	if m := p.lineRE.FindStringSubmatch(line); m != nil {
		for i, name := range p.lineRE.SubexpNames() {
			if name != "" && i < len(m) {
				fields[name] = m[i]
			}
		}
		if raw, _ := fields[p.lvlField].(string); raw != "" {
			level = p.mapLevel(raw)
		}
		if mm, ok := fields[logMsgField].(string); ok && mm != "" {
			msg = mm
		}
	}
	fields[logMsgField] = msg

	for _, re := range p.demoteRE {
		if re.MatchString(msg) {
			level = "debug"
			break
		}
	}
	fields["level"] = level

	return Record{Level: level, Fields: fields, Text: render(p.tmplStr, p.tmpl, fields)}
}

// Below reports whether a record falls under the spec's MinLevel and so should
// be dropped in batch mode. Streaming consumers can ignore this and let their
// own logger threshold filter.
func (p *LogParser) Below(rec Record) bool {
	if p.spec.MinLevel == "" {
		return false
	}
	return levelOrder[rec.Level] < levelOrder[p.spec.MinLevel]
}

func (p *LogParser) mapLevel(raw string) string {
	if m, ok := p.spec.LevelMap[raw]; ok {
		return m
	}
	if _, ok := levelOrder[strings.ToLower(raw)]; ok {
		return strings.ToLower(raw)
	}
	return p.defLevel
}

// applyLog runs the batch log path: every line through the parser, dropping
// below-min records, returning compressed Text plus structured Records.
func (f *specFilter) applyLog(cmd Command) Result {
	raw := lineSource(cmd, orDefault(f.spec.Log.Source, "both"))
	lines := strings.Split(raw, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}

	var records []Record
	var b strings.Builder
	dropped := 0
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		rec := f.logParser.Parse(line)
		if f.logParser.Below(rec) {
			dropped++
			continue
		}
		records = append(records, rec)
		b.WriteString(rec.Text)
		b.WriteByte('\n')
	}

	res := Result{Filter: f.spec.Name, Records: records, Text: b.String()}
	if dropped > 0 {
		res.Truncation.dropLines(dropped, "dropped "+itoa(dropped)+" line(s) below min level")
	}
	if res.Text == "" && f.spec.EmptyText != "" {
		res.Text = f.spec.EmptyText + "\n"
	}
	return f.limit(res)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
