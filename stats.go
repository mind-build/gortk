package gortk

import "sync"

// Stats accumulates compression savings across many commands — gortk's answer to
// rtk's "gain" report. Wire it into a Registry as an observer:
//
//	stats := &gortk.Stats{}
//	reg := gortk.Default().Observe(stats.Observe)
//	...
//	fmt.Println(stats.Report()) // total saved across every Compress
//
// The zero value is ready to use and safe for concurrent use.
type Stats struct {
	mu    sync.Mutex
	cmds  int
	in    int64
	out   int64
	dropL int64
}

// Observe records one result's sizes. Its signature matches Registry.Observe, so
// it can be passed directly.
func (s *Stats) Observe(_ Command, res Result) {
	s.mu.Lock()
	s.cmds++
	s.in += int64(res.InputBytes)
	s.out += int64(res.OutputBytes)
	s.dropL += int64(res.Truncation.DroppedLines)
	s.mu.Unlock()
}

// Report returns a snapshot of the accumulated savings.
func (s *Stats) Report() StatsReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	rep := StatsReport{
		Commands:     s.cmds,
		InputBytes:   s.in,
		OutputBytes:  s.out,
		DroppedLines: s.dropL,
	}
	if s.in > s.out {
		rep.SavedBytes = s.in - s.out
	}
	if s.in > 0 {
		rep.SavedFraction = float64(rep.SavedBytes) / float64(s.in)
	}
	return rep
}

// StatsReport is an immutable snapshot of cumulative savings.
type StatsReport struct {
	Commands      int
	InputBytes    int64
	OutputBytes   int64
	SavedBytes    int64
	SavedFraction float64 // [0,1]
	DroppedLines  int64
}

// String renders a one-line summary, e.g.
// "gortk: 12 cmds, 1.2 MiB -> 64 KiB (95% saved)".
func (r StatsReport) String() string {
	return "gortk: " + itoa(r.Commands) + " cmds, " +
		humanBytes(r.InputBytes) + " -> " + humanBytes(r.OutputBytes) +
		" (" + itoa(int(r.SavedFraction*100+0.5)) + "% saved)"
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return itoa(int(n)) + " B"
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	// one decimal place without importing fmt's float formatting cost
	whole := n / div
	frac := (n % div) * 10 / div
	suffix := []string{"KiB", "MiB", "GiB", "TiB"}[exp]
	if frac == 0 {
		return itoa(int(whole)) + " " + suffix
	}
	return itoa(int(whole)) + "." + itoa(int(frac)) + " " + suffix
}
