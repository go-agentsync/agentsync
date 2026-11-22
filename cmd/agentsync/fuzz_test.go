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

// Fuzz targets for the three parsers of input this program does not control:
// the PreToolUse and PostToolUse payloads on stdin, and the records file. The
// seed corpus is committed under testdata/fuzz/<Target>/, in the layout
// go-agentsync/agentscope uses; `go test` replays every seed on every run and CI
// runs each target for a bounded time on top.
//
// ⛔ THE GATE TARGETS ASSERT WHAT THE GATE IS FOR, NOT THAT IT SURVIVES. "Did not
// crash" would have passed both defects fixed on 2026-11-22 (a publish allowed
// with no lease; a lease released after a failed publish). Each structured target
// builds its input from a decision made BEFORE the gate runs -- which command,
// who holds the repository -- so the expected answer comes from the fuzz inputs,
// never from the gate's own output (a fixture seeded from the subject proves
// nothing).
//
// Nothing here reads or writes the shared store: every store is a t.TempDir()
// handed to the gate through gateEnv, and the records target sets AGENTSYNC_DIR
// to one before root() can be asked (root() panics under `go test` otherwise).

const (
	fzMe   = "11111111-aaaa-bbbb-cccc-000000000001"
	fzThem = "22222222-aaaa-bbbb-cccc-000000000002"
)

var fzNow = time.Date(2026, 11, 22, 20, 0, 0, 0, time.UTC)

// fzEnv is a gate over a store in dir, with no git: a repository is always named
// on the command line in these targets, so the remote lookup must never run.
func fzEnv(dir string) (gateEnv, lease.Store) {
	s := lease.Store{Dir: dir, Now: func() time.Time { return fzNow }}
	return gateEnv{
		store: s, now: func() time.Time { return fzNow }, home: "/home/u",
		override: fzMe, idle: 0,
		remote: func(string, string, bool) (string, error) { return "", errors.New("no git in a fuzz target") },
	}, s
}

// fzClear empties the store, so each iteration starts from the state it names.
func fzClear(t *testing.T, dir string) {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range es {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			t.Fatal(err)
		}
	}
}

// fzOwners are the owners of the live leases covering resource.
func fzOwners(t *testing.T, s lease.Store, resource string) []string {
	t.Helper()
	ls, err := s.Covering(resource)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range ls {
		if !l.Expired(fzNow) {
			out = append(out, l.Owner)
		}
	}
	return out
}

var (
	// fzProgs are the gated programs; ghissue is given a flag that makes it a code
	// write, because a bare `ghissue --new` writes the tracker only and is allowed
	// without a lease by design.
	fzProgs = []string{"gitpush", "ghmerge", "ghrelease", "ghpr", "ghissue", "ghrun", "ghrel"}
	// fzFlags never include a help, version or dry-run flag: those publish nothing
	// and are tested as such elsewhere. Every command built from these publishes.
	fzFlags   = []string{"", "-u", "--force", "62", "--squash", "--title T", "v0.1.0"}
	fzWrap    = []string{"", "sudo ", "timeout 5 ", "cd /work/opt && ", "FOO=1 ", "(", "if true; then ", "echo hi; ", "bash -c '"}
	fzCloser  = []string{"", "", "", "", "", ")", "; fi", "", "'"}
	fzRepos   = []string{"go-numerics/optimize", "o/r", "Org-1/some.repo", "github.com/o/r"}
	fzOtherRp = "x/other"
)

func fzCommand(prog, flag, wrap, closer, repo string) string {
	args := repo + " " + flag
	switch prog {
	case "ghissue":
		args += " --edit"
	}
	// The repository comes first on purpose: gitpush's first positional is the
	// REMOTE and a later one a refspec, so a value placed before it would be read
	// as a remote name and need git to resolve it (which fails open, by design).
	return wrap + prog + " " + args + closer
}

func fzPayload(session, cwd, command string) []byte {
	b, err := json.Marshal(map[string]any{
		"session_id": session, "cwd": cwd, "tool_name": "Bash",
		"tool_input": map[string]any{"command": command},
	})
	if err != nil {
		panic(err)
	}
	return b
}

func fzDecision(out []byte) (deny bool) {
	if len(out) == 0 {
		return false
	}
	var r struct {
		H struct {
			D string `json:"permissionDecision"`
		} `json:"hookSpecificOutput"`
	}
	if json.Unmarshal(out, &r) != nil {
		return false
	}
	return r.H.D == "deny"
}

