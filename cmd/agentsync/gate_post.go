package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-agentsync/agentsync/lease"
)

// defaultIdle is how long a holder may do nothing before another session may
// take its lease.
//
// Fifteen minutes because the cases this exists for -- a CI run, a quota reset,
// a load wait -- each cost the queue behind them far more than that, while a
// holder doing real work on the resource publishes or renews well inside it
// (the work happens in its own clone; the lease covers the writes). The leases
// themselves last 45 minutes, so without this a holder that is only waiting
// blocks everyone else for three times as long. AGENTSYNC_IDLE overrides it;
// "0" turns it off.
const defaultIdle = 15 * time.Minute

// defaultWaitGrace is how long a session's transcript may stay unwritten before
// a `claim --wait` it started is dropped.
//
// Twelve minutes because a foreground command cannot run past ten (the Bash
// tool's own ceiling), so a wait whose session has been silent longer than that
// plus a margin is not being watched by anyone: it is a background task that
// outlived its turn. AGENTSYNC_WAIT_GRACE overrides it.
const defaultWaitGrace = 12 * time.Minute

var errRequesterGone = errors.New("the session that asked for this wait has ended")

func envDuration(name string, def time.Duration) time.Duration {
	if v := os.Getenv(name); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			return d
		}
		fmt.Fprintf(os.Stderr, "agentsync: %s=%q is not a duration; using %s\n", name, v, def)
	}
	return def
}

// finishers are the publishing programs that end the work a lease was taken for:
// the merge, and the release. gitpush and ghpr do not: a push is followed by a
// pull request and a merge, and releasing there would drop the lease in the
// middle of the job.
var finishers = map[string]bool{"ghmerge": true, "ghrelease": true}

// publishesNothing reports whether this invocation cannot have published
// anything, whatever its exit status: a dry run, or a request for help or for a
// version.
//
// ⛔ Measured 2026-11-22: a plain `ghrelease -h` released this session's lease on
// go-gitsafe/gitsafe, which then had to be re-taken with --keep-lease. A request
// for help exits 0 and writes no release, so exit status alone can never
// separate it from a release that worked -- which is why this is a predicate
// over the command line and not a reading of the result.
//
// The dry-run case was already excluded here for exactly this reason; help and
// version are the same reason and were missed because a dry run is something one
// MEANS to do, while -h is something one types in the middle of reading.
//
// A bare `help` word counts too, since these are cobra commands and
// `ghrelease help` is the same request.
//
// ⛔⛔ BOTH GATES CALL THIS, AND THEY WANT OPPOSITE MISTAKES. In the post-tool
// gate, answering "nothing" too often keeps a lease that could have been
// released -- the behaviour this gate replaced, a known cost. In the pre-tool
// gate (#30) the same answer lets a publish run with NO LEASE AT ALL. So there
// is no safe direction any more, and the two shapes that only looked right are
// spelled out rather than trimmed:
//
//   - a bare `help` is cobra's help SUBCOMMAND, so it counts only as the FIRST
//     argument. Anywhere else it is a value -- `ghpr … edit --title help` -- and
//     reading that as a request for help would hand over the repository.
//   - `--dry-run=false` IS a publish. Matching any `dry-run=` prefix, as this
//     did, read it as a dry run. Every one of these flags is declared
//     `[=true|false]` by cobra, so the value is parsed for all four rather than
//     for the one where the mistake was found.
//
// An unparseable value (`--dry-run=maybe`) counts as publishing: the strict
// answer for the pre-tool gate, and for the post-tool one the answer it gave
// before this predicate existed.
func publishesNothing(args []string) bool {
	for i, a := range args {
		if i == 0 && a == "help" {
			return true
		}
		if !strings.HasPrefix(a, "-") {
			continue // a value, not a flag
		}
		name, val, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		switch name {
		case "dry-run", "h", "help", "version":
			if !hasVal {
				return true
			}
			if b, err := strconv.ParseBool(val); err == nil && b {
				return true
			}
		}
	}
	return false
}

