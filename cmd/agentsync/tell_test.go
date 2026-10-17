package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sandbox points the store and the owner at this test, so nothing here touches
// the shared lock directory 23 live sessions are using.
func sandbox(t *testing.T, owner string) {
	t.Helper()
	t.Setenv("AGENTSYNC_DIR", t.TempDir())
	t.Setenv("AGENTSYNC_OWNER", owner)
}

// A refused session can now answer, and the holder sees it when it next looks.
//
// This is the whole sequence the command exists for, and it is the one that
// happened on 2026-10-11: a session was refused go-crdt/collab by a holder about
// to redo work that had already landed, and had nowhere to say so.
func TestARefusedSessionCanTellTheHolderAndTheHolderSeesIt(t *testing.T) {
	dir := t.TempDir()

	// The holder claims it.
	t.Setenv("AGENTSYNC_DIR", dir)
	t.Setenv("AGENTSYNC_OWNER", "holder-1111")
	if out := said(t, func() error { return claim([]string{"--note", "about to fix x/net", "o/r"}) }); !strings.Contains(out, "claimed") {
		t.Fatalf("the holder did not get it: %s", out)
	}

	// Another session is refused, and says why that work is unnecessary.
	t.Setenv("AGENTSYNC_OWNER", "refused-2222")
	if err := claim([]string{"o/r"}); err == nil {
		t.Fatal("the second session was not refused")
	}
	if err := tell([]string{"o/r", "x/net v0.60.0 landed in #235 and is tagged v0.77.0 — nothing to redo"}); err != nil {
		t.Fatal(err)
	}

	// The holder looks again, by renewing, and reads it.
	t.Setenv("AGENTSYNC_OWNER", "holder-1111")
	out := said(t, func() error { return claim([]string{"o/r"}) })
	if !strings.Contains(out, "landed in #235") {
		t.Errorf("the holder's next claim did not show the message:\n%s", out)
	}
	if !strings.Contains(out, "refused") {
		t.Errorf("the message does not say who it is from:\n%s", out)
	}

	// And on release, which is the last moment it would ever look.
	out = said(t, func() error { return release([]string{"o/r"}) })
	if !strings.Contains(out, "landed in #235") {
		t.Errorf("release did not show the message:\n%s", out)
	}
}

