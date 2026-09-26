package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// anIndexWithALandingBlock builds the shape this command exists for: a hub the
// index already names, and a landing section three rows long.
func anIndexWithALandingBlock(t *testing.T) (dir, index string) {
	t.Helper()
	dir = t.TempDir()
	index = filepath.Join(dir, "MEMORY.md")
	writeMem(t, dir, "MEMORY.md", `# Memory index

- [the hub](hub.md): where folded rows live

## Atterrissage

- [one](one.md): the first lesson
- [two](two.md): the second
- [three](three.md): and a third
`)
	writeMem(t, dir, "hub.md", "Rows folded out of the index land here.\n")
	for _, n := range []string{"one", "two", "three"} {
		writeMem(t, dir, n+".md", "a memory\n")
	}
	t.Setenv("AGENTSYNC_MEMORY", index)
	return dir, index
}

func TestFoldMovesTheRowsAndKeepsThemReachable(t *testing.T) {
	dir, index := anIndexWithALandingBlock(t)
	all, _ := memoriesIn(dir)
	was := reachableFrom(index, dir, all)

	out := capture(t, func() {
		if err := memFold([]string{"--hub", "hub"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "moved 3 rows") {
		t.Errorf("did not say what it moved:\n%s", out)
	}

	idx, _ := os.ReadFile(index)
	for _, n := range []string{"one.md", "two.md", "three.md"} {
		if strings.Contains(string(idx), n) {
			t.Errorf("%s is still in the index", n)
		}
	}
	hub, _ := os.ReadFile(filepath.Join(dir, "hub.md"))
	for _, n := range []string{"one.md", "two.md", "three.md"} {
		if !strings.Contains(string(hub), n) {
			t.Errorf("%s did not land in the hub", n)
		}
	}
	now := reachableFrom(index, dir, all)
	if len(now) != len(was) {
		t.Errorf("reachable %d → %d; folding must not lose one", len(was), len(now))
	}
}

// TestLostByNamesWhatStoppedBeingReachable. The fold's post-condition cannot
// fire for a correct fold — the pointer it writes names the hub and the rows
// are copied verbatim — so its comparison is tested here rather than through
// the command. A guard nothing exercises is a guard nobody notices breaking.
func TestLostByNamesWhatStoppedBeingReachable(t *testing.T) {
	was := map[string]bool{"a.md": true, "b.md": true, "c.md": true}
	if got := lostBy(was, map[string]bool{"a.md": true, "b.md": true, "c.md": true}); len(got) != 0 {
		t.Errorf("nothing was lost and it named %v", got)
	}
	got := lostBy(was, map[string]bool{"b.md": true})
	if len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Errorf("lostBy = %v, want [a c]", got)
	}
	// The other direction: something NEW being reachable is not a loss.
	if got := lostBy(was, map[string]bool{"a.md": true, "b.md": true, "c.md": true, "d.md": true}); len(got) != 0 {
		t.Errorf("a gain was read as a loss: %v", got)
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	dir, index := anIndexWithALandingBlock(t)
	before, _ := os.ReadFile(index)
	beforeHub, _ := os.ReadFile(filepath.Join(dir, "hub.md"))

	out := capture(t, func() {
		if err := memFold([]string{"--hub", "hub", "--dry-run"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "would move 3 rows") {
		t.Errorf("a dry run must still say what it would do:\n%s", out)
	}
	after, _ := os.ReadFile(index)
	afterHub, _ := os.ReadFile(filepath.Join(dir, "hub.md"))
	if string(after) != string(before) || string(afterHub) != string(beforeHub) {
		t.Error("--dry-run wrote something")
	}
}

// TestOneRowIsNotWorthFolding — a hub row in place of one index row is the
// same length and one more hop.
func TestOneRowIsNotWorthFolding(t *testing.T) {
	dir := t.TempDir()
	writeMem(t, dir, "MEMORY.md", "- [the hub](hub.md): here\n\n## Atterrissage\n\n- [one](one.md): alone\n")
	writeMem(t, dir, "hub.md", "hub\n")
	writeMem(t, dir, "one.md", "a memory\n")
	t.Setenv("AGENTSYNC_MEMORY", filepath.Join(dir, "MEMORY.md"))
	if err := memFold([]string{"--hub", "hub"}); err == nil {
		t.Error("folded a single row")
	}
}

// TestAMissingHubIsRefusedBeforeAnythingMoves. A hub must already exist and
// say what it collects; creating an empty one would put the rows somewhere
// with no explanation of why they are there.
func TestAMissingHubIsRefusedBeforeAnythingMoves(t *testing.T) {
	_, index := anIndexWithALandingBlock(t)
	before, _ := os.ReadFile(index)
	if err := memFold([]string{"--hub", "not-there"}); err == nil {
		t.Fatal("folded into a hub that does not exist")
	}
	after, _ := os.ReadFile(index)
	if string(after) != string(before) {
		t.Error("the index changed although the hub was missing")
	}
}

// TestFoldTakesTheNamedSection, not just the last one.
func TestFoldTakesTheNamedSection(t *testing.T) {
	dir := t.TempDir()
	writeMem(t, dir, "MEMORY.md", `- [the hub](hub.md): here

## Older

- [a](a.md): one
- [b](b.md): two

## Atterrissage

- [c](c.md): three
- [d](d.md): four
`)
	writeMem(t, dir, "hub.md", "hub\n")
	for _, n := range []string{"a", "b", "c", "d"} {
		writeMem(t, dir, n+".md", "m\n")
	}
	t.Setenv("AGENTSYNC_MEMORY", filepath.Join(dir, "MEMORY.md"))
	if err := memFold([]string{"--hub", "hub", "--section", "## Older"}); err != nil {
		t.Fatal(err)
	}
	idx, _ := os.ReadFile(filepath.Join(dir, "MEMORY.md"))
	if strings.Contains(string(idx), "(a.md)") || strings.Contains(string(idx), "(b.md)") {
		t.Error("the named section was not the one folded")
	}
	if !strings.Contains(string(idx), "(c.md)") || !strings.Contains(string(idx), "(d.md)") {
		t.Error("it folded a section it was not asked for")
	}
	// The defect this caught: everything AFTER the folded section was dropped,
	// heading and all.
	if !strings.Contains(string(idx), "## Atterrissage") {
		t.Error("the section after the folded one is gone")
	}
}

// TestASecondFoldDoesNotEatItsOwnPointer. The quote block a fold leaves behind
// is not a row, so running it twice must find nothing to move rather than
// folding the pointer into the hub.
func TestASecondFoldDoesNotEatItsOwnPointer(t *testing.T) {
	_, index := anIndexWithALandingBlock(t)
	if err := memFold([]string{"--hub", "hub"}); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(index)
	if err := memFold([]string{"--hub", "hub"}); err == nil {
		t.Fatal("a second fold found rows where there are none")
	}
	second, _ := os.ReadFile(index)
	if string(second) != string(first) {
		t.Error("the second fold changed the index")
	}
}