// gatePost answers one PostToolUse payload: an invocation that finished the work
// releases the holder's lease unless the holder claimed it with --keep-lease and
// a reason.
//
// It exists because a note told every session to release and none reliably did
// (agentsync#33, six occurrences): the release is the tool's job now.
//
// Like the gate it never fails the command: what it cannot read, it leaves.
//
// ⛔⛔ IT RELEASES ONLY WHEN THE COMMAND'S RESULT SAYS IT SUCCEEDED (notSucceeded).
// Measured 2026-11-22: `ghmerge go-fleettools/docs 44` exited 1, printing
// "GitHub refused this (403) reporting the core budget exhausted", and this hook
// still announced "ghmerge succeeded, so the lease on go-fleettools/docs is
// released"; afterwards `agentsync claims` showed no row for that repository and
// the pull request was still open. The release followed the command's NAME.
// publishesNothing covers the half decidable from the command line (-h, a dry
// run); notSucceeded reads the payload for the rest.
func gatePost(stdin io.Reader, out, errOut io.Writer, env gateEnv) {
	var p struct {
		SessionID string `json:"session_id"`
		Cwd       string `json:"cwd"`
		ToolName  string `json:"tool_name"`
		ToolInput struct {
			Command string `json:"command"`
		} `json:"tool_input"`
	}
	b, err := io.ReadAll(stdin)
	if err != nil || json.Unmarshal(b, &p) != nil {
		fmt.Fprintln(errOut, "agentsync gate --post: could not read the hook payload; releasing nothing")
		return
	}
	if p.ToolName != "" && p.ToolName != "Bash" {
		return
	}
	if why := notSucceeded(b); why != "" {
		fmt.Fprintf(errOut, "agentsync gate --post: %s; releasing nothing\n", why)
		return
	}
	env.owner = firstSet(env.override, p.SessionID, env.session)
	// No owner at all matches no lease: an ownerless lease reads as torn.
	var said []string
	seen := map[string]bool{}
	for _, inv := range invocations(p.ToolInput.Command, p.Cwd, env.home) {
		if !finishers[inv.prog] || publishesNothing(inv.args) {
			continue
		}
		repo, err := env.target(inv)
		if err != nil {
			fmt.Fprintf(errOut, "agentsync gate --post: %s: %v; releasing nothing\n", inv.prog, err)
			continue
		}
		if seen[repo] {
			continue
		}
		seen[repo] = true
		said = append(said, env.finish(repo, inv.prog)...)
	}
	if len(said) == 0 {
		return
	}
	for _, m := range said {
		fmt.Fprintln(errOut, m)
	}
	_ = json.NewEncoder(out).Encode(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "PostToolUse",
			"additionalContext": strings.Join(said, "\n"),
		},
	})
}

// exitKeys are the names a Bash result could carry its exit status under. The
// documented Bash tool_response has none (stdout, stderr, interrupted, isImage;
// a non-zero status arrives as "Exit code N" in `error`, event
// PostToolUseFailure), so these are read only if present: a status that is there
// must be 0.
var exitKeys = []string{"exit_code", "exitCode", "return_code", "returnCode"}

// notSucceeded says why this PostToolUse payload does NOT show a command that ran
// to a zero exit, or "" when it does. It decides from fields only, never from the
// text of the output, and it fails closed: a payload that does not positively
// look like a completed command is no success, because the cost of a lease kept
// is an idle lease that is taken over after the idle limit, while the cost of a
// lease released on a failure is a repository handed away mid-write.
func notSucceeded(raw []byte) string {
	var p map[string]json.RawMessage
	if json.Unmarshal(raw, &p) != nil {
		return "the payload is not an object"
	}
	var s string
	if json.Unmarshal(p["hook_event_name"], &s) == nil && s == "PostToolUseFailure" {
		return "the tool call failed (PostToolUseFailure)"
	}
	if e, ok := p["error"]; ok && string(e) != "null" && string(e) != `""` {
		return "the payload carries an error"
	}
	var b bool
	if json.Unmarshal(p["is_error"], &b) == nil && b {
		return "the payload says is_error"
	}
	resp, ok := p["tool_response"]
	if !ok || string(resp) == "null" {
		return "the payload has no tool_response"
	}
	var r map[string]json.RawMessage
	if json.Unmarshal(resp, &r) != nil {
		return "tool_response is not an object (a failed call delivers its error text as a string)"
	}
	if json.Unmarshal(r["interrupted"], &b) == nil && b {
		return "the command was interrupted"
	}
	for _, k := range exitKeys {
		if v, ok := r[k]; ok {
			var n float64
			if json.Unmarshal(v, &n) != nil || n != 0 {
				return "the command's exit status is not 0"
			}
		}
	}
	return ""
}

