package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-agentsync/agentsync/lease"
)

var gateNow = time.Date(2026, 11, 12, 16, 0, 0, 0, time.UTC)

// gateFixture returns an env over a real claims directory whose leases are
// taken through the store, so the files are exactly what `claim` writes.
func gateFixture(t *testing.T) (gateEnv, lease.Store) {
	t.Helper()
	s := lease.Store{Dir: filepath.Join(t.TempDir(), "locks"), Now: func() time.Time { return gateNow }}
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	remotes := map[string]string{
		"/work/opt:origin": "https://github.com/go-numerics/optimize.git",
		"/work/opt:fork":   "git@github.com:me/optimize.git",
	}
	return gateEnv{
		store: s, now: func() time.Time { return gateNow }, home: "/home/u",
		override: "", session: "",
		remote: func(dir, name string, push bool) (string, error) {
			if name == "" {
				name = "origin" // git's own default-remote order is tested against real git
			}
			if u, ok := remotes[dir+":"+name]; ok {
				return u, nil
			}
			return "", errors.New("no such remote")
		},
	}, s
}

func payload(t *testing.T, session, cwd, command string) *bytes.Reader {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"session_id": session, "cwd": cwd, "tool_name": "Bash",
		"tool_input": map[string]any{"command": command},
	})
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(b)
}

