package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-agentsync/agentsync/lease"
)

func post(t *testing.T, env gateEnv, session, cwd, command string) (out, errOut string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{
		"session_id": session, "cwd": cwd, "tool_name": "Bash", "hook_event_name": "PostToolUse",
		"tool_input":    map[string]any{"command": command},
		"tool_response": okResponse(),
	})
	var o, e bytes.Buffer
	gatePost(bytes.NewReader(b), &o, &e, env)
	return o.String(), e.String()
}

// okResponse is the documented Bash tool_response of a command that ran to
// completion: stdout, stderr, interrupted, isImage (Claude Code hooks reference).
func okResponse() map[string]any {
	return map[string]any{"stdout": "merged\n", "stderr": "", "interrupted": false, "isImage": false}
}

// postRaw feeds gatePost a payload built by the caller, for the shapes a failed
// command can arrive in.
func postRaw(t *testing.T, env gateEnv, fields map[string]any) {
	t.Helper()
	fields["tool_name"] = "Bash"
	b, _ := json.Marshal(fields)
	var o, e bytes.Buffer
	gatePost(bytes.NewReader(b), &o, &e, env)
}

// ⛔ Measured 2026-11-22: `ghmerge go-fleettools/docs 44` exited 1 (403, budget
// exhausted) and the hook still released the lease. A release needs the tool's
// exit status to be 0, and Claude Code delivers a non-zero status as
// "Exit code N" in `error` (PostToolUseFailure), or as a string tool_response.
func TestAFailedMergeReleasesNothing(t *testing.T) {
	const cmd = "ghmerge go-fleettools/docs 44"
	const refused = "Exit code 1\nGitHub refused this (403) reporting the core budget exhausted until 17:48:59"
	for name, f := range map[string]map[string]any{
		"failure event":         {"hook_event_name": "PostToolUseFailure", "error": refused},
		"error field":           {"hook_event_name": "PostToolUse", "error": refused, "tool_response": okResponse()},
		"string response":       {"hook_event_name": "PostToolUse", "tool_response": refused},
		"is_error":              {"hook_event_name": "PostToolUse", "is_error": true, "tool_response": okResponse()},
		"interrupted":           {"hook_event_name": "PostToolUse", "tool_response": map[string]any{"stdout": "", "stderr": "", "interrupted": true}},
		"exit_code 1":           {"hook_event_name": "PostToolUse", "tool_response": map[string]any{"stdout": "", "stderr": "x", "interrupted": false, "exit_code": 1}},
		"exitCode 1":            {"hook_event_name": "PostToolUse", "tool_response": map[string]any{"stdout": "", "stderr": "x", "exitCode": 1}},
		"no response at all":    {"hook_event_name": "PostToolUse"},
		"response is a number":  {"hook_event_name": "PostToolUse", "tool_response": 1},
		"exit_code as a string": {"hook_event_name": "PostToolUse", "tool_response": map[string]any{"stdout": "", "exit_code": "1"}},
	} {
		env, s := gateFixture(t)
		if _, err := s.Acquire("go-fleettools/docs", me, "", 45*time.Minute); err != nil {
			t.Fatal(err)
		}
		f["session_id"], f["cwd"], f["tool_input"] = me, "/work/opt", map[string]any{"command": cmd}
		postRaw(t, env, f)
		if !holds(t, s, "go-fleettools/docs", me) {
			t.Errorf("%s: a merge that did not succeed released the lease", name)
		}
	}
}

// Each reason is its own branch, so each is asserted by the reason it gives:
// a case that another branch also catches would let this one be deleted.
func TestEachFailureShapeSaysItsOwnReason(t *testing.T) {
	for name, tc := range map[string]struct {
		fields map[string]any
		want   string
	}{
		"failure event alone": {map[string]any{"hook_event_name": "PostToolUseFailure", "tool_response": okResponse()}, "PostToolUseFailure"},
		"no response alone":   {map[string]any{"hook_event_name": "PostToolUse"}, "no tool_response"},
		"null response":       {map[string]any{"hook_event_name": "PostToolUse", "tool_response": nil}, "no tool_response"},
		"error alone":         {map[string]any{"hook_event_name": "PostToolUse", "tool_response": okResponse(), "error": "Exit code 1"}, "carries an error"},
		"is_error alone":      {map[string]any{"hook_event_name": "PostToolUse", "tool_response": okResponse(), "is_error": true}, "is_error"},
		"empty error is none": {map[string]any{"hook_event_name": "PostToolUse", "tool_response": okResponse(), "error": ""}, ""},
		"null error is none":  {map[string]any{"hook_event_name": "PostToolUse", "tool_response": okResponse(), "error": nil}, ""},
	} {
		b, _ := json.Marshal(tc.fields)
		got := notSucceeded(b)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: notSucceeded = %q, want it to mention %q", name, got, tc.want)
		}
	}
	if got := notSucceeded([]byte("[1]")); got == "" {
		t.Error("a payload that is not an object is no success")
	}
}