// mine returns this session's live leases that are about exactly this repository
// -- its own, or the bare-name spelling of it -- and not the organisation
// around it, which one merge says nothing about.
func (g gateEnv) mine(repo string) []lease.Lease {
	// A missing or unreadable claims directory, or a torn lease in it, comes back
	// as an error with no leases: nothing is this session's, so nothing is released.
	ls, _ := g.store.Covering(repo)
	key := lease.Normalize(repo)
	bare := key[strings.LastIndex(key, "/")+1:]
	var out []lease.Lease
	for _, l := range ls {
		k := lease.Normalize(l.Resource)
		if l.Owner == g.owner && !l.Expired(g.now()) && (k == key || k == bare) {
			out = append(out, l)
		}
	}
	return out
}

// finish releases this session's lease on repo after prog succeeded, and says
// what it did, including a lease it kept and why.
func (g gateEnv) finish(repo, prog string) []string {
	var said []string
	for _, l := range g.mine(repo) {
		rel, kept, err := g.store.ReleaseUnlessKept(l.Resource, g.owner)
		switch {
		case err != nil:
			said = append(said, fmt.Sprintf("agentsync: %s succeeded but the lease on %s could not be released: %v", prog, l.Resource, err))
		case rel:
			said = append(said, fmt.Sprintf("agentsync: %s succeeded, so the lease on %s is released. Claim it again before publishing more (claim --keep-lease \"<why>\" holds it across a merge).", prog, l.Resource))
		case kept != "":
			said = append(said, fmt.Sprintf("agentsync: %s succeeded; the lease on %s is KEPT (%s). It still goes idle after %s without activity, and release it when the work is done: agentsync release %s",
				prog, l.Resource, kept, g.idleText(), l.Resource))
		}
	}
	return said
}

func (g gateEnv) idleText() string {
	if g.idle <= 0 {
		return "its deadline"
	}
	return g.idle.String()
}

// touch records the publish this session was just allowed as activity on every
// lease of its own that covers repo.
func (g gateEnv) touch(repo string, errOut io.Writer) {
	for _, l := range g.mine(repo) {
		if _, err := g.store.Touch(l.Resource, g.owner); err != nil {
			fmt.Fprintf(errOut, "agentsync gate: could not record activity on %s: %v\n", l.Resource, err)
		}
	}
}

// statFile is os.Stat; a test replaces it, because the transcript vanishing
// between the lookup that found it and the stat cannot be provoked on demand.
var statFile = os.Stat

// sessionGone says why a session that asked for a wait no longer wants it, or
// nil. A session is gone when its transcript has not been written for longer
// than grace. Anything that cannot be read -- no session id, no transcript, an
// ambiguous one -- is NOT gone: a refusal is not a verdict, and dropping a live
// session's wait on a guess would be the worse defect.
func sessionGone(projects, sessionID string, now func() time.Time, grace time.Duration) error {
	dir, err := sessionProjectDir(projects, sessionID)
	if err != nil {
		return nil
	}
	if info, err := statFile(filepath.Join(dir, sessionID+".jsonl")); err == nil {
		if silent := now().Sub(info.ModTime()); silent > grace {
			return fmt.Errorf("%w: its transcript has been silent for %s (limit %s)", errRequesterGone, silent.Round(time.Second), grace)
		}
	}
	return nil
}

// describe is the activity part of a `claims` line.
func describe(l lease.Lease, now time.Time, idle time.Duration) string {
	s := fmt.Sprintf("idle %dm", int(now.Sub(l.LastActive())/time.Minute))
	if l.Idle(now, idle) {
		s += fmt.Sprintf(", takeable (idle limit %s)", idle)
	}
	if l.Keep != "" {
		s += "; keep: " + l.Keep
	}
	return s
}
