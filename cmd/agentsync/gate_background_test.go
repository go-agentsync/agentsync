package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// bgPayload is the PreToolUse payload Claude Code documents for Bash:
// tool_input carries command, description, timeout and run_in_background.
func bgPayload(command string, background any) *bytes.Reader {
	in := map[string]any{"command": command, "description": "x", "timeout": 120000}
	if background != nil {
		in["run_in_background"] = background
	}
	b, _ := json.Marshal(map[string]any{
		"session_id": me, "cwd": "/work/opt", "hook_event_name": "PreToolUse",
		"tool_name": "Bash", "tool_input": in,
	})
	return bytes.NewReader(b)
}

// ⛔ 2026-11-22 (agentsync#33, occurrence 7): a subagent handed back with
// `claim --wait 45m` running in the background. The "session silent" rule looks
// at the parent session, which stayed active, so only the command can say it.
func TestAClaimWaitInTheBackgroundIsRefused(t *testing.T) {
	for _, cmd := range []string{
		"agentsync claim --wait 45m go-fleettools/fleettools",
		"agentsync claim --wait=45m go-fleettools/fleettools",
		"agentsync claim -wait 45m go-fleettools/fleettools",
		"agentsync claim --note x --wait 40m go-fleettools/fleettools",
		"cd /work/opt && agentsync claim --wait 45m go-fleettools/fleettools",
		"~/.local/bin/agentsync claim --wait 45m go-fleettools/fleettools",
		"/Users/u/.local/bin/agentsync claim --keep-lease why --wait 1m a/b",
		"nohup agentsync claim --wait 45m a/b",
		"env X=1 agentsync claim --wait 45m a/b",
		"bash -c 'agentsync claim --wait 45m a/b'",
		"agentsync claim --wait 45m a/b && prwait -repo a/b 1",
	} {
		env, _ := gateFixture(t)
		denied, reason, _ := ask(env, bgPayload(cmd, true))
		if !denied {
			t.Errorf("%q in the background was allowed", cmd)
			continue
		}
		if !strings.Contains(reason, "foreground") || !strings.Contains(reason, "turn that uses the lease") {
			t.Errorf("%q: the refusal must say to wait in the foreground, in the turn that uses the lease: %q", cmd, reason)
		}
	}
}

func TestAClaimWaitInTheForegroundIsAllowed(t *testing.T) {
	for _, bg := range []any{false, nil} {
		for _, cmd := range []string{
			"agentsync claim --wait 45m go-fleettools/fleettools",
			"agentsync claim --wait=45m a/b && prwait -repo a/b 1 && agentsync release a/b",
		} {
			env, _ := gateFixture(t)
			if denied, reason, _ := ask(env, bgPayload(cmd, bg)); denied {
				t.Errorf("%q (run_in_background=%v) refused: %s", cmd, bg, reason)
			}
		}
	}
}

// Only the wait is the problem: a background claim without it returns at once,
// and background commands that are not a claim are none of this gate's business.
func TestOtherBackgroundCommandsAreAllowed(t *testing.T) {
	for _, cmd := range []string{
		"agentsync claim --note x a/b",
		"agentsync claims",
		"agentsync release a/b",
		"agentsync tell abc --wait hello",
		"echo agentsync claim --wait 45m a/b",
		"go test ./...",
		"git commit -m 'agentsync claim --wait 45m a/b'",
		"cat <<'EOF'\nagentsync claim --wait 45m a/b\nEOF",
	} {
		env, _ := gateFixture(t)
		if denied, reason, _ := ask(env, bgPayload(cmd, true)); denied {
			t.Errorf("%q refused: %s", cmd, reason)
		}
	}
}

// A run_in_background that is not the boolean true is not a verdict: the gate
// refuses on a definite finding only.
func TestANonBooleanBackgroundFlagIsNotAFinding(t *testing.T) {
	for _, bg := range []any{"true", 1, "yes"} {
		env, _ := gateFixture(t)
		if denied, _, _ := ask(env, bgPayload("agentsync claim --wait 45m a/b", bg)); denied {
			t.Errorf("run_in_background=%#v refused", bg)
		}
	}
}
