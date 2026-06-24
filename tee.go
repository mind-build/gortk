package gortk

import (
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// This file is the full-output recovery half of "honest about loss". gortk
// records what a filter dropped (Truncation); a Sink lets it also keep the
// dropped bytes somewhere recoverable, so an agent can fetch the one line a
// filter cut instead of re-running the command. Mirrors rtk's "tee".

// FileSink persists full command output to files under Dir, returning each
// file's path as the recovery handle. It is the disk-backed Sink (rtk's tee
// dir). Use it via Registry.WithSink:
//
//	reg := gortk.Default().WithSink(gortk.FileSink{Dir: teeDir})
//
// The zero value writes to os.TempDir()/gortk-tee. FileSink is safe for
// concurrent use; filenames are unique per call.
type FileSink struct {
	// Dir is where full-output files are written. Created on first use. Empty =
	// os.TempDir()/gortk-tee.
	Dir string
}

// teeSeq disambiguates files written in the same instant (Date/random-free).
var teeSeq atomic.Uint64

// Save writes cmd's full output (stderr then stdout, the same order as
// passthrough) to a uniquely named file and returns its path.
func (s FileSink) Save(cmd Command, _ Result) (string, error) {
	dir := s.Dir
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "gortk-tee")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	path := filepath.Join(dir, teeFilename(cmd))
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	// A one-line header records exactly what produced this, so the file is
	// self-describing when an agent opens it.
	if _, err := fmt.Fprintf(f, "$ %s\n", cmdLine(cmd)); err != nil {
		return "", err
	}
	if len(cmd.Stderr) > 0 {
		if _, err := f.Write(cmd.Stderr); err != nil {
			return "", err
		}
		if cmd.Stderr[len(cmd.Stderr)-1] != '\n' {
			if _, err := f.WriteString("\n"); err != nil {
				return "", err
			}
		}
	}
	if _, err := f.Write(cmd.Stdout); err != nil {
		return "", err
	}
	return path, nil
}

// teeFilename builds a collision-free, human-scannable filename for a command:
// "<base>-<contenthash>-<seq>.log".
func teeFilename(cmd Command) string {
	h := fnv.New32a()
	h.Write(cmd.Stdout)
	h.Write(cmd.Stderr)
	name := sanitizeFilename(base(cmd.Name))
	if name == "" {
		name = "cmd"
	}
	return fmt.Sprintf("%s-%08x-%d.log", name, h.Sum32(), teeSeq.Add(1))
}

// cmdLine reconstructs the command line for the tee header.
func cmdLine(cmd Command) string {
	if len(cmd.Args) == 0 {
		return cmd.Name
	}
	return cmd.Name + " " + strings.Join(cmd.Args, " ")
}

// sanitizeFilename keeps only filename-safe characters.
func sanitizeFilename(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		default:
			return '-'
		}
	}, s)
}
