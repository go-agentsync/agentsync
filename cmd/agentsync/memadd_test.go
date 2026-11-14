package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// agentsync#27: `mem-add file.md "<text>"` printed "indexed" and appended the
// text alone, with no link; mem-verify then called the memory reachable.

func memEnv(t *testing.T) (dir, index string) {
	t.Helper()
	dir = t.TempDir()
	index = filepath.Join(dir, "MEMORY.md")
	t.Setenv("AGENTSYNC_DIR", filepath.Join(t.TempDir(), "state"))
	t.Setenv("AGENTSYNC_MEMORY", index)
	t.Setenv("AGENTSYNC_OWNER", "test-session")
	writeMem(t, dir, "MEMORY.md", "# Memory index\n\n## Lessons\n")
	return dir, index
}

func indexText(t *testing.T, index string) string {
	t.Helper()
	b, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func isUsage(err error) bool {
	var u usageError
	return errors.As(err, &u)
}

func TestMemAddRefusesALineThatDoesNotLinkTheFile(t *testing.T) {
	for _, name := range []string{"a-lesson", "a-lesson.md"} {
		t.Run(name, func(t *testing.T) {
			dir, index := memEnv(t)
			writeMem(t, dir, "a-lesson.md", "---\nname: a-lesson\ndescription: d\n---\nbody\n")
			err := memAdd([]string{name, "⛔ plain text, no link"})
			if !isUsage(err) {
				t.Fatalf("want a usage refusal (exit 2), got %v", err)
			}
			if !strings.Contains(err.Error(), "(a-lesson.md)") {
				t.Errorf("refusal does not name the missing link: %v", err)
			}
			if strings.Contains(indexText(t, index), "plain text") {
				t.Errorf("wrote a line that links nothing:\n%s", indexText(t, index))
			}
		})
	}
}

func TestMemAddAcceptsALinkedLineWithOrWithoutDotMd(t *testing.T) {
	for _, name := range []string{"a-lesson", "a-lesson.md"} {
		t.Run(name, func(t *testing.T) {
			dir, index := memEnv(t)
			writeMem(t, dir, "a-lesson.md", "body\n")
			out := capture(t, func() {
				if err := memAdd([]string{"--section", "## Lessons", name, "- [a lesson](a-lesson.md): why"}); err != nil {
					t.Fatal(err)
				}
			})
			if !strings.Contains(out, "indexed a-lesson") {
				t.Errorf("output %q", out)
			}
			if !strings.Contains(indexText(t, index), "- [a lesson](a-lesson.md): why") {
				t.Errorf("row missing:\n%s", indexText(t, index))
			}
			verify := capture(t, func() { _ = memVerify(nil) })
			if !strings.Contains(verify, "ok       a-lesson") {
				t.Errorf("verify: %s", verify)
			}
		})
	}
}

func TestMemAddBuildsTheLinkFromTheFrontmatter(t *testing.T) {
	dir, index := memEnv(t)
	writeMem(t, dir, "a-lesson.md", "---\nname: A lesson\ndescription: what it taught\n---\nbody\n")
	capture(t, func() {
		if err := memAdd([]string{"a-lesson.md"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(indexText(t, index), "- [A lesson](a-lesson.md): what it taught") {
		t.Errorf("built line wrong:\n%s", indexText(t, index))
	}
	if got := capture(t, func() { _ = memVerify(nil) }); !strings.Contains(got, "ok       a-lesson") {
		t.Errorf("verify: %s", got)
	}
}

func TestMemAddOneArgRefusalsNameWhatIsMissing(t *testing.T) {
	t.Run("no file", func(t *testing.T) {
		_, index := memEnv(t)
		err := memAdd([]string{"ghost"})
		if !isUsage(err) || !strings.Contains(err.Error(), "ghost.md") {
			t.Errorf("got %v", err)
		}
		if strings.Contains(indexText(t, index), "ghost") {
			t.Error("wrote something for a missing file")
		}
	})
	t.Run("no description", func(t *testing.T) {
		dir, index := memEnv(t)
		writeMem(t, dir, "bare.md", "---\nname: bare\n---\nbody\n")
		err := memAdd([]string{"bare"})
		if !isUsage(err) || !strings.Contains(err.Error(), "description") {
			t.Errorf("got %v", err)
		}
		if strings.Contains(indexText(t, index), "bare") {
			t.Error("wrote something without a description")
		}
	})
	t.Run("no arguments and three arguments", func(t *testing.T) {
		memEnv(t)
		if err := memAdd(nil); !isUsage(err) {
			t.Errorf("no args: %v", err)
		}
		if err := memAdd([]string{"a", "b", "c"}); !isUsage(err) {
			t.Errorf("three args: %v", err)
		}
	})
}

// mem-verify must not trust its own record: text in the index that names the
// memory without linking it is not reachability.
func TestMemVerifyDoesNotCallPlainTextReachable(t *testing.T) {
	dir, index := memEnv(t)
	writeMem(t, dir, "a-lesson.md", "body\n")
	capture(t, func() {
		if err := memAdd([]string{"a-lesson", "- [a lesson](a-lesson.md): why"}); err != nil {
			t.Fatal(err)
		}
	})
	_ = index
	// A neighbour rewrites the row so the link is gone; the recorded line is
	// no longer a substring either, but a variant that keeps the TEXT must
	// not count: record the text-only form to model the legacy bug.
	if err := saveRecords([]record{{Name: "a-lesson", Line: "a-lesson: why", Section: "## Lessons", Index: index}}); err != nil {
		t.Fatal(err)
	}
	writeMem(t, dir, "MEMORY.md", "# Memory index\n\n## Lessons\n- a-lesson: why\n")
	out := capture(t, func() { _ = memVerify([]string{"--repair=false"}) })
	if strings.Contains(out, "still reachable") || strings.Contains(out, "ok       a-lesson") {
		t.Errorf("called an unlinked memory reachable:\n%s", out)
	}
}

// A legacy record whose line has no link is text-only: it must be reported,
// not "ok", even though the text sits in the index.
func TestMemVerifyReportsALegacyLinklessRecord(t *testing.T) {
	dir, _ := memEnv(t)
	writeMem(t, dir, "a-lesson.md", "body\n")
	writeMem(t, dir, "MEMORY.md", "# Memory index\n\n## Lessons\n⛔ plain text only\n")
	if err := saveRecords([]record{{Name: "a-lesson", Line: "⛔ plain text only", Section: "## Lessons", Index: filepath.Join(dir, "MEMORY.md")}}); err != nil {
		t.Fatal(err)
	}
	out := capture(t, func() { _ = memVerify([]string{"--repair=false"}) })
	if strings.Contains(out, "still reachable") || strings.Contains(out, "ok       a-lesson") {
		t.Errorf("text-only line called reachable:\n%s", out)
	}
	if !strings.Contains(out, "MISSING  a-lesson") {
		t.Errorf("not reported:\n%s", out)
	}
}