// FuzzGateHook is the pre-tool gate over a command built from fuzzed choices that
// ALL publish into repo, with the repository held by nobody (holder 0), by this
// session (1), or by another live session (2).
//
// Properties: nobody holds it -> DENIED; another session holds it -> DENIED;
// this session holds it -> allowed. Also, for every holder, a lease of another
// session on an unrelated repository changes nothing.
func FuzzGateHook(f *testing.F) {
	dir := f.TempDir()
	env, s := fzEnv(dir)
	f.Fuzz(func(t *testing.T, prog, flag, wrap, repoI, holder uint8) {
		fzClear(t, dir)
		p, fl := fzProgs[int(prog)%len(fzProgs)], fzFlags[int(flag)%len(fzFlags)]
		w := int(wrap) % len(fzWrap)
		repo := fzRepos[int(repoI)%len(fzRepos)]
		who := int(holder) % 3
		switch who {
		case 1:
			if _, err := s.Acquire(repo, fzMe, "", 45*time.Minute); err != nil {
				t.Fatal(err)
			}
		case 2:
			if _, err := s.Acquire(repo, fzThem, "", 45*time.Minute); err != nil {
				t.Fatal(err)
			}
		}
		// Someone else's claim elsewhere must not matter to any of the three.
		if _, err := s.Acquire(fzOtherRp, fzThem, "", 45*time.Minute); err != nil {
			t.Fatal(err)
		}
		cmd := fzCommand(p, fl, fzWrap[w], fzCloser[w], repo)
		var out, errOut bytes.Buffer
		gateHook(bytes.NewReader(fzPayload(fzMe, "/work/opt", cmd)), &out, &errOut, env)
		deny := fzDecision(out.Bytes())
		switch {
		case who == 0 && !deny:
			t.Fatalf("ALLOWED a publish into %s that nobody holds: %q\nstderr: %s", repo, cmd, errOut.String())
		case who == 2 && !deny:
			t.Fatalf("ALLOWED a publish into %s held by another live session: %q\nstderr: %s", repo, cmd, errOut.String())
		case who == 1 && deny:
			t.Fatalf("DENIED the holder: %q\n%s", cmd, out.String())
		}
	})
}

// FuzzGateHookPayload feeds the raw stdin payload. The command line in it is
// attacker-influenced text (a file the session read can put anything in a tool
// call), and a parse that fails must ALLOW, because a refusal is not a verdict.
//
// Properties: output is empty or one valid JSON object; a refusal is only ever
// issued for a Bash call whose command names a gated program (anything else is a
// false refusal of an unrelated command); with an empty store directory an
// unreadable payload never denies.
func FuzzGateHookPayload(f *testing.F) {
	dir := f.TempDir()
	env, _ := fzEnv(dir)
	f.Fuzz(func(t *testing.T, data []byte) {
		var out, errOut bytes.Buffer
		gateHook(bytes.NewReader(data), &out, &errOut, env)
		if out.Len() > 0 {
			var v map[string]any
			if err := json.Unmarshal(out.Bytes(), &v); err != nil {
				t.Fatalf("output is not one JSON object: %q", out.String())
			}
		}
		if !fzDecision(out.Bytes()) {
			return
		}
		var p struct {
			ToolName  string `json:"tool_name"`
			ToolInput struct {
				Command string `json:"command"`
			} `json:"tool_input"`
		}
		if err := json.Unmarshal(data, &p); err != nil {
			t.Fatalf("DENIED a payload that does not parse: %q", data)
		}
		if p.ToolName != "" && p.ToolName != "Bash" {
			t.Fatalf("DENIED a %s call: %q", p.ToolName, data)
		}
		named := false
		for g := range gated {
			if strings.Contains(p.ToolInput.Command, g) {
				named = true
			}
		}
		if !named {
			t.Fatalf("DENIED a command naming no gated program: %q", p.ToolInput.Command)
		}
	})
}

// fzResponses are the tool_response shapes, index 0 being the only one that
// evidences a command that ran to a zero exit.
var fzResponses = []string{
	`{"stdout":"merged","stderr":"","interrupted":false,"isImage":false}`,
	`null`,
	`"Exit code 1\nGitHub refused this (403)"`,
	`{"stdout":"","stderr":"boom","interrupted":false,"exit_code":1}`,
	`{"stdout":"","stderr":"","interrupted":true}`,
	`{}`,
}

