# Integrating gortk into a host runtime

This guide is for embedding gortk in an agent runtime that already runs commands
itself (a gateway, a tool-execution server, an in-process executor). If you just
want the CLI, see the README.

## TL;DR

- **Use it as a library, not a CLI.** A host that runs commands in-process
  already holds the raw `stdout`/`stderr`/`exitCode`. Compressing is one function
  call on that result. rtk ships a CLI only because it's an external proxy for
  third-party tools that shell out — an in-process host is not in that position.
- The CLI in `cmd/gortk` is an **optional dev/debug tool** (iterate on filters,
  let non-Go callers pipe through it). It is not on the runtime path.
- Drop gortk in wherever command output is about to enter the model's context.
- Turn on **redaction** there: `gortk.Default().WithRedaction()`.

## Do we need a CLI?

No — not for the runtime. The decision matrix:

| Need | Answer |
|---|---|
| Compress output inside an agent / gateway | **Library.** You already have the bytes; call `Compress`. |
| Iterate on a new filter spec from a terminal | CLI (`gortk filter …`) — convenient, optional. |
| A non-Go process wants compression | CLI (`gortk run -- …`) — optional. |
| Match rtk's transparent shell-hook UX | Only if you *are* a shell proxy; an in-process host isn't. |

So: build against the library; keep the CLI around as a dev tool.

## Where to call it

Call gortk at the seam where a tool result becomes model-facing text. Two common
shapes:

### Option A — compress in place (smallest change)

If you have a function that returns `{Stdout, Stderr, ExitCode}` to the model,
compress right before returning:

```go
res := gortk.Default().WithRedaction().Compress(gortk.CommandFromArgs(
    append([]string{cmdName}, args...),
    stdout.Bytes(), stderr.Bytes(), exitCode,
))
// res.Text -> the model-facing output; res.Truncation -> what changed
```

This is the one-liner win. It's safe because gortk is lossless for any command
without a dedicated filter, and `res.Truncation` records anything dropped or
masked.

### Option B — keep raw, add a compressed field (recommended)

If you have a sanctioned raw-exec boundary (security reviewed), don't mutate its
output there. Keep the raw bytes available and compress at the point where the
runtime turns a tool result into context:

```go
view := gortk.Default().WithRedaction().Compress(gortk.CommandFromArgs(
    argv, []byte(resp.Stdout), []byte(resp.Stderr), int(resp.ExitCode),
))
resp.LlmOutput            = view.Text                  // what the model sees
resp.OutputTruncated      = view.Truncation.Happened
resp.OutputTruncationNote = view.Truncation.Note
```

The runtime uses `LlmOutput` for context and can fall back to raw `Stdout` when
an agent needs the full thing (e.g. parsing a diff). This preserves gortk's
"lossless-by-default, honest-about-loss" contract end to end.

### Shell-line commands (`sh -c "…"`)

When the command was run as a shell line rather than argv, use the line adapter —
it tokenizes enough to route to a filter:

```go
cmd := gortk.CommandFromLine(cmdLine, []byte(resp.Stdout), []byte(resp.Stderr), int(resp.ExitCode))
view := gortk.Default().WithRedaction().Compress(cmd)
```

## The input/output split (and where your executor sits)

gortk separates **running** (`Runner` / `Invocation` → `Command`) from
**parsing** (`Registry.Compress(Command)` → `Result`). If you already own a
hardened executor (process groups, timeouts, sandbox, traversal checks), the
recommended posture is:

- **Keep your executor as the input half.** Don't adopt `ExecRunner` on the
  runtime path — it's the standalone/CLI runner and lacks your security
  hardening.
- **Use gortk's output half** (`Registry.Compress`) on the captured bytes.

If you want a uniform seam, wrap your executor as a `gortk.Runner` so callers can
use a `Session` while execution still goes through your sanctioned path:

```go
func hostRunner(exec MyExecutor) gortk.Runner {
    return gortk.RunnerFunc(func(ctx context.Context, inv gortk.Invocation) (gortk.Command, error) {
        out, errOut, code, err := exec.Run(ctx, inv.Name, inv.Args, inv.Dir, inv.Env, inv.Timeout)
        if err != nil {
            return gortk.Command{}, err
        }
        return gortk.CommandFromArgs(
            append([]string{inv.Name}, inv.Args...), out, errOut, code,
        ), nil
    })
}

// Compose with the output half:
session := gortk.NewSession(hostRunner(myExec), gortk.Default().WithRedaction())
cmd, view, err := session.Run(ctx, gortk.Invocation{Name: "go", Args: []string{"test", "./..."}})
```

This gives you one call site (`session.Run`) while keeping execution inside your
security-reviewed executor, and leaves the raw `cmd` available next to the
compressed `view`.

## Recovering full output (tee)

Lossy filters are only safe if a dropped line is recoverable. Attach a `Sink` and
every lossy result carries a handle to its full output:

```go
reg := gortk.Default().WithRedaction().WithSink(gortk.FileSink{Dir: teeDir})
res := reg.Compress(cmd)
if res.Truncation.FullRef != "" {
    // expose a "read full output" affordance at res.Truncation.FullRef
}
```

Note: the sink saves the **full, unredacted** output for local recovery —
redaction protects the model context, not your disk. Point `Dir` at a location
as trusted as the command's own environment.

## One registry, reused

`gortk.Default()` allocates and compiles filters; build it once and share it
(it's read-only after construction, safe for concurrent `Compress`).

```go
var compressor = gortk.Default().WithRedaction() // package-level, built once

func compress(argv []string, out, err []byte, code int) gortk.Result {
    return compressor.Compress(gortk.CommandFromArgs(argv, out, err, code))
}
```

To add your own rules without forking this repo, ship a JSON spec file and layer
it on at startup:

```go
data, _ := os.ReadFile("my-filters.json")
specs, _ := gortk.LoadSpecs(data)
for _, s := range specs { _ = compressor.RegisterSpec(s) }
```

## Relationship to existing structured runners

If your runtime already has structured runners (pytest JUnit, `go test -json`,
cargo, jest) that produce compact typed results — **keep using them** for those
commands. gortk is for the generic command tail that otherwise dumps raw bytes
into context: arbitrary `git`, `docker`, `make`, lint, and one-off shell
commands. That's the gap it fills.

This mirrors rtk's own architecture: hand-written parsers for rich formats, plus
a declarative engine for everything else.
