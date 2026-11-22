package modzip

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This repository must be publishable as a Go module.
//
// The failing case this pins: a file named `nul` (or `con`, `prn`, `aux`,
// `com1`..`com9`, `lpt1`..`lpt9`, with or without an extension) anywhere in the
// tree. A fuzz corpus is where one arrives without anyone deciding to add it,
// because seed files are named after the input they hold, and "a NUL byte" is
// naturally written `nul`.
func TestThisModuleCanBePackedAsAZip(t *testing.T) {
	root, err := Root(".")
	if err != nil {
		t.Fatal(err)
	}
	if err := Check(root, "github.com/go-agentsync/agentsync", "v0.0.0-00010101000000-000000000000"); err != nil {
		t.Fatalf("%v\n\nA tag cut from this tree could not be installed: `go install <module>@<tag>` fails in\nevery clone, while every CI lane here stays green because they all build from a\ncheckout. Rename the offending path; do not move the tag.", err)
	}
}

// Control: the check must FAIL on the exact tree that was released, or it
// measures nothing. The reserved name is created here rather than committed,
// because committing it would make THIS repository unpublishable again -- the
// defect under test is a property of the file tree, so the test has to build the
// tree it rejects.
func TestTheCheckRejectsTheNameThatWasReleased(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/m\n\ngo 1.27\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seeds := filepath.Join(dir, "testdata", "fuzz", "FuzzNormalizePath")
	if err := os.MkdirAll(seeds, 0o755); err != nil {
		t.Fatal(err)
	}
	// The file v0.13.0 shipped, by the name it shipped under.
	if err := os.WriteFile(filepath.Join(seeds, "nul"), []byte("go test fuzz v1\nstring(\"\\x00\")\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := Check(dir, "example.com/m", "v0.0.0-00010101000000-000000000000")
	if err == nil {
		t.Fatal("a tree containing testdata/fuzz/FuzzNormalizePath/nul packed cleanly, so this check would not have caught v0.13.0")
	}
	t.Logf("rejected as expected: %v", err)
}

// Root is about go.mod, not about the caller's directory: the module root of
// this package is two levels up, and a check that assumed otherwise would pass
// by looking at the wrong tree.
func TestRootFindsTheModuleRootAndSaysWhenThereIsNone(t *testing.T) {
	root, err := Root(".")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Errorf("Root(\".\") = %q, which holds no go.mod: %v", root, err)
	}

	// A directory with no go.mod above it at all. t.TempDir is under the OS
	// temporary directory, which has none.
	//
	// ⛔ The REASON is asserted, not just the error. Once the walk was bounded,
	// both exits returned an error, so `mutsweep` could force `parent == abs`
	// false and SURVIVE: the walk then ran to the bound and reported that
	// instead, which an `err != nil` test reads as the same answer.
	got, err := Root(t.TempDir())
	if err == nil {
		t.Fatalf("Root of a tree with no go.mod returned %q and no error", got)
	}
	if !strings.Contains(err.Error(), "no go.mod at or above") {
		t.Errorf("Root reached the filesystem root but did not say so: %v", err)
	}
}

// The walk must return even when the step never shortens its argument.
//
// `mutsweep` forced `parent == abs` false and the mutation HUNG rather than
// failing: that comparison was the loop's only exit. No input can provoke it
// through the real `filepath.Dir`, which is why the step is a var -- and why a
// test asserting only on Root's answers would have left the bound unheld.
func TestRootReturnsEvenWhenTheWalkNeverShortens(t *testing.T) {
	restore := dirOf
	defer func() { dirOf = restore }()
	calls := 0
	dirOf = func(path string) string {
		calls++
		return path + "x" // never shortens, never equals its argument
	}

	done := make(chan error, 1)
	go func() { _, err := Root(t.TempDir()); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Root returned no error for a walk that cannot reach a root")
		}
		if !strings.Contains(err.Error(), "did not reach a filesystem root") {
			t.Errorf("the bound returned the wrong answer: %v", err)
		}
		t.Logf("bounded after %d step(s): %v", calls, err)
	case <-time.After(30 * time.Second):
		t.Fatalf("Root did not return after %d step(s): the walk has no bound", calls)
	}
}

// The absolute-path failure is an error Root reports, not one it drops.
// `filepath.Abs` fails only for a process with no working directory, so this is
// the other answer no input can produce.
func TestRootReportsAnUnresolvableDirectory(t *testing.T) {
	restore := absPath
	defer func() { absPath = restore }()
	absPath = func(string) (string, error) { return "", errors.New("no working directory") }

	got, err := Root(".")
	if err == nil {
		t.Fatalf("Root returned %q and no error when the path could not be resolved", got)
	}
	if !strings.Contains(err.Error(), "no working directory") {
		t.Errorf("Root dropped the cause: %v", err)
	}
}
