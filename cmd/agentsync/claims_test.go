package main

import (
	"testing"
	"time"

	"github.com/go-agentsync/agentsync/lease"
)

// claims answers one question — what is taken RIGHT NOW. On this machine it was
// answering it under 527 expired leases for 19 live ones, so a session scanning the
// list to avoid a collision read 97% history to find the 3% that could stop it.
func leases(now time.Time) []lease.Lease {
	return []lease.Lease{
		{Resource: "live/one", Expires: now.Add(time.Hour)},
		{Resource: "dead/one", Expires: now.Add(-time.Hour)},
		{Resource: "dead/two", Expires: now.Add(-time.Minute)},
	}
}

func TestExpiredLeasesAreHiddenByDefault(t *testing.T) {
	now := time.Now()
	shown, hidden := visible(leases(now), now, false)
	if len(shown) != 1 || shown[0].Resource != "live/one" {
		t.Fatalf("shown = %v, want the one live lease", shown)
	}
	if hidden != 2 {
		t.Errorf("hidden = %d, want 2", hidden)
	}
}

// ⛔ Nothing may be dropped without being counted: the caller prints the number, and a
// listing that quietly omits rows is read as a listing of everything.
func TestEveryHiddenLeaseIsCounted(t *testing.T) {
	now := time.Now()
	all := leases(now)
	shown, hidden := visible(all, now, false)
	if len(shown)+hidden != len(all) {
		t.Errorf("%d shown + %d hidden != %d leases: a row vanished unaccounted for",
			len(shown), hidden, len(all))
	}
}

func TestAllShowsTheHistoryAndHidesNothing(t *testing.T) {
	now := time.Now()
	all := leases(now)
	shown, hidden := visible(all, now, true)
	if len(shown) != len(all) || hidden != 0 {
		t.Errorf("--all shown %d hidden %d, want %d and 0", len(shown), hidden, len(all))
	}
}

// ⛔ The boundary belongs to the HOLDER, and this pins it rather than inventing it:
// Expired is now.After(l.Expires), so a lease expiring at the instant asked about is
// still held and is still shown. I first wrote this test the other way round, asserting
// a rule the code never made — and for a coordination tool this direction is the safe
// one: a warning about a resource that is free costs a moment, a missed collision cost
// this fleet an interrupted build.
func TestALeaseExpiringAtThisInstantIsStillHeld(t *testing.T) {
	now := time.Now()
	shown, hidden := visible([]lease.Lease{{Resource: "edge", Expires: now}}, now, false)
	if len(shown) != 1 || hidden != 0 {
		t.Errorf("shown %d hidden %d: a lease expiring at this instant must still be shown",
			len(shown), hidden)
	}
}
