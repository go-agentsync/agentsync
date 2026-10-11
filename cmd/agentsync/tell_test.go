package main

import (
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
