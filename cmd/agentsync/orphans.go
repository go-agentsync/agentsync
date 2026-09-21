package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Memories that nothing points at.
//
// mem-verify repairs what THIS session registered. It cannot repair what
// another session wrote and a third overwrote, and those accumulate: 64 of 679
// were unreachable when this was written, holding real work nobody could find
// again.
//
// ⛔ Reachability is TRANSITIVE. A memory linked from a memory linked from the
// index is reachable, and the fleet deliberately uses hub files to keep the
// index small enough to be read at all. Counting only what MEMORY.md mentions
// reported 351 unreachable when the true number was 64 -- and would have sent
// somebody reorganising five times more than was broken.

// linkRE matches both spellings a memory uses to point at another: the
// markdown link the index carries, and the [[wikilink]] a body carries.
//
// The dot is IN the class on purpose. Names like go-filesystems-v0.1.0 exist,
// and a class without it reports them as unreachable when they are merely
// spelled with a version in them -- the instrument inventing the defect.
var linkRE = regexp.MustCompile(`\(([a-z0-9._-]+\.md)\)|\[\[([a-z0-9._-]+)\]\]`)

// linksIn returns every memory named by one file.
func linksIn(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, m := range linkRE.FindAllStringSubmatch(string(b), -1) {
		switch {
		case m[1] != "":
			out = append(out, m[1])
		case m[2] != "":
			out = append(out, m[2]+".md")
		}
	}
	return out
}

func orphans(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("usage: agentsync orphans")
	}
	index := memoryIndex()
	if index == "" {
		return fmt.Errorf("agentsync: no memory index for this directory")
	}
	dir := filepath.Dir(index)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("reading %s: %w", dir, err)
	}
	all := map[string]bool{}
	for _, e := range entries {
		n := e.Name()
		if !e.IsDir() && strings.HasSuffix(n, ".md") && n != "MEMORY.md" {
			all[n] = true
		}
	}

	// Breadth-first from the index, following links through whatever they
	// reach. A memory named by an unreachable memory is still unreachable.
	reached := map[string]bool{}
	frontier := linksIn(index)
	for len(frontier) > 0 {
		var next []string
		for _, n := range frontier {
			if reached[n] || !all[n] {
				continue
			}
			reached[n] = true
			next = append(next, linksIn(filepath.Join(dir, n))...)
		}
		frontier = next
	}

	var lost []string
	for n := range all {
		if !reached[n] {
			lost = append(lost, n)
		}
	}
	sort.Strings(lost)

	fmt.Printf("%d memories, %d reachable, %d unreachable\n", len(all), len(reached), len(lost))
	for _, n := range lost {
		fmt.Printf("  %s\n", strings.TrimSuffix(n, ".md"))
	}
	if len(lost) > 0 {
		// Not an error: this is a report, and a caller scripting it wants the
		// count on stdout rather than a non-zero status it has to special-case.
		fmt.Println("\nnothing points at these. A hub file naming them costs ONE index line;")
		fmt.Println("adding one line each makes the index harder to read, which is what lost them.")
	}
	return nil
}
