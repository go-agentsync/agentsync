package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot walks up from the test's directory to the checkout that holds it.
//
// It fails rather than skips when it cannot find one: a hygiene check that
// quietly passes because it could not locate the repository is the zero that
// means "could not read", and this fleet has paid for that confusion before.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no .git above %s: this test must run inside the checkout", dir)
		}
		dir = parent
	}
}

// TestNoEditorScratchFileIsTracked refuses the mistake that #34 shipped.
//
// That merge committed lease/.idle.go.atomicwrite-112752008, -526108506 and
// -917872315: two empty files and a 219-line copy of lease/idle.go. The copy is
// the dangerous one. Nothing compiles it, because its name does not end in .go,
// and nothing updates it, so it is a duplicate of source that drifts from the
// original while still reading as the real thing to anyone who opens it.
//
// The editing tool writes these beside the file it is saving, so no code in this
// repository can prevent them; .gitignore stops `git add -A` from sweeping them
// in. This test is the gate for every other route: `git add -f`, a rebase that
// resurrects them, a pattern the ignore file does not cover.
//
// It asks git which files are TRACKED, not which exist on disk: an untracked
// scratch file in a working tree is nobody's problem, and a tracked one is
// everybody's.
func TestNoEditorScratchFileIsTracked(t *testing.T) {
	root := repoRoot(t)
	out, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("git ls-files in %s: %v (this test needs git, and CI has it)", root, err)
	}
	// -z so a path with a space or a newline in it cannot split into two.
	paths := strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
	if len(paths) == 1 && paths[0] == "" {
		t.Fatalf("git ls-files listed NOTHING in %s; that is a broken reading, not a clean repository", root)
	}
	var bad []string
	for _, p := range paths {
		base := filepath.Base(p)
		switch {
		case strings.Contains(base, ".atomicwrite-"):
			bad = append(bad, p)
		case strings.HasPrefix(base, ".#"), strings.HasSuffix(base, "~"), strings.HasSuffix(base, ".orig"), strings.HasSuffix(base, ".rej"):
			bad = append(bad, p)
		}
	}
	if len(bad) > 0 {
		// Both numbers, and never a separate line claiming zero: the first
		// version of this test logged "0 scratch files" directly under a
		// failure that named one, which is a report contradicting its own
		// verdict.
		t.Errorf("%d of %d tracked path(s) are editor/tool scratch files; remove them with git rm and keep .gitignore covering them:\n  %s",
			len(bad), len(paths), strings.Join(bad, "\n  "))
		return
	}
	t.Logf("%d tracked path(s) read, 0 scratch files", len(paths))
}
