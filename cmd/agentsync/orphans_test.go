package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// write puts one memory in place.
func writeMem(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestOrphansFollowsLinksTransitively(t *testing.T) {
	dir := t.TempDir()
	// The index names one hub. The hub names two memories, one of which names
	// a third. Nothing names the last one.
	writeMem(t, dir, "MEMORY.md", "- [a hub](hub.md): everything else hangs here\n")
	writeMem(t, dir, "hub.md", "see [[first]] and [[second]]\n")
	writeMem(t, dir, "first.md", "which mentions [[third]]\n")
	writeMem(t, dir, "second.md", "nothing here\n")
	writeMem(t, dir, "third.md", "reached through two hops\n")
	writeMem(t, dir, "lost.md", "nothing points at this\n")

	t.Setenv("AGENTSYNC_MEMORY", filepath.Join(dir, "MEMORY.md"))
	out := capture(t, func() {
		if err := orphans(nil); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "5 memories, 4 reachable, 1 unreachable") {
		t.Errorf("counted wrong:\n%s", out)
	}
	if !strings.Contains(out, "lost") {
		t.Errorf("did not name the unreachable one:\n%s", out)
	}
	// ⛔ A memory two hops from the index is REACHABLE. Counting only what
	// MEMORY.md mentions reported five times too many when this was written.
	if strings.Contains(out, "  third\n") {
		t.Errorf("reported a transitively reachable memory as lost:\n%s", out)
	}
}

// ⛔ Names carry dots. A link class without one reports go-filesystems-v0.1.0
// as unreachable when it is merely spelled with a version in it.
func TestOrphansSeesADotInAName(t *testing.T) {
	dir := t.TempDir()
	writeMem(t, dir, "MEMORY.md", "- [hub](hub.md)\n")
	writeMem(t, dir, "hub.md", "[[go-filesystems-v0.1.0]] and [[plain]]\n")
	writeMem(t, dir, "go-filesystems-v0.1.0.md", "tagged\n")
	writeMem(t, dir, "plain.md", "ordinary\n")

	t.Setenv("AGENTSYNC_MEMORY", filepath.Join(dir, "MEMORY.md"))
	out := capture(t, func() {
		if err := orphans(nil); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "3 memories, 3 reachable, 0 unreachable") {
		t.Errorf("a dotted name was miscounted:\n%s", out)
	}
}

// A memory named only by an unreachable one is still unreachable: the walk
// starts at the index, not at every file.
func TestOrphansDoesNotRescueThroughAnOrphan(t *testing.T) {
	dir := t.TempDir()
	writeMem(t, dir, "MEMORY.md", "nothing linked\n")
	writeMem(t, dir, "island.md", "points at [[reef]]\n")
	writeMem(t, dir, "reef.md", "only the island knows\n")

	t.Setenv("AGENTSYNC_MEMORY", filepath.Join(dir, "MEMORY.md"))
	out := capture(t, func() {
		if err := orphans(nil); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "2 memories, 0 reachable, 2 unreachable") {
		t.Errorf("an island rescued itself:\n%s", out)
	}
}

func TestOrphansTakesNoArguments(t *testing.T) {
	if err := orphans([]string{"extra"}); err == nil {
		t.Error("an argument was accepted")
	}
}

// capture runs fn with stdout redirected and returns what it printed.
func capture(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = old
	buf := make([]byte, 1<<16)
	n, _ := r.Read(buf)
	r.Close()
	return string(buf[:n])
}