// A payload with nothing to gate on says nothing: a hook that printed on every
// Bash command would be noise.
func TestAnUnrelatedCommandIsSilentEvenWithoutASession(t *testing.T) {
	env, _ := gateFixture(t)
	for _, cmd := range []string{"ls", "agentsync claim --wait 45m go-numerics/optimize"} {
		_, _, stderr := ask(env, payload(t, "", "/work/opt", cmd))
		if stderr != "" {
			t.Errorf("%q: stderr %q, want none", cmd, stderr)
		}
	}
}

// A zero exit status, spelled out, still releases.
func TestAnExplicitZeroExitStillReleases(t *testing.T) {
	env, s := gateFixture(t)
	if _, err := s.Acquire("go-fleettools/docs", me, "", 45*time.Minute); err != nil {
		t.Fatal(err)
	}
	postRaw(t, env, map[string]any{
		"session_id": me, "cwd": "/work/opt", "hook_event_name": "PostToolUse",
		"tool_input":    map[string]any{"command": "ghmerge go-fleettools/docs 44"},
		"tool_response": map[string]any{"stdout": "ok", "exit_code": 0},
	})
	if holds(t, s, "go-fleettools/docs", me) {
		t.Fatal("exit_code 0 did not release")
	}
}

// ⛔ Measured 2026-11-22: a plain `ghrelease -h` released a lease. It exits 0, so
// only the command line tells it from a release (publishesNothing).
func TestHelpReleasesNothing(t *testing.T) {
	for _, cmd := range []string{"ghrelease go-gitsafe/gitsafe -h", "ghrelease go-gitsafe/gitsafe --help", "ghmerge go-gitsafe/gitsafe help", "ghrelease go-gitsafe/gitsafe --version"} {
		env, s := gateFixture(t)
		if _, err := s.Acquire("go-gitsafe/gitsafe", me, "", 45*time.Minute); err != nil {
			t.Fatal(err)
		}
		post(t, env, me, "/work/opt", cmd)
		if !holds(t, s, "go-gitsafe/gitsafe", me) {
			t.Errorf("%q released a lease", cmd)
		}
	}
}

