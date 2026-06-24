package gortk

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
	"time"
)

// sharedDefaultSession is the process-wide ExecRunner + Default() session used
// by the package-level Run/RunStream shortcuts. Built once (Default compiles
// specs); a Registry is read-only after construction, so sharing is safe for
// concurrent use.
var sharedDefaultSession = sync.OnceValue(DefaultSession)

// Run is a convenience shortcut: run inv with the default in-process session
// (ExecRunner + the built-in filters) and return the raw capture plus the
// compressed view. Equivalent to gortk.DefaultSession().Run, but reuses one
// shared session instead of recompiling filters on each call.
func Run(ctx context.Context, inv Invocation) (Command, Result, error) {
	return sharedDefaultSession().Run(ctx, inv)
}

// This file is the INPUT half of gortk: running a command and capturing its
// raw output. It is deliberately decoupled from the OUTPUT half (Registry /
// Compress, which never executes anything). The seam between them is the
// Command type: a Runner produces one, a Registry consumes one. Neither half
// imports the other's behavior, so you can:
//
//   - run without parsing (use a Runner alone),
//   - parse without running (feed a Command from recorded fixtures or from a
//     host's own executor), or
//   - compose both via an optional Session.

// DefaultMaxCaptureBytes bounds each captured stream in ExecRunner. It matches a
// common 2 MiB host-executor cap so behavior is consistent if you swap runners.
const DefaultMaxCaptureBytes = 2 << 20 // 2 MiB per stream

// Invocation describes a command to run — the pure input description, with no
// notion of compression. A Runner turns it into a Command (raw output).
type Invocation struct {
	Name    string
	Args    []string
	Dir     string        // working directory; "" = current
	Env     []string      // extra KEY=VALUE entries, appended to the process env
	Stdin   []byte        // optional stdin
	Timeout time.Duration // 0 = no timeout
}

// Runner executes an Invocation and returns the captured output as a Command.
//
// A Runner MUST NOT compress or interpret output — that is the Registry's job.
// This is the extension point a host implements over its own executor (a
// hardened or sandboxed exec RPC, say); ExecRunner is the standalone os/exec
// implementation used by the CLI.
type Runner interface {
	Run(ctx context.Context, inv Invocation) (Command, error)
}

// RunnerFunc adapts an ordinary function to the Runner interface — handy for
// tests and for wrapping an existing executor without a new type.
type RunnerFunc func(context.Context, Invocation) (Command, error)

// Run implements Runner.
func (f RunnerFunc) Run(ctx context.Context, inv Invocation) (Command, error) {
	return f(ctx, inv)
}

// ExecRunner is the default Runner: it runs commands with os/exec and captures
// stdout/stderr into bounded buffers. Hosts with a hardened executor (process
// groups, sandboxing, RPC) should implement Runner over that instead.
type ExecRunner struct {
	// MaxCaptureBytes bounds each of stdout/stderr. 0 uses DefaultMaxCaptureBytes.
	MaxCaptureBytes int
}

// Run executes inv and returns the captured Command. A non-zero exit status is
// reported in Command.ExitCode and is NOT a Go error. A Go error is returned
// only when the process could not run to completion (failed to start, context
// cancelled, timed out) — the partial Command is still returned alongside it.
func (r ExecRunner) Run(ctx context.Context, inv Invocation) (Command, error) {
	if inv.Name == "" {
		return Command{}, errors.New("gortk: invocation has empty Name")
	}
	if inv.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, inv.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, inv.Name, inv.Args...)
	cmd.Dir = inv.Dir
	if len(inv.Env) > 0 {
		cmd.Env = append(os.Environ(), inv.Env...)
	}
	if len(inv.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(inv.Stdin)
	}

	limit := r.MaxCaptureBytes
	if limit == 0 {
		limit = DefaultMaxCaptureBytes
	}
	var out, errb boundedBuffer
	out.limit, errb.limit = limit, limit
	cmd.Stdout, cmd.Stderr = &out, &errb

	runErr := cmd.Run()
	captured := Command{
		Name:   inv.Name,
		Args:   inv.Args,
		Stdout: out.Bytes(),
		Stderr: errb.Bytes(),
	}

	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			captured.ExitCode = ee.ExitCode()
			return captured, nil // ran and exited non-zero — not an error
		}
		captured.ExitCode = -1
		return captured, runErr // failed to start / cancelled / timed out
	}
	return captured, nil
}

// Session pairs a Runner (input) with a Registry (output). It is the only place
// the two halves meet, and it is optional sugar: callers who already have raw
// output just call Registry.Compress directly.
type Session struct {
	Runner   Runner
	Registry *Registry
}

// NewSession composes a Runner and a Registry.
func NewSession(runner Runner, reg *Registry) *Session {
	return &Session{Runner: runner, Registry: reg}
}

// DefaultSession is ExecRunner + Default(): run a command and compress its
// output with the built-in filters, all in process.
func DefaultSession() *Session {
	return NewSession(ExecRunner{}, Default())
}

// Run executes the invocation and compresses its output. The returned Command
// is the raw capture (so callers can keep it); Result is the compressed view. A
// non-zero exit is reflected in both, not returned as an error.
func (s *Session) Run(ctx context.Context, inv Invocation) (Command, Result, error) {
	cmd, err := s.Runner.Run(ctx, inv)
	if err != nil {
		return cmd, Result{}, err
	}
	return cmd, s.Registry.Compress(cmd), nil
}

// boundedBuffer is an io.Writer that accepts up to `limit` bytes and drops the
// rest, so a chatty command can't exhaust memory. limit <= 0 means unbounded.
type boundedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.limit > 0 && b.buf.Len() >= b.limit {
		b.truncated = true
		return len(p), nil // pretend we wrote it so the child doesn't block
	}
	if b.limit > 0 && b.buf.Len()+len(p) > b.limit {
		room := b.limit - b.buf.Len()
		b.buf.Write(p[:room])
		b.truncated = true
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *boundedBuffer) Bytes() []byte { return b.buf.Bytes() }
