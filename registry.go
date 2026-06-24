package gortk

import "strings"

// DefaultMaxBytes bounds the passthrough (and post-filter) output size. It is
// the last line of defence against a runaway command flooding the context. The
// value (32 KiB) is a deliberately small per-command cap — gortk output is meant
// to already be compact, unlike a raw shell capture.
const DefaultMaxBytes = 32 * 1024

// Registry holds an ordered set of filters and applies the first one that
// matches. The zero value is not usable; build one with New.
type Registry struct {
	filters   []Filter
	maxBytes  int
	compact   bool
	normalize bool
	redactor  *Redactor
	sink      Sink
	observers []func(Command, Result)
}

// New builds a Registry with the given filters (first match wins) and the
// default size bound.
func New(filters ...Filter) *Registry {
	return &Registry{filters: filters, maxBytes: DefaultMaxBytes}
}

// Sink persists the full, uncompressed output of a command so a lossy Result can
// carry a handle (Truncation.FullRef) back to it. Configure one with
// Registry.WithSink; FileSink is the built-in disk implementation (rtk's "tee").
// A Sink is only consulted when a Result actually dropped something.
type Sink interface {
	// Save persists cmd's full output and returns an opaque handle (e.g. a file
	// path) the caller can later use to retrieve it. An error means "could not
	// save" — Compress then leaves FullRef empty and proceeds; recovery is a
	// best-effort convenience, never a hard dependency.
	Save(cmd Command, res Result) (ref string, err error)
}

// Default returns a Registry wired with the built-in filters: the hand-written
// structured parsers (go test) plus the embedded declarative Specs
// (golangci-lint, git status). This is the intended entry point for embedding
// gortk in an agent: keep one Default registry and call Compress on every
// command's output.
//
// It panics only if the embedded defaults are malformed, which is a build-time
// guarantee covered by tests.
func Default() *Registry {
	r := New(GoTest{})
	for _, s := range DefaultSpecs() {
		f, err := s.Compile()
		if err != nil {
			// Be resilient: one malformed embedded/community spec must not break
			// the whole registry. TestEmbeddedDefaultsAllCompile fails loudly if
			// any built-in spec is invalid, so this only ever skips at runtime
			// for third-party specs added later.
			continue
		}
		r.Register(f)
	}
	return r
}

// RegisterSpec compiles a Spec and appends it as a lowest-priority filter.
func (r *Registry) RegisterSpec(s Spec) error {
	f, err := s.Compile()
	if err != nil {
		return err
	}
	r.Register(f)
	return nil
}

// WithMaxBytes returns a copy of the registry with a different size bound.
func (r *Registry) WithMaxBytes(n int) *Registry {
	cp := *r
	cp.maxBytes = n
	return &cp
}

// WithSink returns a copy of the registry that persists the full output of every
// lossy result through s, attaching a recovery handle to Truncation.FullRef. Pass
// nil to disable. Off by default (gortk holds nothing it wasn't asked to).
func (r *Registry) WithSink(s Sink) *Registry {
	cp := *r
	cp.sink = s
	return &cp
}

// WithCompact returns a copy of the registry that applies a final
// whitespace-collapsing pass to every result (rtk's "-u" ultra-compact spirit):
// runs of blank lines collapse to one and leading/trailing blanks are trimmed.
// It only ever removes whitespace, so it is safe to leave on; dropped blank
// lines are still recorded in Truncation.
func (r *Registry) WithCompact() *Registry {
	cp := *r
	cp.compact = true
	return &cp
}

// WithRedaction returns a copy of the registry that masks credentials in every
// result — including lossless passthrough, since unfiltered commands (env,
// printenv, cat .env) are the likeliest to leak. Uses the high-precision default
// pattern set (cloud keys, tokens, JWTs, PEM private keys, key=value secrets,
// URL credentials). Strongly recommended for any output bound for a model.
// Masked spans are counted in Truncation.Masked.
func (r *Registry) WithRedaction() *Registry {
	red, _ := newRedactor(RedactOptions{}) // default rules never fail to compile
	cp := *r
	cp.redactor = red
	return &cp
}

