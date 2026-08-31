package main

import (
	"strings"
	"testing"
)

const index = `# Memory index

- [a](a.md) — first
- [b](b.md) — second

## HARD RULES (feedback)

- [c](c.md) — third
- [d](d.md) — fourth

## Other

- [e](e.md) — fifth
`

func TestInsertAppendsAtTheEndOfTheSection(t *testing.T) {
	out, changed := insert(index, "- [new](new.md) — hook", "## HARD RULES (feedback)")
	if !changed {
		t.Fatal("reported no change")
	}
	lines := strings.Split(out, "\n")
	var at, dAt, otherAt int
	for i, l := range lines {
		switch {
		case strings.Contains(l, "new.md"):
			at = i
		case strings.Contains(l, "d.md"):
			dAt = i
		case strings.HasPrefix(l, "## Other"):
			otherAt = i
		}
	}
	// After the section's last entry and before the next heading. Inserting at
	// the top of a section puts the line inside the block a neighbouring writer
	// is most likely to re-emit, which is how entries were lost.
	if !(at > dAt && at < otherAt) {
		t.Errorf("inserted at %d; want between %d (last entry) and %d (next heading)", at, dAt, otherAt)
	}
}

func TestInsertIsIdempotent(t *testing.T) {
	line := "- [new](new.md) — hook"
	once, _ := insert(index, line, "## Other")
	twice, changed := insert(once, line, "## Other")
	if changed {
		t.Error("second insert reported a change")
	}
	if strings.Count(twice, "new.md") != 1 {
		t.Errorf("line appears %d times; a repair must never duplicate", strings.Count(twice, "new.md"))
	}
}

func TestInsertFallsBackToTheEndWhenTheSectionIsAbsent(t *testing.T) {
	out, changed := insert(index, "- [new](new.md) — hook", "## No Such Heading")
	if !changed {
		t.Fatal("reported no change")
	}
	if !strings.HasSuffix(out, "- [new](new.md) — hook\n") {
		t.Errorf("did not append at the end:\n%s", out[len(out)-80:])
	}
	// Nothing else may be disturbed by the fallback.
	if !strings.Contains(out, "- [e](e.md) — fifth") {
		t.Error("existing entries were damaged")
	}
}

func TestInsertKeepsEveryExistingLine(t *testing.T) {
	out, _ := insert(index, "- [new](new.md) — hook", "## HARD RULES (feedback)")
	for _, want := range []string{"a.md", "b.md", "c.md", "d.md", "e.md", "# Memory index", "## Other"} {
		if !strings.Contains(out, want) {
			t.Errorf("lost %q", want)
		}
	}
}

func TestInsertHandlesAnIndexWithoutTrailingNewline(t *testing.T) {
	out, changed := insert("# Memory index\n\n- [a](a.md) — first", "- [new](new.md) — hook", "")
	if !changed {
		t.Fatal("reported no change")
	}
	if strings.Contains(out, "first- [new]") {
		t.Error("ran the new line onto the previous one")
	}
}

func TestProjectSlugFlattensUnderscoresToo(t *testing.T) {
	// The home directory here is /Users/david_delavennat, and Claude's project
	// directory is -Users-david-delavennat-…: underscores become dashes as well
	// as slashes. Missing that pointed the tool at a path that does not exist.
	got := projectSlug("/Users/david_delavennat/Documents/VCS/GIT/localhost")
	want := "-Users-david-delavennat-Documents-VCS-GIT-localhost"
	if got != want {
		t.Errorf("projectSlug = %q, want %q", got, want)
	}
}

func TestShortTrimsOnlyUUIDs(t *testing.T) {
	if got := short("d53a55df-e77d-4ab6-8e1d-00870803ea64"); got != "d53a55df" {
		t.Errorf("short(uuid) = %q", got)
	}
	if got := short("alice"); got != "alice" {
		t.Errorf("short(name) = %q, want it untouched", got)
	}
}

func TestEnclosingSectionFindsWhereALineLives(t *testing.T) {
	if got := enclosingSection(index, "- [c](c.md) — third"); got != "## HARD RULES (feedback)" {
		t.Errorf("section = %q, want the HARD RULES heading", got)
	}
	if got := enclosingSection(index, "- [a](a.md) — first"); got != "# Memory index" {
		t.Errorf("section = %q, want the top heading", got)
	}
	if got := enclosingSection(index, "- [zz](zz.md) — absent"); got != "" {
		t.Errorf("section = %q, want empty for a line that is not there", got)
	}
}

func TestRepairPutsALineBackInItsSection(t *testing.T) {
	line := "- [c](c.md) — third"
	sec := enclosingSection(index, line)
	dropped := strings.Replace(index, line+"\n", "", 1)
	repaired, changed := insert(dropped, line, sec)
	if !changed {
		t.Fatal("repair reported no change")
	}
	// The guarantee is that the entry comes back INSIDE its section, not at the
	// exact byte offset it left: insert deliberately appends at the end of a
	// section, because the top of a block is what a neighbouring writer is most
	// likely to re-emit. Without this, a repair appends at the end of the FILE
	// and a few of them scramble the index.
	lines := strings.Split(repaired, "\n")
	at, secAt, nextHeading := -1, -1, len(lines)
	for i, l := range lines {
		switch {
		case strings.TrimSpace(l) == strings.TrimSpace(sec):
			secAt = i
		case secAt >= 0 && nextHeading == len(lines) && strings.HasPrefix(l, "#"):
			nextHeading = i
		}
		if strings.Contains(l, "c.md") {
			at = i
		}
	}
	if !(at > secAt && at < nextHeading) {
		t.Errorf("line at %d; want inside %q (%d..%d)", at, sec, secAt, nextHeading)
	}
	if strings.Count(repaired, "c.md") != 1 {
		t.Errorf("appears %d times", strings.Count(repaired, "c.md"))
	}
}

// A memory is present when the index points at it, whatever the pointing text
// says today. Testing the exact recorded text instead treats a corrected line
// as a lost one, and "repairs" it by restoring the claim that was corrected --
// leaving two rows for one memory, one of them false.
func TestRowLinkingFindsARewordedEntry(t *testing.T) {
	const index = "# Memory index\n" +
		"- [old wording](thing.md) — was true yesterday\n" +
		"- [other](other.md) — unrelated\n"

	got := rowLinking(index, "thing")
	if got != "- [old wording](thing.md) — was true yesterday" {
		t.Errorf("did not find the row linking to thing.md: %q", got)
	}
	if rowLinking(index, "absent") != "" {
		t.Error("found a row for a memory nothing links to")
	}
	// A name that is a prefix of another must not match it.
	if rowLinking("- [x](thingamy.md) — n\n", "thing") != "" {
		t.Error("thing matched thingamy.md")
	}
}
