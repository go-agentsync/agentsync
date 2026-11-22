package main

import (
	"strings"
	"testing"
	"time"
)

// A tracker write touches no file of the holder's: it needs no lease, and the
// holder is only named. Observed 2026-11-18: ghissue --new was refused for 30+
// minutes by another session's claim.
func TestATrackerWriteIsAllowedWhereAnotherSessionHoldsTheClaim(t *testing.T) {
	env, s := gateFixture(t)
	if _, err := s.Acquire("go-numerics/optimize", them, "refbench", 45*time.Minute); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{
		"ghissue go-numerics/optimize --new --title T --body-file f",
		"ghissue go-numerics/optimize --new --title T --body-file f --dry-run",
		"ghissue go-numerics/optimize 12 --body-file f",
		"ghissue go-numerics/optimize 12 --close --reason not_planned",
		"ghissue go-numerics/optimize 12 --reopen --body-file=f",
		"ghissue --repo go-numerics/optimize 12 --body-file f",
		`ghissue go-numerics/optimize --new --title "a b" --body-file f --prior-art "read go-numerics/x"`,
	} {
		denied, reason, errText := ask(env, payload(t, me, "/work/opt", cmd))
		if denied {
			t.Errorf("%q refused, but a tracker write needs no lease: %s", cmd, reason)
		}
		if !strings.Contains(errText, "22222222") || !strings.Contains(errText, "tracker") {
			t.Errorf("%q: the notice must name the holder and say tracker; stderr %q", cmd, errText)
		}
	}
}

// ⛔⛔ AN INSTRUCTION THIS TOOL GIVES HAS TO BE ONE THIS TOOL ACCEPTS.
//
// The tracker notice above ends with a command to run, and its last argument
// used to be short(l.Owner) -- a SESSION ID -- while `tell` takes a RESOURCE. So
// the notice told every session to open a queue on a pseudo-resource nobody
// will ever claim. Measured 2026-11-22: twice in one hour, by a session doing
// exactly what the notice said, and both handovers were lost.
//
// This repository already paid for this once, from the other direction:
// `messages o/r --max-age 0` was printed by this program and answered `usage:`.
// So the test does not check the wording -- it takes the command out of the
// notice and HANDS IT TO tell, which is the only check that cannot drift from
// what tell accepts.
func TestTheNoticesOwnCommandIsOneTellAccepts(t *testing.T) {
	env, s := gateFixture(t)
	if _, err := s.Acquire("go-numerics/optimize", them, "refbench", 45*time.Minute); err != nil {
		t.Fatal(err)
	}
	_, _, errText := ask(env, payload(t, me, "/work/opt", "ghissue go-numerics/optimize --new --title T --body-file f"))
	i := strings.Index(errText, "agentsync tell ")
	if i < 0 {
		t.Fatalf("the notice no longer names a tell command, so this test measures nothing:\n%s", errText)
	}
	arg := strings.Fields(errText[i+len("agentsync tell "):])
	if len(arg) == 0 {
		t.Fatalf("the tell command in the notice has no argument:\n%s", errText)
	}
	if arg[0] != "go-numerics/optimize" {
		t.Errorf("the notice says `agentsync tell %s`; it must name the RESOURCE, go-numerics/optimize", arg[0])
	}
	// The part that cannot go stale: tell itself judges the argument. If the
	// notice ever names a session again, this fails without anyone having to
	// remember what a session id looks like.
	//
	// ⛔ THE CLAIM BELOW NAMES THE RESOURCE LITERALLY, NEVER arg[0]. Written as
	// Acquire(arg[0], ...) -- the obvious way -- it taught the store that the
	// string read out of the notice IS a resource, so tell accepted it and this
	// half of the test passed WITH the defect reinstated. Measured 2026-11-22:
	// only the comparison above went red. Two errors cancelling, the shape of
	// [[proxy-measure-lets-two-errors-cancel]]: the fixture must not learn its
	// facts from the thing under measurement.
	sandbox(t, me)
	if _, err := store().Acquire("go-numerics/optimize", them, "refbench", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := tell([]string{arg[0], "the notice told me to send this"}); err != nil {
		t.Errorf("tell refuses the command the notice prints: %v", err)
	}
}

// The other side of the boundary: everything that is not a tracker write is
// still refused, and so is a ghissue flag the classifier does not know.
func TestACodeWriteOrAnUnclassifiedCommandIsStillRefused(t *testing.T) {
	env, s := gateFixture(t)
	if _, err := s.Acquire("go-numerics/optimize", them, "refbench", 45*time.Minute); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{
		"gitpush -u origin topic",
		"ghmerge go-numerics/optimize 62",
		"ghrelease go-numerics/optimize 62 v0.1.0",
		"ghpr go-numerics/optimize 62 edit",
		"ghrel go-numerics/optimize v0.1.0 edit --title T",
		"ghrun go-numerics/optimize dispatch ci.yml --ref main",
		"ghissue go-numerics/optimize 12 --edit --title T",
		"ghissue go-numerics/optimize 12 --delete --confirm go-numerics/optimize#12",
		"ghissue go-numerics/optimize --new --title T --body-file f --frobnicate",
		"ghissue go-numerics/optimize 12 --body-file f --edit",
		"ghissue go-numerics/optimize --new --title T --body-file f && gitpush -u origin topic",
	} {
		if denied, _, _ := ask(env, payload(t, me, "/work/opt", "cd /work/opt && "+cmd)); !denied {
			t.Errorf("%q was allowed into a repository claimed by another session", cmd)
		}
	}
}

func TestATrackerWriteWithNoClaimAtAllIsAllowedSilently(t *testing.T) {
	env, _ := gateFixture(t)
	denied, reason, errText := ask(env, payload(t, me, "/work/opt", "ghissue go-numerics/optimize --new --title T --body-file f"))
	if denied || strings.Contains(errText, "claimed") {
		t.Fatalf("denied=%v reason=%q stderr=%q", denied, reason, errText)
	}
}
