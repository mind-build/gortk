package gortk

import (
	"sort"
	"sync"
)

// Discovery records the commands that fell through to lossless passthrough —
// i.e. those gortk has no dedicated filter for. It is the data-driven answer to
// "which filter should I write next?" (rtk's "discover"): rather than porting
// rtk's whole catalog blind, you learn exactly what your agents actually run
// that isn't yet compressed. Wire it as an observer:
//
//	disc := &gortk.Discovery{}
//	reg := gortk.Default().Observe(disc.Observe)
//	...
//	for _, e := range disc.Top(10) {
//	    fmt.Printf("%4d  %s\n", e.Count, e.Command)
//	}
//
// The zero value is ready to use and safe for concurrent use.
type Discovery struct {
	mu     sync.Mutex
	counts map[string]int
	bytes  map[string]int64
}

// Observe records cmd when its result came from passthrough (no filter matched).
// Its signature matches Registry.Observe, so it can be passed directly. Results
// that a real filter produced are ignored — those are already covered.
func (d *Discovery) Observe(cmd Command, res Result) {
	if res.Filter != "passthrough" {
		return
	}
	key := discoverKey(cmd)
	if key == "" {
		return
	}
	d.mu.Lock()
	if d.counts == nil {
		d.counts = map[string]int{}
		d.bytes = map[string]int64{}
	}
	d.counts[key]++
	d.bytes[key] += int64(res.InputBytes)
	d.mu.Unlock()
}

// discoverKey is the grouping key for an uncompressed command: the base program
// name plus its subcommand (e.g. "git rebase", "docker ps"). The subcommand is
// included because filters key off it too, so it points at the exact filter to
// write.
func discoverKey(cmd Command) string {
	name := base(cmd.Name)
	if name == "" {
		return ""
	}
	if sub := cmd.Sub(); sub != "" {
		return name + " " + sub
	}
	return name
}

// DiscoveryEntry is one uncompressed command family and how often it was seen.
type DiscoveryEntry struct {
	Command string // e.g. "git rebase"
	Count   int    // times seen with no matching filter
	Bytes   int64  // total uncompressed bytes these passthroughs carried
}

// Top returns the n most-seen uncompressed commands, highest count first (ties
// broken by total bytes, then name). n <= 0 returns all of them.
func (d *Discovery) Top(n int) []DiscoveryEntry {
	d.mu.Lock()
	entries := make([]DiscoveryEntry, 0, len(d.counts))
	for k, c := range d.counts {
		entries = append(entries, DiscoveryEntry{Command: k, Count: c, Bytes: d.bytes[k]})
	}
	d.mu.Unlock()

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Count != entries[j].Count {
			return entries[i].Count > entries[j].Count
		}
		if entries[i].Bytes != entries[j].Bytes {
			return entries[i].Bytes > entries[j].Bytes
		}
		return entries[i].Command < entries[j].Command
	})
	if n > 0 && len(entries) > n {
		entries = entries[:n]
	}
	return entries
}