func holds(t *testing.T, s lease.Store, repo, who string) bool {
	t.Helper()
	ls, err := s.Covering(repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range ls {
		if l.Owner == who && !l.Expired(gateNow) {
			return true
		}
	}
	return false
}

// Variant (a): the lease kept after the work. A merge that succeeded releases
// it, with nothing for the holder to remember.
func TestASuccessfulPublishReleasesTheHoldersLease(t *testing.T) {
	for _, cmd := range []string{
		"ghmerge go-numerics/optimize 62",
		"ghrelease go-numerics/optimize 62 v0.1.0",
		"cd /work/opt && ghmerge 62",
	} {
		env, s := gateFixture(t)
		if _, err := s.Acquire("go-numerics/optimize", me, "", 45*time.Minute); err != nil {
			t.Fatal(err)
		}
		_, errOut := post(t, env, me, "/work/opt", cmd)
		if holds(t, s, "go-numerics/optimize", me) {
			t.Errorf("%q: the lease survived a successful publish", cmd)
		}
		if !strings.Contains(errOut, "released") {
			t.Errorf("%q: the release must be said, got %q", cmd, errOut)
		}
	}
}

func TestOnlyPublishesThatFinishTheWorkRelease(t *testing.T) {
	for _, cmd := range []string{
		"gitpush -u origin topic", // the PR and the merge still follow
		"ghpr go-numerics/optimize 62 ready",
		"ghmerge go-numerics/optimize 62 --dry-run",
		// --dry-run=true as well as the bare flag: the value spelling is a
		// separate branch in publishesNothing, and mutsweep found that nothing
		// held it -- deleting that branch kept the whole suite green.
		"ghmerge go-numerics/optimize 62 --dry-run=true",
		"git status",
	} {
		env, s := gateFixture(t)
		if _, err := s.Acquire("go-numerics/optimize", me, "", 45*time.Minute); err != nil {
			t.Fatal(err)
		}
		post(t, env, me, "/work/opt", cmd)
		if !holds(t, s, "go-numerics/optimize", me) {
			t.Errorf("%q released a lease it should not have", cmd)
		}
	}
}

// ⛔ Measured 2026-11-22: a plain `ghrelease -h` released this session's lease on
// go-gitsafe/gitsafe, which then had to be re-taken with --keep-lease.
//
// It is its own test rather than another row of the table above because it is
// the one case that no reading of the exit status could ever catch: help EXITS
// 0 and publishes nothing, so only the command line separates it from a release
// that worked. The dry-run row above was already there for the same reason,
// which is what makes this a gap in that reasoning and not a new idea.
//
// It also asserts SILENCE. A message naming a lease is what a session believes:
// one that says "released" about a lease still held is as harmful as releasing
// it, because the holder stops renewing.
func TestAskingForHelpOrAVersionIsNotPublishing(t *testing.T) {
	for _, cmd := range []string{
		"ghrelease -h",
		"ghrelease --help",
		"ghrelease help",
		"ghmerge -h",
		"ghmerge --help",
		"ghmerge --version",
		"cd /work/opt && ghmerge -h",
	} {
		env, s := gateFixture(t)
		if _, err := s.Acquire("go-numerics/optimize", me, "", 45*time.Minute); err != nil {
			t.Fatal(err)
		}
		_, errOut := post(t, env, me, "/work/opt", cmd)
		if !holds(t, s, "go-numerics/optimize", me) {
			t.Errorf("%q released the lease; asking for help publishes nothing", cmd)
		}
		if errOut != "" {
			t.Errorf("%q said %q; it must say nothing at all", cmd, errOut)
		}
	}
}

func TestAKeptLeaseSurvivesAndSaysWhy(t *testing.T) {
	env, s := gateFixture(t)
	if _, err := s.AcquireKeep("go-numerics/optimize", me, "", "ghrelease follows the merge", 45*time.Minute); err != nil {
		t.Fatal(err)
	}
	_, errOut := post(t, env, me, "/work/opt", "ghmerge go-numerics/optimize 62")
	if !holds(t, s, "go-numerics/optimize", me) {
		t.Fatal("a kept lease was released")
	}
	if !strings.Contains(errOut, "ghrelease follows the merge") {
		t.Errorf("the keep reason must be shown, got %q", errOut)
	}
}

func TestAPublishNeverReleasesAnotherSessionsLease(t *testing.T) {
	env, s := gateFixture(t)
	if _, err := s.Acquire("go-numerics/optimize", them, "", 45*time.Minute); err != nil {
		t.Fatal(err)
	}
	post(t, env, me, "/work/opt", "ghmerge go-numerics/optimize 62")
	if !holds(t, s, "go-numerics/optimize", them) {
		t.Fatal("the other session's lease was released by my merge")
	}
}

// Publishing is activity: a holder that is pushing must not read as idle.
func TestAnAllowedPublishCountsAsActivity(t *testing.T) {
	env, s := gateFixture(t)
	early := gateNow.Add(-10 * time.Minute)
	s.Now = func() time.Time { return early }
	if _, err := s.Acquire("go-numerics/optimize", me, "", 45*time.Minute); err != nil {
		t.Fatal(err)
	}
	s.Now = func() time.Time { return gateNow }
	env.store = s
	if denied, reason, _ := ask(env, payload(t, me, "/work/opt", "gitpush -u origin topic")); denied {
		t.Fatalf("holder denied: %s", reason)
	}
	ls, _ := s.List()
	if len(ls) != 1 || !ls[0].LastActive().Equal(gateNow) {
		t.Fatalf("last activity = %v, want %v", ls[0].LastActive(), gateNow)
	}
}

func TestTheRefusalSaysWhenTheHolderIsIdle(t *testing.T) {
	env, s := gateFixture(t)
	s.Idle = 15 * time.Minute
	env.store, env.idle = s, 15*time.Minute
	s.Now = func() time.Time { return gateNow.Add(-20 * time.Minute) }
	if _, err := s.Acquire("go-numerics/optimize", them, "waiting on CI", 90*time.Minute); err != nil {
		t.Fatal(err)
	}
	env.store.Now = func() time.Time { return gateNow }
	denied, reason, _ := ask(env, payload(t, me, "/work/opt", "gitpush -u origin topic"))
	if !denied || !strings.Contains(reason, "idle 20m") || !strings.Contains(reason, "agentsync claim") {
		t.Fatalf("denied=%v reason=%q: it must say the holder is idle and that claim takes it", denied, reason)
	}
}

func TestDescribeShowsIdleAndKeep(t *testing.T) {
	now := gateNow
	l := lease.Lease{Resource: "o/r", Owner: them, Acquired: now.Add(-30 * time.Minute), Expires: now.Add(15 * time.Minute), Keep: "release follows"}
	got := describe(l, now, 15*time.Minute)
	for _, want := range []string{"idle 30m", "takeable", "keep: release follows"} {
		if !strings.Contains(got, want) {
			t.Errorf("describe = %q, missing %q", got, want)
		}
	}
	l.Active = now.Add(-2 * time.Minute)
	if got := describe(l, now, 15*time.Minute); strings.Contains(got, "takeable") || !strings.Contains(got, "idle 2m") {
		t.Errorf("an active lease: %q", got)
	}
}

// Variant (c): a background `claim --wait` whose session has ended.
func TestAWaitForAnEndedSessionIsDropped(t *testing.T) {
	projects := t.TempDir()
	dir := filepath.Join(projects, "-some-project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const sid = "33333333-aaaa-bbbb-cccc-000000000003"
	tr := filepath.Join(dir, sid+".jsonl")
	if err := os.WriteFile(tr, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	clock := func() time.Time { return now }
	grace := 12 * time.Minute

	if err := sessionGone(projects, sid, clock, grace); err != nil {
		t.Fatalf("a session that just wrote is alive: %v", err)
	}
	old := now.Add(-grace - time.Second)
	if err := os.Chtimes(tr, old, old); err != nil {
		t.Fatal(err)
	}
	if err := sessionGone(projects, sid, clock, grace); err == nil {
		t.Fatal("a session silent past the grace must read as gone")
	}
	edge := now.Add(-grace)
	if err := os.Chtimes(tr, edge, edge); err != nil {
		t.Fatal(err)
	}
	if err := sessionGone(projects, sid, clock, grace); err != nil {
		t.Fatalf("silent for exactly the grace is still within it: %v", err)
	}
	// What cannot be read is not a verdict: no transcript, no session id.
	if err := sessionGone(projects, "44444444-aaaa-bbbb-cccc-000000000004", clock, grace); err != nil {
		t.Errorf("unknown session must not be dropped: %v", err)
	}
	if err := sessionGone(projects, "", clock, grace); err != nil {
		t.Errorf("no session id must not be dropped: %v", err)
	}
}

func TestWaitStopsWhenTheRequesterIsGone(t *testing.T) {
	s, c := waiting(t, time.Unix(1000, 0))
	if _, err := s.Acquire("go-pkgx/packages", "alice", "", time.Hour); err != nil {
		t.Fatal(err)
	}
	polls := 0
	gone := func() error {
		polls++
		if polls > 2 {
			return errRequesterGone
		}
		return nil
	}
	var out bytes.Buffer
	_, err := waitAcquireFor(s, "go-pkgx/packages", "bob", "", "", time.Hour, 3*time.Hour, 15*time.Second,
		c.now, c.sleep, &out, gone)
	if err != errRequesterGone {
		t.Fatalf("err = %v, want errRequesterGone\n%s", err, out.String())
	}
	if len(c.slept) > 3 {
		t.Errorf("kept waiting %d times after the requester left", len(c.slept))
	}
}

func TestAnEndedRequesterNeverAcquires(t *testing.T) {
	s, c := waiting(t, time.Unix(1000, 0))
	var out bytes.Buffer
	_, err := waitAcquireFor(s, "go-pkgx/packages", "bob", "", "", time.Hour, time.Hour, time.Second,
		c.now, c.sleep, &out, func() error { return errRequesterGone })
	if err != errRequesterGone {
		t.Fatalf("err = %v", err)
	}
	if ls, _ := s.List(); len(ls) != 0 {
		t.Fatalf("a lease was granted to a session that is gone: %v", ls)
	}
}

func TestClaimKeepLeaseAndClaimsShowIt(t *testing.T) {
	t.Setenv("AGENTSYNC_DIR", t.TempDir())
	t.Setenv("AGENTSYNC_OWNER", me)
	t.Setenv("AGENTSYNC_IDLE", "15m")
	if out := said(t, func() error { return claim([]string{"--keep-lease", "release follows the merge", "o/r"}) }); !strings.Contains(out, "claimed") || !strings.Contains(out, "15m0s") {
		t.Fatalf("claim: %q", out)
	}
	out := said(t, func() error { return claims(nil) })
	for _, want := range []string{"o/r", "idle 0m", "keep: release follows the merge", "(this session)"} {
		if !strings.Contains(out, want) {
			t.Errorf("claims missing %q:\n%s", want, out)
		}
	}
	// Idle is configurable and "0" turns it off.
	t.Setenv("AGENTSYNC_IDLE", "0")
	if got := idleLimit(); got != 0 {
		t.Errorf("AGENTSYNC_IDLE=0 gave %v", got)
	}
}

// A payload for any tool but Bash says nothing about a merge, whatever its
// input happens to contain.
func TestAPostPayloadForAnotherToolReleasesNothing(t *testing.T) {
	env, s := gateFixture(t)
	if _, err := s.Acquire("go-numerics/optimize", me, "", 45*time.Minute); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{
		"session_id": me, "cwd": "/work/opt", "tool_name": "Edit",
		"tool_input": map[string]any{"command": "ghmerge go-numerics/optimize 62"},
	})
	var o, e bytes.Buffer
	gatePost(bytes.NewReader(b), &o, &e, env)
	if !holds(t, s, "go-numerics/optimize", me) {
		t.Fatal("a non-Bash tool released a lease")
	}
}

// Nobody to attribute the publish to: nothing is released.
func TestAPostWithNoSessionReleasesNothing(t *testing.T) {
	env, s := gateFixture(t)
	if _, err := s.Acquire("go-numerics/optimize", them, "", 45*time.Minute); err != nil {
		t.Fatal(err)
	}
	post(t, env, "", "/work/opt", "ghmerge go-numerics/optimize 62")
	if !holds(t, s, "go-numerics/optimize", them) {
		t.Fatal("an ownerless post released a lease")
	}
}

// A merge with nothing to release prints nothing: no empty hook answer.
func TestAPostWithNothingToSayIsSilent(t *testing.T) {
	env, _ := gateFixture(t)
	out, errOut := post(t, env, me, "/work/opt", "ghmerge go-numerics/optimize 62")
	if out != "" || errOut != "" {
		t.Fatalf("out=%q err=%q, want both empty", out, errOut)
	}
}

func TestTheKeptMessageNamesTheIdleLimit(t *testing.T) {
	for _, tc := range []struct {
		idle time.Duration
		want string
	}{{0, "its deadline"}, {15 * time.Minute, "15m0s"}} {
		env, s := gateFixture(t)
		env.idle = tc.idle
		if _, err := s.AcquireKeep("go-numerics/optimize", me, "", "why", 45*time.Minute); err != nil {
			t.Fatal(err)
		}
		_, errOut := post(t, env, me, "/work/opt", "ghmerge go-numerics/optimize 62")
		if !strings.Contains(errOut, "goes idle after "+tc.want) {
			t.Errorf("idle %v: %q", tc.idle, errOut)
		}
	}
}

// A session id that finds no transcript is not gone, even when a file of that
// name sits in the working directory, where a missing directory would point.
func TestSessionGoneNeverReadsTheWorkingDirectory(t *testing.T) {
	const sid = "55555555-aaaa-bbbb-cccc-000000000005"
	work := t.TempDir()
	tr := filepath.Join(work, sid+".jsonl")
	if err := os.WriteFile(tr, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(tr, old, old); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)
	if err := sessionGone(t.TempDir(), sid, time.Now, time.Minute); err != nil {
		t.Fatalf("no transcript under projects, yet gone: %v", err)
	}
}

func TestSessionGoneWhenTheTranscriptVanishes(t *testing.T) {
	projects := t.TempDir()
	dir := filepath.Join(projects, "-p")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const sid = "66666666-aaaa-bbbb-cccc-000000000006"
	if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := statFile
	statFile = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	t.Cleanup(func() { statFile = old })
	if err := sessionGone(projects, sid, time.Now, time.Minute); err != nil {
		t.Fatalf("an unreadable transcript is not a verdict: %v", err)
	}
}

// `gate --post` is wired to gatePost through the real entry point.
func TestGatePostFlagReleasesThroughTheEntryPoint(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "locks")
	t.Setenv("AGENTSYNC_DIR", dir)
	t.Setenv("AGENTSYNC_OWNER", me)
	s := store()
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire("go-numerics/optimize", me, "", 45*time.Minute); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{
		"session_id": me, "cwd": "/work/opt", "tool_name": "Bash",
		"tool_input":    map[string]any{"command": "ghmerge go-numerics/optimize 62"},
		"tool_response": okResponse(),
	})
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	w.Write(b)
	w.Close()
	oldIn, oldOut := os.Stdin, os.Stdout
	devnull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	os.Stdin, os.Stdout = r, devnull
	t.Cleanup(func() { os.Stdin, os.Stdout = oldIn, oldOut; devnull.Close() })
	if err := gate([]string{"--post"}); err != nil {
		t.Fatalf("gate --post: %v", err)
	}
	ls, _ := s.List()
	if len(ls) != 0 {
		t.Fatalf("the lease survived gate --post: %v", ls)
	}
}
