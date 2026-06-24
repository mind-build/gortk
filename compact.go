package gortk

import "strings"

// compact is the final whitespace-collapsing pass enabled by
// Registry.WithCompact (rtk's "-u" ultra-compact spirit). It only ever removes
// blank lines — runs of them collapse to a single blank, and leading/trailing
// blanks are trimmed — so it never touches signal. Any blank lines removed are
// recorded in Truncation like every other loss.
func compact(res Result) Result {
	if res.Text == "" {
		return res
	}
	hadTrailingNL := strings.HasSuffix(res.Text, "\n")
	lines := strings.Split(strings.TrimSuffix(res.Text, "\n"), "\n")

	out := make([]string, 0, len(lines))
	dropped := 0
	prevBlank := true // leading blanks are dropped (prevBlank starts true)
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			if prevBlank {
				dropped++
				continue
			}
			prevBlank = true
			out = append(out, "")
			continue
		}
		prevBlank = false
		out = append(out, line)
	}
	// Trim a trailing blank the loop may have kept.
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
		dropped++
	}

	res.Text = strings.Join(out, "\n")
	if res.Text != "" && hadTrailingNL {
		res.Text += "\n"
	}
	res.Truncation.dropLines(dropped, "compacted "+itoa(dropped)+" blank line(s)")
	return res
}