// FuzzGatePost is the post-tool gate over a finishing command (ghmerge or
// ghrelease) or another gated one, with this session holding the repository
// (optionally with --keep-lease) and another session holding an unrelated one.
//
// Properties (these hold today):
//   - another session's lease is never released, whatever the payload;
//   - a lease is released only for ghmerge or ghrelease: gitpush, ghpr, ghissue,
//     ghrun and ghrel never end the work;
//   - a lease claimed with a keep reason is never released.
//
// The property the gate does NOT yet have -- no release without evidence of
// success -- is in fuzz_success_test.go behind the `postsuccess` build tag until
// the change that teaches the gate to read tool_response lands; see that file.
func FuzzGatePost(f *testing.F) {
	dir := f.TempDir()
	env, s := fzEnv(dir)
	f.Fuzz(func(t *testing.T, prog, flag, repoI, resp uint8, keep bool) {
		fzClear(t, dir)
		p, fl := fzProgs[int(prog)%len(fzProgs)], fzFlags[int(flag)%len(fzFlags)]
		repo := fzRepos[int(repoI)%len(fzRepos)]
		if keep {
			if _, err := s.AcquireKeep(repo, fzMe, "", "waiting on CI", 45*time.Minute); err != nil {
				t.Fatal(err)
			}
		} else if _, err := s.Acquire(repo, fzMe, "", 45*time.Minute); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Acquire(fzOtherRp, fzThem, "", 45*time.Minute); err != nil {
			t.Fatal(err)
		}
		cmd := fzCommand(p, fl, "", "", repo)
		r := int(resp) % len(fzResponses)
		body := `{"session_id":` + quote(fzMe) + `,"cwd":"/work/opt","tool_name":"Bash","tool_input":{"command":` + quote(cmd) + `},"tool_response":` + fzResponses[r] + `}`
		var out, errOut bytes.Buffer
		gatePost(strings.NewReader(body), &out, &errOut, env)

		released := !fzHolds(t, s, repo, fzMe)
		if !fzHolds(t, s, fzOtherRp, fzThem) {
			t.Fatalf("RELEASED another session's lease: %s", body)
		}
		if released && p != "ghmerge" && p != "ghrelease" {
			t.Fatalf("RELEASED the lease after %s, which does not end the work: %s", p, body)
		}
		if released && keep {
			t.Fatalf("RELEASED a lease claimed with a keep reason: %s", body)
		}
		fzSuccessProperty(t, released, r, body)
	})
}

// FuzzGatePostPayload feeds the raw PostToolUse stdin.
//
// Properties: another session's lease survives any bytes; this session's lease
// survives any payload that does not even NAME ghmerge or ghrelease; output is
// empty or valid JSON.
func FuzzGatePostPayload(f *testing.F) {
	dir := f.TempDir()
	env, s := fzEnv(dir)
	f.Fuzz(func(t *testing.T, data []byte) {
		fzClear(t, dir)
		if _, err := s.Acquire("o/r", fzMe, "", 45*time.Minute); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Acquire(fzOtherRp, fzThem, "", 45*time.Minute); err != nil {
			t.Fatal(err)
		}
		var out, errOut bytes.Buffer
		gatePost(bytes.NewReader(data), &out, &errOut, env)
		if out.Len() > 0 {
			var v map[string]any
			if err := json.Unmarshal(out.Bytes(), &v); err != nil {
				t.Fatalf("output is not one JSON object: %q", out.String())
			}
		}
		if !fzHolds(t, s, fzOtherRp, fzThem) {
			t.Fatalf("RELEASED another session's lease: %q", data)
		}
		if !bytes.Contains(data, []byte("ghmerge")) && !bytes.Contains(data, []byte("ghrelease")) && !fzHolds(t, s, "o/r", fzMe) {
			t.Fatalf("RELEASED a lease for a payload naming no finishing program: %q", data)
		}
	})
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func fzHolds(t *testing.T, s lease.Store, resource, owner string) bool {
	t.Helper()
	for _, o := range fzOwners(t, s, resource) {
		if o == owner {
			return true
		}
	}
	return false
}

// FuzzParseRecords feeds the records file (what this session claims to have put
// in the index). It is read by mem-verify at the end of a session, so a torn file
// must be an error, not a panic and not an empty "all verified".
//
// Properties: a file that loads survives a save and a load unchanged in length
// and names; a file that does not load returns an error and no records the
// caller could mistake for the session's own.
func FuzzParseRecords(f *testing.F) {
	f.Setenv("AGENTSYNC_DIR", f.TempDir())
	f.Setenv("AGENTSYNC_OWNER", "fuzz-1111")
	f.Fuzz(func(t *testing.T, data []byte) {
		p := recordPath()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		rs, err := loadRecords()
		if err != nil {
			return
		}
		if err := saveRecords(rs); err != nil {
			t.Fatal(err)
		}
		back, err := loadRecords()
		if err != nil {
			t.Fatalf("records that loaded could not be read back: %v", err)
		}
		if len(back) != len(rs) {
			t.Fatalf("round trip changed the count: %d -> %d", len(rs), len(back))
		}
		for i := range rs {
			if back[i].Name != rs[i].Name || back[i].Line != rs[i].Line || back[i].Index != rs[i].Index {
				t.Fatalf("round trip changed record %d: %+v -> %+v", i, rs[i], back[i])
			}
		}
	})
}
