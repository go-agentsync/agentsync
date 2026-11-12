package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/go-agentsync/agentsync/lease"
)

// gate refuses a publishing command aimed at a repository whose claim the
// session does not hold.
//
// It exists because the claim was advisory and nothing read it at the moment
// that mattered. Two recorded occurrences: 2026-09-02, and 2026-11-12, when one
// session's claim on go-numerics/optimize lapsed, another session claimed it,
// and the first one pushed and merged #62 without claiming again. Nothing was
// damaged; nothing refused it either.
//
// # WHO "THIS SESSION" IS
//
// The same rule as `claim` and `whoami` (owner), with one addition that only a
// hook can make: the PreToolUse payload carries the session's own id.
//
//  1. AGENTSYNC_OWNER, if set -- the explicit override `claim` honours first.
//  2. the payload's session_id -- authoritative, and equal to the
//     CLAUDE_CODE_SESSION_ID that `claim` runs under when Claude Code starts it.
//  3. CLAUDE_CODE_SESSION_ID in the hook's own environment.
//
// With none of the three the gate cannot tell whose claim to look for, and
// allows: owner()'s "unidentified-<host>" fallback would match no lease and so
// refuse every publish on the machine.
//
// # WHAT IT BLOCKS
//
// ONE CONDITION: a definite finding about a publishing command whose target
// repository it resolved. A payload it cannot read, a command it cannot parse,
// a repository it cannot name, a claims directory it cannot open -- all allow,
// and say so on stderr. About 25 sessions share this machine and a gate that
// halted them all because it could not read a file would be a worse defect than
// the one it is here to catch. A refusal is not a verdict.
//
// The decision travels in the JSON on stdout, exit status always 0, so a
// failing exit cannot be mistaken for a block.
const gateUsage = "usage: agentsync gate --hook   (PreToolUse payload on stdin)\n" +
	"       agentsync gate <owner/repo>... (would a publish into it be allowed for this session?)"

// gated are the programs that write to a repository.
var gated = map[string]bool{
	"gitpush": true, "ghmerge": true, "ghrelease": true, "ghpr": true,
	"ghissue": true, "ghrun": true, "ghrel": true,
}

// gateEnv is everything the decision reads, injectable so no test touches the
// real claims directory, clock, environment or git.
type gateEnv struct {
	store lease.Store
	owner string // who is asking; in hook mode resolved from the three below
	// override is AGENTSYNC_OWNER, session is CLAUDE_CODE_SESSION_ID; the
	// payload's session_id sits between them.
	override, session string
	now               func() time.Time
	remote            func(dir, name string) (string, error) // the URL of a git remote
	home              string
}