// WithRedactionOptions is WithRedaction with control over entropy scanning and
// extra patterns. It returns an error only if an Extra regex fails to compile.
func (r *Registry) WithRedactionOptions(opts RedactOptions) (*Registry, error) {
	red, err := newRedactor(opts)
	if err != nil {
		return nil, err
	}
	cp := *r
	cp.redactor = red
	return &cp, nil
}

// WithNormalize returns a copy of the registry that collapses volatile
// high-cardinality tokens (UUIDs, ISO timestamps, IPs, hex digests) to stable
// markers (<uuid>, <ts>, <ip>, <hash>). This cuts tokens and lets dedup collapse
// otherwise-unique lines. It changes content (loses the specific identifier), so
// it is opt-in; substitutions are counted in Truncation.Masked.
func (r *Registry) WithNormalize() *Registry {
	cp := *r
	cp.normalize = true
	return &cp
}

// Observe registers a callback invoked with (cmd, result) after every Compress,
// for metrics and discovery. Multiple observers run in registration order.
// Observers must not mutate their arguments. Returns the registry for chaining.
// See Stats (savings) and Discovery (unmatched commands) for ready-made ones.
func (r *Registry) Observe(fn func(Command, Result)) *Registry {
	if fn != nil {
		r.observers = append(r.observers, fn)
	}
	return r
}

// Register appends a filter, giving it lowest priority. Use this to add
// project-specific filters on top of Default().
func (r *Registry) Register(f Filter) *Registry {
	r.filters = append(r.filters, f)
	return r
}

// Compress runs the first matching filter, then bounds the result size. If no
// filter matches, it returns a lossless passthrough of stdout+stderr (still
// size-bounded). Compress never errors and never panics on a well-formed
// Command — a command with no special handling is simply passed through.
//
// After producing the view it records the savings (InputBytes/OutputBytes),
// persists the full output through any configured Sink when the result is lossy
// (Truncation.FullRef), and fans the (cmd, result) pair out to observers.
func (r *Registry) Compress(cmd Command) Result {
	res := r.bound(r.apply(cmd))
	if r.compact {
		res = compact(res)
	}
	// Redact secrets before normalization so a credential is masked as a secret,
	// not silently rewritten to <hash>. Both run on passthrough too.
	if r.redactor != nil {
		var n int
		res.Text, n = r.redactor.redact(res.Text)
		res.Truncation.addMasked(n)
	}
	if r.normalize {
		var n int
		res.Text, n = normalizeText(res.Text)
		res.Truncation.addMasked(n)
	}

	res.InputBytes = len(cmd.Stdout) + len(cmd.Stderr)
	res.OutputBytes = len(res.Text)

	if r.sink != nil && res.Truncation.Happened {
		if ref, err := r.sink.Save(cmd, res); err == nil {
			res.Truncation.FullRef = ref
		}
	}

	for _, ob := range r.observers {
		ob(cmd, res)
	}
	return res
}

// apply runs the first matching filter, or passthrough when none match. It does
// no bounding/accounting — Compress wraps it.
func (r *Registry) apply(cmd Command) Result {
	for _, f := range r.filters {
		if f.Match(cmd.Name, cmd.Args) {
			return f.Apply(cmd)
		}
	}
	return passthrough(cmd)
}

// passthrough joins stderr and stdout verbatim. It is lossless by construction;
// only r.bound may later trim it for size.
func passthrough(cmd Command) Result {
	var b strings.Builder
	if len(cmd.Stderr) > 0 {
		b.Write(cmd.Stderr)
		if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
			b.WriteByte('\n')
		}
	}
	b.Write(cmd.Stdout)
	return Result{Text: b.String(), Filter: "passthrough"}
}

// bound trims a Result's Text to maxBytes, keeping the tail (where failures and
// summaries usually live) and recording the loss.
func (r *Registry) bound(res Result) Result {
	if r.maxBytes <= 0 || len(res.Text) <= r.maxBytes {
		return res
	}
	dropped := len(res.Text) - r.maxBytes
	res.Text = "… (" + itoa(dropped) + " bytes trimmed) …\n" + res.Text[dropped:]
	res.Truncation.dropBytes(dropped, "output exceeded size bound; kept tail")
	return res
}