// tell says who will read it, and says plainly when that is nobody.
//
// A message left on an unheld resource is still worth leaving — the next
// claimer sees it — but "told nobody" must not read like "told someone".
func TestTellSaysWhetherAnybodyIsThereToRead(t *testing.T) {
	sandbox(t, "sender-3333")

	out := said(t, func() error { return tell([]string{"o/r", "nobody is holding this"}) })
	if !strings.Contains(out, "NOBODY holds it") {
		t.Errorf("telling an unheld resource did not say so:\n%s", out)
	}

	t.Setenv("AGENTSYNC_OWNER", "holder-4444")
	if err := claim([]string{"o/r"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTSYNC_OWNER", "sender-3333")
	out = said(t, func() error { return tell([]string{"o/r", "now somebody is"}) })
	if !strings.Contains(out, "holds it until") {
		t.Errorf("telling a held resource did not name the holder:\n%s", out)
	}
	if strings.Contains(out, "NOBODY") {
		t.Errorf("it said nobody holds a resource that is held:\n%s", out)
	}
}

// Asking what is waiting does not require taking the resource.
func TestMessagesAnswersWithoutClaiming(t *testing.T) {
	sandbox(t, "asker-5555")
	if err := tell([]string{"o/r", "something waiting"}); err != nil {
		t.Fatal(err)
	}

	out := said(t, func() error { return messages([]string{"o/r"}) })
	if !strings.Contains(out, "something waiting") {
		t.Errorf("messages did not show it:\n%s", out)
	}
	// Nothing was claimed by asking.
	out = said(t, func() error { return claims(nil) })
	if strings.Contains(out, "o/r") {
		t.Errorf("asking for messages claimed the resource:\n%s", out)
	}

	// And a resource nobody has said anything about says so, rather than
	// printing an empty list that reads like an answer.
	out = said(t, func() error { return messages([]string{"other/thing"}) })
	if !strings.Contains(out, "nothing has been said") {
		t.Errorf("an empty inbox gave %q", out)
	}
}

// ⛔ A hint this program prints has to be a command this program accepts.
//
// A lease refusal ends with `agentsync messages o/r --max-age 0 for all`, and
// run as printed it answered "usage:": Go's flag package stops at the first
// non-flag argument, so `--max-age` and `0` were positional arguments. The
// test spells the invocation the way the hint does rather than the way the
// parser liked, which is the only version that would have failed before.
func TestTheFlagWorksWhereTheHintPutsIt(t *testing.T) {
	sandbox(t, "asker-7777")
	if err := tell([]string{"o/r", "something waiting"}); err != nil {
		t.Fatal(err)
	}

	// The control first: before the resource, which always worked.
	out := said(t, func() error { return messages([]string{"--max-age", "0", "o/r"}) })
	if !strings.Contains(out, "something waiting") {
		t.Fatalf("the flag before the resource did not work either, so this test says nothing about order:\n%s", out)
	}
	// And after it, which is where the hint and every habit put it.
	out = said(t, func() error { return messages([]string{"o/r", "--max-age", "0"}) })
	if !strings.Contains(out, "something waiting") {
		t.Errorf("the flag after the resource was read as a resource:\n%s", out)
	}
	// Two resources is still a usage error: accepting flags anywhere must not
	// turn a mistake into a silent choice of the first one.
	if err := messages([]string{"o/r", "other/thing"}); err == nil {
		t.Error("two resources were accepted")
	}
}

// ⛔ And the OTHER half of that rule: `mem-add` and `tell` must keep stopping
// at the first positional argument, because their last argument is free text
// that routinely begins with a dash.
//
//	agentsync mem-add thing "- [thing](thing.md): what it is"
//
// is the normal shape of an index line. Parsing on past the name would read
// `- [thing]…` as a flag and refuse the command. This pins both, so that
// "accept flags anywhere" cannot be applied here as a tidy-up.
func TestFreeTextArgumentsMayBeginWithADash(t *testing.T) {
	sandbox(t, "writer-8888")

	line := "- [thing](thing.md): what it is"
	idx := filepath.Join(t.TempDir(), "MEMORY.md")
	if err := os.WriteFile(idx, []byte("# Memory index\n\n## Landing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := memAdd([]string{"--index", idx, "thing", line}); err != nil {
		t.Fatalf("an index line beginning with a dash was refused: %v", err)
	}
	b, err := os.ReadFile(idx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), line) {
		t.Errorf("the line did not land:\n%s", b)
	}

	// Same for a message whose text begins with a dash.
	if err := tell([]string{"o/r", "-- careful, this one is mine"}); err != nil {
		t.Fatalf("a message beginning with a dash was refused: %v", err)
	}
	out := said(t, func() error { return messages([]string{"o/r"}) })
	if !strings.Contains(out, "careful") {
		t.Errorf("the message did not land:\n%s", out)
	}
}

// forget clears them, and says how many it cleared.
func TestForgetSaysHowManyItDropped(t *testing.T) {
	sandbox(t, "holder-6666")
	for _, s := range []string{"first", "second"} {
		if err := tell([]string{"o/r", s}); err != nil {
			t.Fatal(err)
		}
	}
	out := said(t, func() error { return forget([]string{"o/r"}) })
	if !strings.Contains(out, "2 message(s)") {
		t.Errorf("forget did not say how many: %q", out)
	}
	out = said(t, func() error { return messages([]string{"o/r"}) })
	if !strings.Contains(out, "nothing has been said") {
		t.Errorf("messages survived forget:\n%s", out)
	}
}

// The usage errors, each for its own reason.
func TestTellRefusesWhatItCannotSend(t *testing.T) {
	sandbox(t, "sender-7777")
	for _, args := range [][]string{
		{},             // nothing at all
		{"o/r"},        // a resource and no message
		{"o/r", "   "}, // a resource and nothing to say
	} {
		if err := tell(args); err == nil {
			t.Errorf("tell %v was accepted", args)
		}
	}
	// The control: the same shape with something in it works, so the refusals
	// above are about what is missing and not about the command.
	if err := tell([]string{"o/r", "a real message"}); err != nil {
		t.Errorf("a well-formed tell gave %v", err)
	}
}

// said runs a command and returns what it printed, over the capture helper this
// package already had. The error is dropped on purpose: every use here is
// reading the OUTPUT of a command whose success or refusal is asserted
// separately, and a wrapper that returned both would be two things.
func said(t *testing.T, fn func() error) string {
	t.Helper()
	return capture(t, func() { _ = fn() })
}