func ask(env gateEnv, in *bytes.Reader) (denied bool, reason, stderr string) {
	var out, errOut bytes.Buffer
	gateHook(in, &out, &errOut, env)
	var r struct {
		H struct {
			D string `json:"permissionDecision"`
			R string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if out.Len() > 0 {
		_ = json.Unmarshal(out.Bytes(), &r)
	}
	return r.H.D == "deny", r.H.R, errOut.String()
}

const me, them = "11111111-aaaa-bbbb-cccc-000000000001", "22222222-aaaa-bbbb-cccc-000000000002"

func TestAClaimHeldByThisSessionAllows(t *testing.T) {
	env, s := gateFixture(t)
	if _, err := s.Acquire("go-numerics/optimize", me, "", 45*time.Minute); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{
		"cd /work/opt && gitpush -u origin topic",
		"ghmerge go-numerics/optimize 62",
		"ghrelease go-numerics/optimize 62 v0.1.0",
		"ghpr go-numerics/optimize 62 ready",
		"ghissue go-numerics/optimize --new --title T --body-file f",
		"ghrun go-numerics/optimize dispatch ci.yml --ref main",
		"ghrel go-numerics/optimize v0.1.0 edit --title T",
	} {
		if denied, reason, _ := ask(env, payload(t, me, "/work/opt", cmd)); denied {
			t.Errorf("%q denied for the holder: %s", cmd, reason)
		}
	}
}

func TestAClaimHeldByAnotherSessionRefusesAndNamesTheHolder(t *testing.T) {
	env, s := gateFixture(t)
	if _, err := s.Acquire("go-numerics/optimize", them, "refbench", 45*time.Minute); err != nil {
		t.Fatal(err)
	}
	denied, reason, _ := ask(env, payload(t, me, "/work/opt", "cd /work/opt && gitpush -u origin topic"))
	if !denied {
		t.Fatal("a push into a repository claimed by another session was allowed")
	}
	for _, want := range []string{"22222222", "16:45:00", "refbench", "go-numerics/optimize"} {
		if !strings.Contains(reason, want) {
			t.Errorf("the refusal must name %q; it said %q", want, reason)
		}
	}
}

func TestNoClaimAtAllRefusesAndSaysClaimItFirst(t *testing.T) {
	env, _ := gateFixture(t)
	denied, reason, _ := ask(env, payload(t, me, "/work/opt", "ghmerge go-numerics/optimize 62"))
	if !denied || !strings.Contains(reason, "Claim it first") || !strings.Contains(reason, "no session holds") {
		t.Fatalf("denied=%v reason=%q", denied, reason)
	}
}

// The 2026-11-12 case: the session's own claim lapsed, and nothing refused it.
func TestAnExpiredClaimIsNotAClaim(t *testing.T) {
	env, s := gateFixture(t)
	past := lease.Store{Dir: s.Dir, Now: func() time.Time { return gateNow.Add(-2 * time.Hour) }}
	if _, err := past.Acquire("go-numerics/optimize", me, "", 45*time.Minute); err != nil {
		t.Fatal(err)
	}
	denied, reason, _ := ask(env, payload(t, me, "/work/opt", "ghmerge go-numerics/optimize 62"))
	if !denied || !strings.Contains(reason, "this session's claim expired at 14:45:00") ||
		!strings.Contains(reason, "Claim it first") {
		t.Fatalf("denied=%v reason=%q", denied, reason)
	}
}

// Another session's lapsed claim does not block; a live one of mine beside it allows.
func TestAnotherSessionsExpiredClaimDoesNotBlockTheHolder(t *testing.T) {
	env, s := gateFixture(t)
	past := lease.Store{Dir: s.Dir, Now: func() time.Time { return gateNow.Add(-3 * time.Hour) }}
	if _, err := past.Acquire("go-numerics", them, "", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire("go-numerics/optimize", me, "", time.Hour); err != nil {
		t.Fatal(err)
	}
	if denied, reason, _ := ask(env, payload(t, me, "/", "ghmerge go-numerics/optimize 62")); denied {
		t.Fatalf("denied: %s", reason)
	}
}

func TestAnOrganisationClaimCoversItsRepositories(t *testing.T) {
	env, s := gateFixture(t)
	if _, err := s.Acquire("go-numerics", me, "", time.Hour); err != nil {
		t.Fatal(err)
	}
	if denied, reason, _ := ask(env, payload(t, me, "/", "ghmerge go-numerics/optimize 62")); denied {
		t.Fatalf("denied: %s", reason)
	}
	if denied, _, _ := ask(env, payload(t, them, "/", "ghmerge go-numerics/optimize 62")); !denied {
		t.Fatal("another session was allowed under my organisation claim")
	}
}

func TestABareRepositoryNameClaimIsRecognised(t *testing.T) {
	env, s := gateFixture(t)
	if _, err := s.Acquire("optimize", me, "", time.Hour); err != nil {
		t.Fatal(err)
	}
	if denied, reason, _ := ask(env, payload(t, me, "/", "ghmerge go-numerics/optimize 62")); denied {
		t.Fatalf("denied: %s", reason)
	}
}

// ⛔ A refusal is not a verdict: everything the gate cannot read allows.
func TestWhatItCannotReadAllows(t *testing.T) {
	env, s := gateFixture(t)
	if _, err := s.Acquire("go-numerics/optimize", them, "", time.Hour); err != nil {
		t.Fatal(err)
	}
	missing := env
	missing.store = lease.Store{Dir: filepath.Join(t.TempDir(), "nope")}
	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	notDir := env
	notDir.store = lease.Store{Dir: file}

	for _, tc := range []struct {
		name  string
		env   gateEnv
		in    *bytes.Reader
		warns string
	}{
		{"unreadable claims directory", missing, payload(t, me, "/work/opt", "ghmerge go-numerics/optimize 62"), "unreadable"},
		{"claims path is a file", notDir, payload(t, me, "/work/opt", "ghmerge go-numerics/optimize 62"), "unreadable"},
		{"a payload that is not JSON", env, bytes.NewReader([]byte("not json")), "could not read"},
		{"no session identity", env, payload(t, "", "/work/opt", "ghmerge go-numerics/optimize 62"), "which session"},
		{"a remote git cannot name", env, payload(t, me, "/elsewhere", "gitpush origin topic"), "allowing"},
		{"ghrelease backfill spans repositories", env, payload(t, me, "/", "ghrelease backfill"), "allowing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			denied, reason, errOut := ask(tc.env, tc.in)
			if denied {
				t.Fatalf("denied what it could not read: %s", reason)
			}
			if !strings.Contains(errOut, tc.warns) {
				t.Errorf("stderr should say %q, said %q", tc.warns, errOut)
			}
		})
	}
}

func TestCommandsThatPublishNothingAreLeftAlone(t *testing.T) {
	env, s := gateFixture(t)
	if _, err := s.Acquire("go-numerics/optimize", them, "", time.Hour); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{
		"ls -la", "echo 'gitpush ghmerge go-numerics/optimize 62'",
		"git commit -m 'run ghmerge later'", "git commit -m \"gitpush it\"",
		"git commit -F - <<'EOF'\nghmerge go-numerics/optimize 62\nEOF",
		"ls # then gitpush origin topic; ghmerge go-numerics/optimize 62",
	} {
		if denied, _, errOut := ask(env, payload(t, me, "/work/opt", cmd)); denied || errOut != "" {
			t.Errorf("%q: denied=%v stderr=%q", cmd, denied, errOut)
		}
	}
	var out, errOut bytes.Buffer
	b, _ := json.Marshal(map[string]any{"session_id": me, "tool_name": "Edit",
		"tool_input": map[string]any{"command": "ghmerge go-numerics/optimize 62"}})
	gateHook(bytes.NewReader(b), &out, &errOut, env)
	if out.Len() != 0 {
		t.Errorf("a non-Bash tool was answered: %s", out.String())
	}
}

func TestTheTargetIsResolvedFromTheCommand(t *testing.T) {
	env, _ := gateFixture(t)
	for _, tc := range []struct{ cmd, cwd, want string }{
		{"gitpush -u origin topic", "/work/opt", "go-numerics/optimize"},
		{"cd /work/opt && gitpush origin topic", "/", "go-numerics/optimize"},
		{"gitpush fork topic", "/work/opt", "me/optimize"},
		{"gitpush", "/work/opt", "go-numerics/optimize"},
		{"gitpush -C /work/opt origin topic", "/", "go-numerics/optimize"},
		{"gitpush https://github.com/Org/Repo.git topic", "/", "org/repo"},
		{"FOO=1 ~/.local/bin/gitpush origin topic", "/work/opt", "go-numerics/optimize"},
		{"ghmerge 62", "/work/opt", "go-numerics/optimize"},
		{"ghmerge --wait 2m --evidence u o/r 62", "/", "o/r"},
		{"ghmerge --squash Org/Repo 62", "/", "org/repo"},
		{"ghissue -repo o/r --new --title T", "/", "o/r"},
		{"ghpr https://github.com/o/r/pull/62 62 ready", "/", "o/r"},
		{"ghrun o/r.github.io dispatch ci.yml --ref main", "/", "o/r.github.io"},
	} {
		invs := invocations(tc.cmd, tc.cwd, env.home)
		if len(invs) != 1 {
			t.Errorf("%q: %d invocations", tc.cmd, len(invs))
			continue
		}
		got, err := env.target(invs[0])
		if err != nil || got != tc.want {
			t.Errorf("%q: target %q, err %v; want %q", tc.cmd, got, err, tc.want)
		}
	}
}

func TestSeveralPublishingCommandsAreAllChecked(t *testing.T) {
	env, s := gateFixture(t)
	if _, err := s.Acquire("a/mine", me, "", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire("b/theirs", them, "", time.Hour); err != nil {
		t.Fatal(err)
	}
	denied, reason, _ := ask(env, payload(t, me, "/", "ghmerge a/mine 1 && ghmerge b/theirs 2"))
	if !denied || !strings.Contains(reason, "b/theirs") {
		t.Fatalf("denied=%v reason=%q", denied, reason)
	}
}

// The identity rule: AGENTSYNC_OWNER, then the payload's session_id, then
// CLAUDE_CODE_SESSION_ID -- the first two are what `claim` writes under.
func TestTheSessionIdentityRule(t *testing.T) {
	env, s := gateFixture(t)
	if _, err := s.Acquire("o/r", me, "", time.Hour); err != nil {
		t.Fatal(err)
	}
	cmd := "ghmerge o/r 1"
	if denied, _, _ := ask(env, payload(t, me, "/", cmd)); denied {
		t.Error("the payload's session_id was not used")
	}
	e := env
	e.session = me
	if denied, _, _ := ask(e, payload(t, "", "/", cmd)); denied {
		t.Error("CLAUDE_CODE_SESSION_ID was not the fallback")
	}
	e.session = them
	if denied, _, _ := ask(e, payload(t, me, "/", cmd)); denied {
		t.Error("the payload must outrank the hook's environment")
	}
	e.override = them
	if denied, _, _ := ask(e, payload(t, me, "/", cmd)); !denied {
		t.Error("AGENTSYNC_OWNER must outrank the payload, as it does in claim")
	}
}