func gate(args []string) error {
	fs := flag.NewFlagSet("gate", flag.ExitOnError)
	hook := fs.Bool("hook", false, "read a PreToolUse payload on stdin and answer it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	env := gateEnv{store: store(), now: time.Now, remote: gitRemote, home: home()}
	if *hook {
		env.override, env.session = os.Getenv("AGENTSYNC_OWNER"), os.Getenv("CLAUDE_CODE_SESSION_ID")
		gateHook(os.Stdin, os.Stdout, os.Stderr, env)
		return nil // always 0: the decision is in the JSON
	}
	if fs.NArg() == 0 {
		return errors.New(gateUsage)
	}
	env.owner = owner()
	denied := false
	for _, r := range fs.Args() {
		if msg := env.decide(r, os.Stderr); msg != "" {
			denied = true
			fmt.Println(msg)
		} else {
			fmt.Printf("allowed  %s\n", r)
		}
	}
	if denied {
		return errUnavailable
	}
	return nil
}

func firstSet(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// gateHook answers one PreToolUse payload. It always returns normally; the
// caller exits 0.
func gateHook(stdin io.Reader, out, errOut io.Writer, env gateEnv) {
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
		fmt.Fprintln(errOut, "agentsync gate: could not read the hook payload; allowing (a refusal is not a verdict)")
		return
	}
	if p.ToolName != "" && p.ToolName != "Bash" {
		return
	}
	invs := invocations(p.ToolInput.Command, p.Cwd, env.home)
	if len(invs) == 0 {
		return
	}
	env.owner = firstSet(env.override, p.SessionID, env.session)
	if env.owner == "" {
		fmt.Fprintln(errOut, "agentsync gate: cannot tell which session this is; allowing (a refusal is not a verdict)")
		return
	}
	for _, inv := range invs {
		repo, err := env.target(inv)
		if err != nil {
			fmt.Fprintf(errOut, "agentsync gate: %s: %v; allowing (a refusal is not a verdict)\n", inv.prog, err)
			continue
		}
		if msg := env.decide(repo, errOut); msg != "" {
			_ = json.NewEncoder(out).Encode(map[string]any{
				"hookSpecificOutput": map[string]any{
					"hookEventName":            "PreToolUse",
					"permissionDecision":       "deny",
					"permissionDecisionReason": msg,
				},
			})
			return
		}
	}
}

// decide returns the refusal for publishing into repo, or "" to allow.
func (g gateEnv) decide(repo string, errOut io.Writer) string {
	if info, err := os.Stat(g.store.Dir); err != nil || !info.IsDir() {
		// A store that is not there is not a store with nothing in it: the first
		// would refuse every session on a machine where agentsync never ran.
		fmt.Fprintf(errOut, "agentsync gate: claims directory %s unreadable; allowing (a refusal is not a verdict)\n", g.store.Dir)
		return ""
	}
	ls, err := g.store.Covering(repo)
	if err != nil {
		fmt.Fprintf(errOut, "agentsync gate: %v; allowing (a refusal is not a verdict)\n", err)
		return ""
	}
	now := g.now()
	var other, expired *lease.Lease
	for i := range ls {
		l := ls[i]
		switch {
		case l.Expired(now):
			if expired == nil || l.Expires.After(expired.Expires) {
				expired = &ls[i]
			}
		case l.Owner == g.owner:
			return "" // this session holds it
		case other == nil:
			other = &ls[i]
		}
	}
	if other != nil {
		note := ""
		if other.Note != "" {
			note = fmt.Sprintf(" (%q)", other.Note)
		}
		return fmt.Sprintf("agentsync: %s is claimed by another session, %s, until %s%s.\n"+
			"This command publishes into it. Leave it to the holder, send them a message (agentsync tell %s ...), "+
			"or wait for the lease: agentsync claim --wait 45m %s",
			repo, short(other.Owner), other.Expires.Format("15:04:05"), note, repo, repo)
	}
	why := "no session holds " + repo
	if expired != nil {
		who := "the last claim, by " + short(expired.Owner) + ","
		if expired.Owner == g.owner {
			who = "this session's claim"
		}
		why += fmt.Sprintf(": %s expired at %s", who, expired.Expires.Format("15:04:05"))
	}
	return fmt.Sprintf("agentsync: %s.\nThis command publishes into it. Claim it first: agentsync claim --note \"<what>\" %s -- then retry.", why, repo)
}

// invocation is one publishing command found in a Bash command line, with the
// directory it will run in.
type invocation struct {
	prog string
	args []string
	dir  string
}

// invocations finds the gated programs in a command line.
//
// It matches the PROGRAM of each simple command, not the word anywhere in the
// text: `echo gitpush` and a commit message that mentions ghmerge publish
// nothing. The cost is that a here-document whose body has a line beginning
// with one of these words is read as that command; the gate then asks for a
// claim the session ought to hold anyway.
//
// `cd X &&` is followed, because a push is usually written `cd repo && gitpush`.
func invocations(cmd, cwd, homeDir string) []invocation {
	dir := cwd
	var out []invocation
	for _, seg := range splitCommand(cmd) {
		i := 0
		for i < len(seg) && (isAssignment(seg[i]) || seg[i] == "env" || seg[i] == "command" ||
			seg[i] == "exec" || seg[i] == "time" || seg[i] == "nohup") {
			i++
		}
		if i >= len(seg) {
			continue
		}
		prog, args := seg[i], seg[i+1:]
		if prog == "cd" {
			for len(args) > 0 && strings.HasPrefix(args[0], "-") {
				args = args[1:]
			}
			if len(args) > 0 {
				dir = resolveDir(dir, args[0], homeDir)
			}
			continue
		}
		base := filepath.Base(prog)
		if gated[base] {
			out = append(out, invocation{prog: base, args: args, dir: dir})
		}
	}
	return out
}

var assignRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

func isAssignment(s string) bool { return assignRe.MatchString(s) }

func resolveDir(cur, to, homeDir string) string {
	switch {
	case to == "~" || strings.HasPrefix(to, "~/"):
		to = filepath.Join(homeDir, strings.TrimPrefix(to, "~"))
	case to == "$HOME" || strings.HasPrefix(to, "$HOME/"):
		to = filepath.Join(homeDir, strings.TrimPrefix(to, "$HOME"))
	case strings.Contains(to, "$"):
		return cur // cannot know; keep the directory we had
	}
	if filepath.IsAbs(to) {
		return filepath.Clean(to)
	}
	if cur == "" {
		return to
	}
	return filepath.Join(cur, to)
}

// splitCommand splits a shell command line into simple commands, each a list of
// words, honouring quotes and backslashes. Operators (; & | newline and
// parentheses) end a command. It is not a shell: anything it cannot follow
// simply yields words that match no gated program, which allows.
func splitCommand(s string) [][]string {
	var segs [][]string
	var cur []string
	var tok strings.Builder
	in := false
	endWord := func() {
		if in {
			cur = append(cur, tok.String())
			tok.Reset()
			in = false
		}
	}
	endCmd := func() {
		endWord()
		if len(cur) > 0 {
			segs = append(segs, cur)
			cur = nil
		}
	}
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == '\'':
			in = true
			for i++; i < len(rs) && rs[i] != '\''; i++ {
				tok.WriteRune(rs[i])
			}
		case c == '"':
			in = true
			for i++; i < len(rs) && rs[i] != '"'; i++ {
				if rs[i] == '\\' && i+1 < len(rs) {
					i++
				}
				tok.WriteRune(rs[i])
			}
		case c == '\\' && i+1 < len(rs):
			in = true
			i++
			tok.WriteRune(rs[i])
		case c == ' ' || c == '\t':
			endWord()
		case c == '\n' || c == ';' || c == '&' || c == '|' || c == '(' || c == ')':
			endCmd()
		default:
			in = true
			tok.WriteRune(c)
		}
	}
	endCmd()
	return segs
}

var repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// valueFlags take the NEXT word as their value, so that word is not a repository.
var valueFlags = map[string]bool{
	"--evidence": true, "--issues-waiver": true, "--token-file": true, "--wait": true,
	"--waive": true, "--title": true, "--body-file": true, "--notes-file": true,
	"--ref": true, "--input": true, "--inputs-file": true, "--confirm": true,
	"--backfill-max": true, "-o": true, "--push-option": true, "--receive-pack": true,
}

// target names the repository a publishing command writes to, as owner/repo.
func (g gateEnv) target(inv invocation) (string, error) {
	dir, args := inv.dir, inv.args
	remote := ""
	explicit := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "backfill" && inv.prog == "ghrelease" {
			return "", errors.New("backfill spans many repositories")
		}
		switch {
		case a == "-C" && i+1 < len(args):
			i++
			dir = resolveDir(dir, args[i], g.home)
		case a == "--repo" || a == "-repo" || a == "-R":
			if i+1 < len(args) {
				i++
				explicit = args[i]
			}
		case strings.HasPrefix(a, "--repo="), strings.HasPrefix(a, "-repo="):
			explicit = a[strings.Index(a, "=")+1:]
		case valueFlags[a]:
			i++
		case strings.HasPrefix(a, "-"):
		case inv.prog == "gitpush":
			if remote == "" {
				remote = a
			}
		case explicit == "" && inv.prog != "gitpush" && ownerRepo(a) != "":
			explicit = a
		}
	}
	if explicit == "" && inv.prog == "gitpush" && remote != "" {
		if r := ownerRepo(remote); r != "" {
			return r, nil // pushing to a URL or an owner/repo directly
		}
	}
	if explicit != "" {
		if r := ownerRepo(explicit); r != "" {
			return r, nil
		}
		return "", fmt.Errorf("cannot read a repository from %q", explicit)
	}
	if remote == "" {
		remote = "origin"
	}
	if dir == "" {
		return "", errors.New("no repository named and no working directory")
	}
	u, err := g.remote(dir, remote)
	if err != nil {
		return "", fmt.Errorf("remote %q of %s: %v", remote, dir, err)
	}
	if r := ownerRepo(u); r != "" {
		return r, nil
	}
	return "", fmt.Errorf("remote %q of %s is %q, not an owner/repo", remote, dir, u)
}

// ownerRepo reduces an owner/repo, a GitHub URL (of the repository or of a pull
// request in it) or an SSH remote to "owner/repo", or "" when it is none.
func ownerRepo(s string) string {
	if strings.Contains(s, "://") || strings.Contains(s, "@") || strings.Contains(s, "github.com") {
		parts := strings.Split(lease.Normalize(s), "/")
		if len(parts) >= 2 && repoRe.MatchString(parts[0]+"/"+parts[1]) {
			return parts[0] + "/" + parts[1]
		}
		return ""
	}
	if repoRe.MatchString(s) && !strings.HasPrefix(s, ".") {
		return lease.Normalize(s)
	}
	return ""
}

// gitRemote reads the URL of a remote of the checkout in dir.
func gitRemote(dir, name string) (string, error) {
	if strings.HasPrefix(name, "-") {
		return "", errors.New("not a remote name")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, "git", "-C", dir, "remote", "get-url", name).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
