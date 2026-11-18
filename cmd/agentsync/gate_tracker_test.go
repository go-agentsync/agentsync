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
