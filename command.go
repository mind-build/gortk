package gortk

import "strings"

// This file holds adapters that build a Command from the shapes callers
// actually have on hand — an argv slice or a shell command line — so embedding
// gortk is a one-liner regardless of how the command was run.

// CommandFromArgs builds a Command from an argv slice (argv[0] is the program).
// Use this when you ran the command in exec/argv form, e.g. an exec-style
// request carrying a command name and its arguments.
func CommandFromArgs(argv []string, stdout, stderr []byte, exitCode int) Command {
	c := Command{Stdout: stdout, Stderr: stderr, ExitCode: exitCode}
	if len(argv) > 0 {
		c.Name = argv[0]
		c.Args = argv[1:]
	}
	return c
}

// CommandFromLine builds a Command from a shell command line (the `sh -c "…"`
// form). The line is tokenized with shell-like quoting so `git commit -m "a b"`
// yields Name=git, Args=[commit -m "a b"]. Only the first simple command is
// inspected — everything up to the first pipe/redirect/operator — which is
// enough to pick a filter.
func CommandFromLine(line string, stdout, stderr []byte, exitCode int) Command {
	name, args := ParseCommandLine(line)
	return Command{Name: name, Args: args, Stdout: stdout, Stderr: stderr, ExitCode: exitCode}
}

// ParseCommandLine splits a shell command line into a program name and args,
// honoring single quotes, double quotes, and backslash escapes. It stops at the
// first shell control operator (| & ; < > ( )) so only the leading command is
// returned. It is intentionally small — enough to route to a filter, not a full
// POSIX shell parser.
func ParseCommandLine(line string) (name string, args []string) {
	var tokens []string
	var cur strings.Builder
	hasCur := false
	flush := func() {
		if hasCur {
			tokens = append(tokens, cur.String())
			cur.Reset()
			hasCur = false
		}
	}

	runes := []rune(line)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch c {
		case ' ', '\t', '\n', '\r':
			flush()
		case '|', '&', ';', '<', '>', '(', ')':
			// Control operator — leading command ends here.
			flush()
			i = len(runes)
		case '\'':
			hasCur = true
			for i++; i < len(runes) && runes[i] != '\''; i++ {
				cur.WriteRune(runes[i])
			}
		case '"':
			hasCur = true
			for i++; i < len(runes) && runes[i] != '"'; i++ {
				// Inside double quotes the shell only treats backslash as an
				// escape before " \ $ or `; otherwise the backslash is literal.
				if runes[i] == '\\' && i+1 < len(runes) {
					switch runes[i+1] {
					case '"', '\\', '$', '`':
						i++
					}
				}
				cur.WriteRune(runes[i])
			}
		case '\\':
			hasCur = true
			if i+1 < len(runes) {
				i++
				cur.WriteRune(runes[i])
			}
		default:
			hasCur = true
			cur.WriteRune(c)
		}
	}
	flush()

	if len(tokens) == 0 {
		return "", nil
	}
	// Skip leading VAR=value assignments and an `env` prefix so the real
	// program name is found (e.g. `FOO=1 git status`).
	idx := 0
	for idx < len(tokens) && isAssignment(tokens[idx]) {
		idx++
	}
	if idx >= len(tokens) {
		return "", nil
	}
	return tokens[idx], tokens[idx+1:]
}

func isAssignment(tok string) bool {
	eq := strings.IndexByte(tok, '=')
	if eq <= 0 {
		return false
	}
	for _, r := range tok[:eq] {
		if !(r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}
