package main

import "testing"

// fzSuccessProperty: a lease is released only for a payload that evidences a
// command that ran to a zero exit (fzResponses[0]). Every other response shape --
// no result, an error string, a non-zero exit, an interruption, an empty object
// -- must leave the lease where it is, because a lease released after a failed
// ghmerge hands a repository away mid-write (measured 2026-11-22).
//
// This property was written behind a `postsuccess` build tag, with a no-op twin
// for the default build, because it was RED on five seeds against the gate as it
// stood: a refused 403, a non-zero exit, an interrupted run, an empty result and
// a null result each released a lease. v0.12.0 (c6b4562) carries notSucceeded,
// which fails closed on all five, so the tag and the twin are gone and the
// assertion runs in every build -- the five seeds now guard the fix instead of
// recording the defect.
func fzSuccessProperty(t *testing.T, released bool, response int, body string) {
	t.Helper()
	if released && response != 0 {
		t.Fatalf("RELEASED a lease for a payload that does not evidence success: %s", body)
	}
}
