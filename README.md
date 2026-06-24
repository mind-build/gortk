# gortk

[![CI](https://github.com/codefly-dev/gortk/actions/workflows/ci.yml/badge.svg)](https://github.com/codefly-dev/gortk/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/codefly-dev/gortk.svg)](https://pkg.go.dev/github.com/codefly-dev/gortk)
[![Go Report Card](https://goreportcard.com/badge/github.com/codefly-dev/gortk)](https://goreportcard.com/report/github.com/codefly-dev/gortk)
[![Go Version](https://img.shields.io/github/go-mod/go-version/codefly-dev/gortk)](https://github.com/codefly-dev/gortk/blob/main/go.mod)
[![Release](https://img.shields.io/github/v/release/codefly-dev/gortk?sort=semver)](https://github.com/codefly-dev/gortk/releases)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](./LICENSE)
[![Dependencies](https://img.shields.io/badge/dependencies-zero-brightgreen.svg)](./go.mod)
[![PRs Welcome](https://img.shields.io/badge/PRs-welcome-brightgreen.svg)](https://github.com/codefly-dev/gortk/pulls)

A Go-native take on [rtk](https://github.com/rtk-ai/rtk) (the Rust "token
killer"): it compresses shell-command output before it reaches an LLM context
window. Unlike rtk — a standalone Rust binary you shell out to — gortk is a
plain Go package meant to be **embedded inside an agent runtime**.

It keeps the signal an agent needs (failures, errors, `file:line`), drops the
noise (progress bars, "ok" lines), **redacts credentials**, and is **honest
about everything it changed**. It is lossless by default: a command with no
dedicated filter passes through untouched.

## Install

```
go get github.com/codefly-dev/gortk
```

Zero dependencies — the core is pure Go stdlib.

## Quickstart

```go
reg := gortk.Default().WithRedaction() // built-in filters + secret masking

res := reg.Compress(gortk.Command{
    Name: "go", Args: []string{"test", "./..."},
    Stdout: stdout, Stderr: stderr, ExitCode: code,
})

fmt.Print(res.Text)                 // compact, secret-free output for the model
if !res.Lossless() {
    log.Print(res.Truncation.Note)  // e.g. "kept 3 failing tests, dropped 412 passing"
}
```

## Why not just use rtk?

rtk has no Go binding; it's a Rust binary. Shelling out to it from an agent
means bundling and version-managing a foreign binary in every environment, and
accepting its **opaque, lossy** rewriting — fine for a human at a terminal,
risky for an agent that might need the one line rtk dropped.

gortk keeps the good idea (per-command output compression) and changes the
defaults that matter for an agent:

| | rtk | gortk |
|---|---|---|
| Form | external Rust binary | imported Go package |
| Default | lossy rewrite | **lossless passthrough**; loss is opt-in per filter |
| Loss visibility | none | every drop recorded in `Truncation` |
| Coverage | 100+ commands | the few your agent actually runs |

## Two halves: input (run) and output (parse)

gortk splits cleanly into two independent halves that meet only at the `Command`
type. You can use either alone.

```
INPUT  half:  Invocation ──Runner.Run──▶ Command        (runs; never parses)
                                            │
OUTPUT half:  Command ──Registry.Compress──▶ Result     (parses; never runs)

glue (optional):  Session = Runner + Registry
```

- **Input** — `Runner.Run(ctx, Invocation) (Command, error)`. `ExecRunner` is the
  built-in os/exec implementation; a host with its own executor (a hardened or
  sandboxed exec RPC) implements `Runner` over that. Runners never compress.
- **Output** — `Registry.Compress(Command) Result`. Pure: it executes nothing,
  so you can feed it recorded fixtures or output captured by any executor.
- **Glue** — `Session` composes the two, and is optional sugar:

```go
cmd, res, err := gortk.DefaultSession().Run(ctx, gortk.Invocation{
    Name: "go", Args: []string{"test", "./..."},
})
// cmd = raw capture, res.Text = compressed view, res.Truncation = what was dropped
```

For the common in-process case there are package-level shortcuts over a shared
default session (filters compiled once): `gortk.Run(ctx, inv)` and
`gortk.RunStream(ctx, inv, onLine)`.

The point of the split: a host can keep its own sanctioned execution and use
only the output half; standalone callers (and the CLI) use both. Neither half
depends on the other.

### Streaming (long-running commands)

For commands where you want output *live* (build/test progress, early signals),
the input half also has a streaming form. It's an optional capability —
`StreamRunner`, which `ExecRunner` implements — that emits each line as it
arrives and still returns the full `Command` at the end:

```go
cmd, res, err := gortk.DefaultSession().RunStream(ctx, inv, func(ev gortk.StreamEvent) {
    fmt.Printf("%s| %s\n", ev.Stream, ev.Line) // live, line by line
})
// res = compressed view, computed once at the end from the full cmd
```

Compression stays whole-output (match_output, json, head/tail all need the
complete result), so the model-facing compression still happens once when the
command finishes — streaming is purely about *observing* progress meanwhile.
Callbacks are serialized (stdout/stderr never overlap), so no locking is needed.
`Session.RunStream` degrades to a batch `Run` if the underlying runner can't
stream, so callers use one code path. CLI: `gortk run --stream -- <cmd>`.

## Grounded in rtk's actual implementation

I read rtk's source (not just its README) before building this. Two things
stood out and shaped the design:

1. **rtk is itself schema-driven.** Its compression rules live in TOML
   (`.rtk/filters.toml`, engine in `src/core/toml_filter.rs`) — `match_command`,
   `strip_lines_matching`/`keep_lines_matching`, `head_lines`/`tail_lines`,
   `max_lines`, `on_empty`, `match_output`, `replace`. gortk's `Spec` is the
   same idea in JSON, so rules are editable/loadable without a recompile.
2. **rtk splits structured parsers from the declarative engine.** It has ~60
   hand-written Rust parsers in `src/cmds/**` for rich formats (go test, pytest,
   golangci-lint, cargo…) and the TOML engine for the long tail. gortk uses the
   exact same split: code `Filter`s in `filters.go`, declarative `Spec`s for the
   rest.

Where gortk deliberately differs from rtk:

| | rtk | gortk |
|---|---|---|
| Loss reporting | inline `... (N omitted)` markers in text | structured `Truncation` on every `Result` (agent-readable) |
| JSON | only via hand-written Rust parsers | declarative `json` template stage in a Spec — no code needed |
| Form | external Rust binary | imported Go package |
| Default for unmatched cmd | n/a (rewrites only known cmds) | lossless passthrough |

### Parity with rtk's runtime features

The ideas rtk leans on at runtime have native, agent-shaped equivalents here:

| rtk feature | gortk equivalent |
|---|---|
| `tee` (save full output of failed commands) | `Registry.WithSink` + `FileSink`; the handle rides on `Truncation.FullRef` (see [Full-output recovery](#full-output-recovery-tee)) |
| `rtk gain` (token-savings report) | `Result.SavedBytes`/`SavedFraction` + the `Stats` observer |
| `rtk discover` (find missed optimizations) | the `Discovery` observer — records every command that fell through to passthrough |
| grouping (files by dir, errors by type) | the `group` spec stage |
| `-u` ultra-compact | `Registry.WithCompact` / CLI `-u` |

And one thing rtk doesn't do: **credential redaction** —
`Registry.WithRedaction()` masks secrets (incl. on passthrough) before output
reaches the model. See [Secret redaction](#secret-redaction-beyond-rtk).

## Catalog

**94 filters**, organized by ecosystem in `specs/<eco>.json` (plus the `go-test`
structured code filter). The bulk are imported from rtk's TOML catalog and kept
in sync from upstream (see [Keeping the catalog in sync](#keeping-the-catalog-in-sync-with-rtk));
the rest are hand-written extras.

| Ecosystem | Covered (selection) |
|---|---|
| Go | `go build`/`vet`/`run`/`mod`/`test`, `golangci-lint` |
| Python | `pytest`, `ruff`, `mypy`, `ty`, `basedpyright`, `pip`/`poetry`/`uv` |
| JS/TS | `tsc`, `eslint`, `prettier`, `npm`/`pnpm`, `vitest`, `biome`, `oxlint`, `turbo`, `nx`, `next` |
| Ruby | `rspec`, `rubocop`, `rake`, `bundle install` |
| JVM | `mvn`, `gradle`, `spring-boot` |
| Cloud/Infra | `aws`, `kubectl`, `psql`, `gcloud`, `helm`, `skopeo`, `terraform`/`tofu`, `ansible`, `docker build` |
| Build | `make`, `cargo`, `gcc`, `just`, `task`, `dotnet`, `swift-build`, `xcodebuild` |
| System | `grep`, `find`, `ls`, `tree`, `ps`, `df`, `du`, `ssh`, `rsync`, `ping`, `systemctl`, … |
| Git | `git status`/`push`/`pull`, `gh`, `jj` |
| Lint/misc | `shellcheck`, `hadolint`, `yamllint`, `markdownlint`, `curl`, and more |

Add a tool by appending a `Spec` to the relevant `specs/*.json` — no code.

### How the catalog was built

- **rtk's TOML filters** (`src/filters/*.toml`) are imported *deterministically*
  via the `rtk2json` converter (see below) — a faithful, mechanical translation.
- **rtk's structured Rust parsers** (`src/cmds/**`, e.g. `aws`, `rspec`, `psql`)
  have no TOML, so they were ported to line/JSON `Spec`s and each ships fixtures
  in `testdata/structured_fixtures.json`, run by `TestStructuredFixtures`.

### Zero-dependency core; TOML import is a separate module

gortk core (this module) is **pure stdlib — zero external dependencies**, so
embedding it pulls nothing extra. The rtk-TOML importer lives in a sibling
module, `rtkcompat/`, which is the only thing that depends on a TOML parser.

To import rtk (or community) `.toml` filters into JSON specs, run the converter
from the `rtkcompat` module:

```
cd rtkcompat
go run ./cmd/rtk2json [--tests] [--exclude a,b] path/to/rtk/src/filters/*.toml > ../specs/rtk.json
```

Field mapping (`match_command`→`command_regex`, `strip_lines_matching`→
`drop_regexps`, `keep_lines_matching`→whitelist `keep_regexps`,
`on_empty`→`empty_text`, …) is in `rtkcompat`. Re-run it to pull upstream
updates. The runtime never touches it — `specs/*.json` is JSON-only.

### Go templates for JSON output

`json.item_template` / `summary_template` auto-detect syntax: plain `{a.b}` for
the common case, or a full **Go `text/template`** when the string contains
`{{`. The template form handles nested arrays (e.g. rubocop's `files[].offenses[]`)
via `range`, conditionals, and `printf` — all stdlib, still zero-dep:

```json
"item_template": "{{$p := .path}}{{range .offenses}}{{$p}}:{{.location.start_line}} [{{.cop_name}}] {{.message}}\n{{end}}"
```

## Schema (the `Spec` type)

A `Spec` is JSON and runs as an ordered pipeline (mirrors rtk's stages):

1. `match_output` — whole-blob regex → one-line message, with an `unless` guard
   so errors are never hidden behind an "ok" (rtk's highest-leverage stage).
2. `json` — flatten a JSON array to one templated line per element.
3. `log` — parse a log stream into levelled `Records` (see below).
4. `lines` — `strip_ansi`, `trim_space`, `drop_blank`, `dedup_adjacent`,
   `drop_prefixes`, `drop_regexps`, `keep_regexps` (whitelist).
5. `group` — collapse surviving lines that share a `key_regex` capture into one
   `{key} ({count})` summary line (rtk's grouping; e.g. files by directory).
6. `limit` — `max_lines` head/tail cap.
7. `empty_text` — message when nothing survives.

Example (`git push` → one line unless it failed):

```json
{
  "name": "git-push",
  "match": { "command": "git", "subcommands": ["push"] },
  "match_output": [
    { "pattern": "(?m)^To .+", "unless": "(?i)error|rejected|fatal", "message": "git push: ok" }
  ]
}
```

Load and register at runtime:

```go
specs, _ := gortk.LoadSpecs(jsonBytes)
reg := gortk.Default()
for _, s := range specs { _ = reg.RegisterSpec(s) }
```

### Log parsing → structured `Records`

The `log` stage parses a log stream: a named-capture regex extracts fields per
line, `level_map` normalizes severities, `demote_patterns` drop routine noise to
`debug`, `min_level` filters, and `template` renders each kept line. It produces
**structured `Records`** (`{Level, Fields, Text}`) on the `Result`, not just
compressed text — so a caller can route by level or read fields directly.

It's deliberately *parse → route → render*, not a query language: no
expressions, no aggregation. That's where the "swiss-army" line is drawn.

The compiled form, `LogParser`, parses **one line at a time**, so the same spec
serves batch compression *and* streaming consumers (e.g. postgres' log writer
routing each line to a logger at its level — no buffering):

```go
p, _ := gortk.LogSpec{
    LineRegex: `^\S+ \S+ \S+ \[(?P<pid>\d+)\] (?P<level>\w+):\s*(?P<msg>.*)$`,
    LevelMap:  map[string]string{"LOG": "info", "FATAL": "fatal", "WARNING": "warn"},
    DemotePatterns: []string{`^checkpoint (starting|complete)`},
}.Compile()

for line := range stream {              // streaming, line by line
    rec := p.Parse(line)                // -> {Level, Fields{pid,msg,...}, Text}
    logger.At(rec.Level, rec.Fields["msg"])
}
```

## Design

Three rules, in priority order:

1. **Lossless by default.** A command with no dedicated filter is passed
   through untouched (only size-bounded). Adding gortk can never silently
   destroy signal.
2. **Failure-preserving.** Filters drop *known noise* (`ok` lines, `=== RUN`
   chatter, git's human hints) but never failures, errors, or `file:line`
   locations.
3. **Honest about loss.** Anything dropped or truncated is recorded in
   `Result.Truncation` — a compact "this view is partial" signal a host can
   surface to its agent or UI.

## Runtime features

These are the runtime conveniences rtk ships, re-cast for an embedded agent.
All are opt-in and the core stays zero-dependency.

### Full-output recovery (tee)

Recording loss is only half the promise — an agent also needs to *recover* the
line a filter dropped without re-running the command. Attach a `Sink` and every
lossy `Result` carries a handle to its full, uncompressed output:

```go
reg := gortk.Default().WithSink(gortk.FileSink{Dir: teeDir})

res := reg.Compress(cmd)
if res.Truncation.FullRef != "" {
    // hand the agent a "read full output" affordance pointing at this path
    full, _ := os.ReadFile(res.Truncation.FullRef)
}
```

`FileSink` writes to disk (rtk's tee dir); implement `Sink` yourself to stash
full output anywhere (blob store, in-memory LRU, RPC). The sink is consulted
*only* when something was actually dropped — a lossless result holds nothing.

### Savings (gain) and discovery

Every `Result` from `Compress` records `InputBytes`/`OutputBytes`, so savings is
computable per command (`res.SavedFraction()`). For cumulative reporting and to
learn which commands you should write a filter for next, register observers:

```go
stats := &gortk.Stats{}       // rtk's "gain"
disc  := &gortk.Discovery{}   // rtk's "discover" (records passthrough commands)
reg := gortk.Default().Observe(stats.Observe).Observe(disc.Observe)

// ... run a session ...
fmt.Println(stats.Report())          // "gortk: 142 cmds, 4.1 MiB -> 210 KiB (95% saved)"
for _, e := range disc.Top(10) {     // commands with no dedicated filter, by frequency
    fmt.Printf("%4d  %s\n", e.Count, e.Command)
}
```

`Discovery` is the data-driven answer to "is the catalog the *right* 94?" — it
tells you exactly what your agents run that still falls through to passthrough.

### Grouping

The `group` stage collapses many similar lines into one summary line per key —
rtk's grouping strategy. The `key_regex`'s first capture is the group key;
non-matching lines pass through untouched, so a header above a list is kept:

```json
{
  "name": "find",
  "match": { "command": "find" },
  "group": { "key_regex": "^(.*)/[^/]+$", "line": "{key}/ ({count} files)", "examples": 0 }
}
```

`examples: N` keeps up to N indented sample lines under each header.

### Ultra-compact

`Registry.WithCompact()` (CLI `-u`) adds a final whitespace-collapsing pass:
runs of blank lines collapse to one, leading/trailing blanks are trimmed. It
only ever removes whitespace, so it's safe to leave on; removed blanks are still
counted in `Truncation`.

### Secret redaction (beyond rtk)

rtk *compresses* output; it doesn't *sanitize* it. gortk adds a registry-wide
redaction pass that masks credentials before they enter the model's context —
and it runs on **every** result, including lossless passthrough, because the
commands most likely to leak (`env`, `printenv`, `cat .env`, `aws configure
list`) are exactly the ones with no dedicated filter:

```go
reg := gortk.Default().WithRedaction()

res := reg.Compress(gortk.Command{Name: "env", Stdout: envDump})
// AWS_SECRET_ACCESS_KEY=wJalr… -> AWS_SECRET_ACCESS_KEY=[REDACTED]
// res.Truncation.Masked counts how many spans were masked
```

The default set is **high-precision patterns** — cloud keys (AWS/GCP), GitHub
and Slack tokens, JWTs, PEM private keys, `Bearer` tokens, `key=value` /
`"key": "value"` secrets, and URL-embedded credentials — chosen to almost never
fire on ordinary build/test output. Masking replaces a span with a marker rather
than dropping a line, so it does **not** flip `Lossless()`; it's reported via
`Truncation.Masked`.

For a catch-all, opt into entropy scanning. It's more aggressive (it can mis-fire
on opaque-but-harmless tokens), so it's off by default and allowlists common
identifiers (UUIDs, git SHAs, hex digests):

```go
reg, _ := gortk.Default().WithRedactionOptions(gortk.RedactOptions{
    Entropy: true,             // also mask high-entropy blobs
    Extra:   []string{`xoxb-\w+`}, // your own patterns
})
```

> Note: a configured `Sink` (`--tee`) saves the **full, unredacted** output
> locally for recovery — redaction protects the model context, not your disk.

### Normalization

`Registry.WithNormalize()` (CLI `--normalize`) collapses volatile,
high-cardinality tokens — UUIDs, ISO timestamps, IPs, hex digests — to stable
markers (`<uuid>`, `<ts>`, `<ip>`, `<hash>`). This cuts tokens and lets
`dedup_adjacent` collapse log lines that differ only by a request id. It changes
content (you lose the specific id), so it's opt-in and counted in
`Truncation.Masked`.

## Usage

```go
reg := gortk.Default() // go test, golangci-lint, git status + passthrough

res := reg.Compress(gortk.Command{
    Name:     "go",
    Args:     []string{"test", "-json", "./..."},
    Stdout:   stdout,
    Stderr:   stderr,
    ExitCode: code,
})

fmt.Print(res.Text)        // compact output for the model
if !res.Lossless() {
    log.Print(res.Truncation.Note) // e.g. "kept 3 failing tests, dropped 412 passing"
}
```

Add a project-specific filter:

```go
reg := gortk.Default().Register(MyPytestFilter{})
```

A filter is anything implementing:

```go
type Filter interface {
    Name() string
    Match(name string, args []string) bool
    Apply(cmd Command) Result
}
```

## Where this fits

If your runtime already has a *structured* runner for a tool — one that parses
`go test -json`, pytest JUnit XML, cargo, jest, etc. into a typed result and
keeps only failed cases — keep using it. gortk is **not** a replacement for
that.

gortk is for the **generic command surface** an agent hits that has no dedicated
runner: arbitrary `git`, `docker`, `make`, lint, and one-off shell commands that
otherwise dump raw bytes straight into the context window. gortk slots in right
there — between your executor returning raw bytes and those bytes entering the
model's context — giving that long tail the same "keep signal, drop noise,
redact secrets, record loss" treatment your first-class runners already get,
without each agent reinventing it.

## Integrating into a host runtime

Short version: **use the library, not a CLI.** A host that runs commands
in-process already holds the raw output, so compressing is one call:

```go
var compressor = gortk.Default().WithRedaction() // build once, reuse (concurrency-safe)

view := compressor.Compress(gortk.CommandFromArgs(
    append([]string{req.Command}, req.Args...),
    stdout, stderr, exitCode,
))
// view.Text -> model context; view.Truncation -> what was dropped / masked
```

The natural seam is wherever command output is about to enter the model's
context. See **[INTEGRATION.md](./INTEGRATION.md)** for copy-pasteable adapters
(argv and shell-line forms), the "do we need a CLI?" decision matrix, and the
recommended keep-raw-add-a-field approach.

## CLI (optional dev tool)

`cmd/gortk` is for iterating on filters from a terminal — **not** the runtime
path:

```
go build -o gortk ./cmd/gortk

go test ./... 2>&1 | gortk filter go test   # compress piped output
gortk run -- go test ./...                  # run a command and compress it
gortk run --tee /tmp/gortk -- go test ./... # keep full output of lossy results
gortk -u run -- go test ./...               # ultra-compact (collapse blanks)
gortk --redact filter env                   # mask credentials in piped output
gortk --normalize filter docker ps          # collapse UUIDs/timestamps/hashes
gortk specs                                 # list active filters
gortk --specs my.json filter docker build   # try extra specs without a rebuild
```

`run`/`filter` print a one-line gain report to stderr (`N -> M bytes (P% saved)`),
the recovery path when `--tee` is set, masked-span counts when redacting, and a
hint when no filter matched.

## Keeping the catalog in sync with rtk

rtk releases roughly weekly, so `specs/rtk.json` is generated from upstream —
never hand-edited. Regenerate it with the Go importer, `rtksync`, which clones
rtk and converts its whole TOML catalog deterministically (`-check` mode just
detects drift, which CI runs on every PR):

```
go generate ./...                              # re-import via rtksync
cd rtkcompat && go run ./cmd/rtksync           # equivalent, run directly
cd rtkcompat && go run ./cmd/rtksync -check    # fail if specs/rtk.json is stale
```

Pin a release with `-ref v0.42.4`, use a local checkout with `-dir path/to/rtk`,
or drop filters with `-exclude name1,name2`. The runtime never touches rtk or
TOML — `specs/*.json` is plain JSON, loaded by the zero-dependency core.

## Status

Reference filters: `go test` (code), `golangci-lint`, `git push`, `git status`
(specs), plus lossless passthrough, and 94 imported/extra filters. Adapters for
argv and shell-line commands. Runtime features: secret redaction (`Redactor`),
normalization, full-output recovery (`Sink`), savings (`Stats`), discovery
(`Discovery`), grouping, ultra-compact. Re-sync rtk's catalog with `go generate
./...`; run the suite with `go test ./...`.

## License

[Apache 2.0](./LICENSE).
