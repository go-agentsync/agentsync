package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/go-agentsync/agentsync/lease"
	"github.com/go-gitsafe/gitsafe/shellcmd"
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

// waitProg marks an `agentsync claim --wait` among the invocations; it is no
// program of the gated set and names no repository.
const waitProg = "agentsync claim --wait"

const backgroundWaitRefusal = "agentsync: `claim --wait` must not run in the background. A lease is granted to whoever is waiting, " +
	"and a background task outlives the turn that started it, so the lease would be held by nobody while the queue waits behind it.\n" +
	"Wait in the foreground, in the turn that uses the lease: agentsync claim --wait 9m <repo> && <the work> && agentsync release <repo> " +
	"(the Bash tool's own limit is ten minutes; repeat the foreground call if the holder has not finished)."

// isClaimWait reports whether args (after `agentsync`) are a claim that waits.
func isClaimWait(args []string) bool {
	if len(args) == 0 || args[0] != "claim" {
		return false
	}
	for _, a := range args[1:] {
		if !strings.HasPrefix(a, "-") {
			continue
		}
		a = strings.TrimLeft(a, "-")
		if a == "wait" || strings.HasPrefix(a, "wait=") {
			return true
		}
	}
	return false
}

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
	// remote is the URL of a git remote of the checkout in dir. push asks for the
	// URL a push goes to; name "" asks for the remote a push goes to by default.
	remote func(dir, name string, push bool) (string, error)
	home   string
	// idle is the store's idle limit, so a refusal can say the holder is idle.
	idle time.Duration
}

func gate(args []string) error {
	fs := newFlags("gate")
	hook := fs.Bool("hook", false, "read a PreToolUse payload on stdin and answer it")
	postHook := fs.Bool("post", false, "read a PostToolUse payload on stdin; a succeeded ghmerge/ghrelease releases the holder's lease")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	env := gateEnv{store: store(), now: time.Now, remote: gitRemote, home: home(), idle: idleLimit()}
	if *postHook {
		env.override, env.session = os.Getenv("AGENTSYNC_OWNER"), os.Getenv("CLAUDE_CODE_SESSION_ID")
		gatePost(os.Stdin, os.Stdout, os.Stderr, env)
		return nil
	}
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
			// RunInBackground is `any` so that a value that is not the boolean
			// true (a string, a number) is no finding rather than a payload that
			// cannot be read.
			RunInBackground any `json:"run_in_background"`
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
	// ⛔ A lease wait belongs to the turn that uses the lease. Run in the
	// background it outlives that turn, and is granted to nobody (agentsync#33,
	// occurrence 7). Decided from the command and the payload alone.
	background := p.ToolInput.RunInBackground == true
	rest := invs[:0:0]
	for _, inv := range invs {
		if inv.prog != waitProg {
			rest = append(rest, inv)
			continue
		}
		if background {
			_ = json.NewEncoder(out).Encode(map[string]any{
				"hookSpecificOutput": map[string]any{
					"hookEventName":            "PreToolUse",
					"permissionDecision":       "deny",
					"permissionDecisionReason": backgroundWaitRefusal,
				},
			})
			return
		}
	}
	if invs = rest; len(invs) == 0 {
		return
	}
	env.owner = firstSet(env.override, p.SessionID, env.session)
	if env.owner == "" {
		fmt.Fprintln(errOut, "agentsync gate: cannot tell which session this is; allowing (a refusal is not a verdict)")
		return
	}
	var notice []string
	for _, inv := range invs {
		repo, err := env.target(inv)
		if err != nil {
			fmt.Fprintf(errOut, "agentsync gate: %s: %v; allowing (a refusal is not a verdict)\n", inv.prog, err)
			continue
		}
		if trackerWrite(inv) {
			// A tracker write touches no file of the holder's, so it needs no lease; the
			// holder is only named, so someone can tell them.
			if l := env.holder(repo); l != nil {
				// ⛔⛔ THE LAST ARGUMENT IS THE REPO, NOT THE OWNER. It used to be
				// short(l.Owner), so this notice told every session to run
				// `agentsync tell <session-id> ...` -- and `tell` takes a RESOURCE.
				// Following it opened a queue on a pseudo-resource nobody will ever
				// claim, and the message reached nobody. Measured 2026-11-22: twice
				// in one hour, by a session that was doing exactly what this line
				// said. An instruction a tool gives has to be one the tool accepts,
				// which this repository already learnt from `messages o/r --max-age
				// 0`; this is the same defect in the other direction, and it is why
				// the refusal in tell.go is not enough on its own.
				notice = append(notice, fmt.Sprintf("agentsync gate: %s is claimed by %s until %s; %s writes to its tracker only, which needs no lease. Tell the holder if it matters: agentsync tell %s ...",
					repo, short(l.Owner), l.Expires.Format("15:04:05"), inv.prog, repo))
			}
			continue
		}
		// #30: a request for help or a version, and a dry run, write nothing, so
		// they need no lease. Measured 2026-11-22: `ghrelease --help` was refused
		// in a clone of a repository NOBODY held -- and reading a program's flags
		// is exactly what one does before deciding whether to claim anything.
		//
		// ⛔ AFTER the tracker branch, not before it. Checking this first was the
		// obvious placement and it silenced `ghissue … --dry-run`'s notice naming
		// the holder: that notice is not a refusal, so "needs no lease" is not a
		// reason to skip it. The suite caught it; nothing about the shape of the
		// code did.
		//
		// Before touch(), too: reading a program's flags is not work on the
		// repository, so it must not keep a lease from reading as idle.
		if publishesNothing(inv.args) {
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
		// Allowed: the holder is publishing, which is the activity that keeps its
		// lease from reading as idle.
		env.touch(repo, errOut)
	}
	if len(notice) > 0 {
		for _, n := range notice {
			fmt.Fprintln(errOut, n)
		}
		// Context for the model, with no permission decision: the command goes on
		// through the ordinary permission path.
		_ = json.NewEncoder(out).Encode(map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName":     "PreToolUse",
				"additionalContext": strings.Join(notice, "\n"),
			},
		})
	}
}

// trackerBool and trackerValue are the ONLY ghissue flags that write to the
// tracker without touching the repository's code. Anything else (--edit,
// --delete, --comment, an unknown flag) is not classified, so it is a code write
// and keeps needing the lease: fail closed.
var trackerBool = map[string]bool{"--new": true, "--close": true, "--reopen": true, "--dry-run": true}
var trackerValue = map[string]bool{
	"--title": true, "--body-file": true, "--reason": true, "--token-file": true, "--base": true,
	"--allow-private-path": true, "--parity-waiver": true, "--prior-art": true, "--prior-art-waiver": true,
	"--repo": true, "-repo": true, "-R": true,
}

// trackerWrite reports whether inv writes to the issue tracker only: a new
// issue, a comment, a close or a reopen. The lease protects the holder's CODE
// (branch, pull request, merge, release); an issue touches no file of theirs.
func trackerWrite(inv invocation) bool {
	if inv.prog != "ghissue" {
		return false
	}
	for i := 0; i < len(inv.args); i++ {
		a := inv.args[i]
		switch {
		case trackerBool[a]:
		case trackerValue[a]:
			i++
		case strings.HasPrefix(a, "-"):
			if k := strings.Index(a, "="); k > 0 && trackerValue[a[:k]] {
				continue
			}
			return false
		}
	}
	return true
}

// holder is the live claim of ANOTHER session covering repo, or nil.
func (g gateEnv) holder(repo string) *lease.Lease {
	// Covering answers a list only when it read every lease, and nil with an
	// error for a missing directory, a file where the directory should be and a
	// torn lease alike. Nobody known to hold is the right answer to all three
	// (a refusal is not a verdict), so the error is not asked for twice: a
	// Stat before it and an `if err` after it each survived a mutation sweep,
	// because deleting either left Covering's answer to say the same.
	ls, _ := g.store.Covering(repo)
	now := g.now()
	for i := range ls {
		if !ls[i].Expired(now) && ls[i].Owner != g.owner {
			return &ls[i]
		}
	}
	return nil
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
	mine := false
	for i := range ls {
		l := ls[i]
		switch {
		case l.Expired(now):
			if expired == nil || l.Expires.After(expired.Expires) {
				expired = &ls[i]
			}
		case l.Owner == g.owner:
			mine = true
		case other == nil:
			other = &ls[i]
		}
	}
	// ⛔ ANOTHER SESSION'S LIVE CLAIM DECIDES, whatever else this one holds. A
	// bare "optimize" of mine does not outrank a live "go-numerics/optimize" of
	// theirs: first-match-allows let exactly that through.
	if other == nil && mine {
		return "" // this session holds it and nobody else does
	}
	if other != nil {
		note := ""
		if other.Note != "" {
			note = fmt.Sprintf(" (%q)", other.Note)
		}
		if other.Idle(now, g.idle) {
			return fmt.Sprintf("agentsync: %s is claimed by %s%s, but the holder is idle %dm (limit %s): it is waiting, not working.\n"+
				"Take it: agentsync claim --note \"<what>\" %s -- the holder is told, and then retry.",
				repo, short(other.Owner), note, int(now.Sub(other.LastActive())/time.Minute), g.idle, repo)
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
// The splitting is gitsafe's shellcmd (the reading every guard on this machine
// shares): quoted heredoc bodies are removed and `#` comments dropped, so text
// that merely QUOTES a command is not the command, then Statements splits on
// ; newline && ||.
//
// ⛔ WITHIN a statement it does not try to be a shell. A gated program word
// ANYWHERE in a simple command counts, unquoted -- so `sudo gitpush`,
// `timeout 5 ghmerge`, `xargs ghrun`, `if x; then gitpush`, `! gitpush`,
// `$(ghmerge ..)` and backticks are all found -- because over-refusing a
// command that names one beats missing one that runs it (recur --hook's rule).
// A quoted word is read as a nested command line only after a shell or eval
// (`bash -c "gitpush ..."`), never otherwise: `git commit -m 'ghmerge later'`
// publishes nothing.
//
// `cd X` is followed, and a `cd` inside ( ... ) or $( ... ) is undone at the
// closing parenthesis, as the shell does.
func invocations(cmd, cwd, homeDir string) []invocation {
	cmd = stripComments(shellcmd.StripQuotedHeredocs(cmd))
	dir := cwd
	var stack []string
	var out []invocation
	for _, st := range shellcmd.Statements(cmd) {
		var words []word
		flush := func() {
			var inv []invocation
			inv, dir = scanSimple(words, dir, homeDir)
			out = append(out, inv...)
			words = nil
		}
		for _, t := range tokens(st) {
			if !t.op {
				words = append(words, t)
				continue
			}
			flush()
			switch t.s {
			case "(":
				stack = append(stack, dir)
			case ")":
				if n := len(stack); n > 0 {
					dir, stack = stack[n-1], stack[:n-1]
				}
			}
		}
		flush()
	}
	return out
}

var shells = map[string]bool{"bash": true, "sh": true, "zsh": true, "dash": true, "ksh": true, "eval": true}

var redirectRe = regexp.MustCompile(`^[0-9]*[<>]`)

// scanSimple reads one simple command: it follows a `cd`, and returns the first
// gated program in it, with the words after it.
func scanSimple(ws []word, dir, homeDir string) ([]invocation, string) {
	// Redirections are not arguments; a bare operator takes the next word with it.
	var w []word
	for i := 0; i < len(ws); i++ {
		if !ws[i].quoted && redirectRe.MatchString(ws[i].s) {
			if strings.Trim(ws[i].s, "0123456789<>&|") == "" && !strings.HasSuffix(ws[i].s, "&") {
				i++
			}
			continue
		}
		w = append(w, ws[i])
	}
	var out []invocation
	// A substitution inside double quotes runs, wherever the word sits.
	for _, x := range w {
		for _, sub := range x.subs {
			out = append(out, invocations(sub, dir, homeDir)...)
		}
	}
	i := commandIndex(w)
	if i >= len(w) || w[i].quoted {
		return out, dir
	}
	switch base := filepath.Base(w[i].s); {
	case base == "cd":
		args := w[i+1:]
		for len(args) > 0 && strings.HasPrefix(args[0].s, "-") {
			args = args[1:]
		}
		if len(args) > 0 {
			dir = resolveDir(dir, args[0].s, homeDir)
		}
	case shells[base]:
		// What a shell or eval is handed as one quoted string is a command line.
		for _, x := range w[i+1:] {
			if x.quoted && strings.ContainsAny(x.s, " \t\n;&|") {
				out = append(out, invocations(x.s, dir, homeDir)...)
			}
		}
		if base == "eval" {
			var inv []invocation
			inv, dir = scanSimple(w[i+1:], dir, homeDir)
			out = append(out, inv...)
		}
	case base == "agentsync":
		args := make([]string, 0, len(w)-i-1)
		for _, a := range w[i+1:] {
			args = append(args, a.s)
		}
		if isClaimWait(args) {
			out = append(out, invocation{prog: waitProg, args: args, dir: dir})
		}
	case gated[base]:
		args := make([]string, 0, len(w)-i-1)
		for _, a := range w[i+1:] {
			args = append(args, a.s)
		}
		out = append(out, invocation{prog: base, args: args, dir: dir})
	}
	return out, dir
}

// wrappers run the command that follows them; the gate looks through them.
var wrappers = map[string]bool{
	"sudo": true, "timeout": true, "xargs": true, "nice": true, "nohup": true, "env": true,
	"command": true, "exec": true, "time": true, "builtin": true, "stdbuf": true, "ionice": true,
	// reserved words that precede a command
	"!": true, "if": true, "then": true, "else": true, "elif": true, "do": true,
	"while": true, "until": true, "{": true,
}

// wrapperValueFlags take the next word as their value (sudo -u bob, nice -n 5).
var wrapperValueFlags = map[string]bool{
	"-n": true, "-u": true, "-g": true, "-s": true, "-k": true, "-I": true, "-P": true,
	"-L": true, "-E": true, "-C": true, "-p": true, "-r": true, "-t": true, "-d": true, "-a": true,
}

// commandIndex is the position of the word that is the command: the first one
// after assignments and wrappers (and their options). A gated program counts
// only there -- `go test ./cmd/ghmerge`, `cat ~/.local/bin/gitpush` and
// `echo ghmerge` name one without running it.
func commandIndex(w []word) int {
	i := 0
	for i < len(w) {
		x := w[i]
		if x.quoted {
			return i
		}
		if isAssignment(x.s) {
			i++
			continue
		}
		base := filepath.Base(x.s)
		if !wrappers[base] {
			return i
		}
		i++
		for i < len(w) && !w[i].quoted && strings.HasPrefix(w[i].s, "-") {
			flag := w[i].s
			i++
			if wrapperValueFlags[flag] && i < len(w) {
				i++
			}
		}
		if base == "timeout" && i < len(w) && w[i].s != "" && w[i].s[0] >= '0' && w[i].s[0] <= '9' {
			i++
		}
	}
	return i
}

// stripComments removes `#` comments: a # that starts a word and is not inside
// quotes runs to the end of its line.
//
// Quote state runs across lines: a # inside a multi-line quoted string is text,
// and dropping the rest of its line would drop a command after the closing quote.
func stripComments(cmd string) string {
	var b strings.Builder
	q := byte(0)
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case q != 0:
			if c == '\\' && q == '"' && i+1 < len(cmd) {
				b.WriteByte(c)
				i++
				c = cmd[i]
			} else if c == q {
				q = 0
			}
		case c == '\\' && i+1 < len(cmd):
			b.WriteByte(c)
			i++
			c = cmd[i]
		case c == '\'' || c == '"':
			q = c
		case c == '#' && (i == 0 || cmd[i-1] == ' ' || cmd[i-1] == '\t' || cmd[i-1] == '\n'):
			for i < len(cmd) && cmd[i] != '\n' {
				i++
			}
			if i < len(cmd) {
				c = '\n'
			} else {
				return b.String()
			}
		}
		b.WriteByte(c)
	}
	return b.String()
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

// word is one shell word, or an operator that ends a simple command.
type word struct {
	s      string
	quoted bool // some of it was inside quotes or escaped
	op     bool // | & ( ) or a backtick
	// subs are the bodies of $(...) and `...` found inside double quotes, which
	// the shell runs although the word is quoted.
	subs []string
}

// substitutions returns the command substitutions inside text that sits in
// double quotes.
func substitutions(text string) []string {
	var out []string
	for i := 0; i < len(text); i++ {
		switch {
		case text[i] == '`':
			if j := strings.IndexByte(text[i+1:], '`'); j >= 0 {
				out = append(out, text[i+1:i+1+j])
				i += j + 1
			}
		case text[i] == '$' && i+1 < len(text) && text[i+1] == '(':
			depth := 0
			for j := i + 1; j < len(text); j++ {
				if text[j] == '(' {
					depth++
				} else if text[j] == ')' {
					if depth--; depth == 0 {
						out = append(out, text[i+2:j])
						i = j
						break
					}
				}
			}
		}
	}
	return out
}

// tokens splits one statement into words and the operators the statement
// splitter leaves inside it (pipes, a lone &, parentheses, backticks), honouring
// quotes and backslashes. It is a word splitter, not a shell: what it cannot
// follow it reads as plain words, which the scan above over-matches.
func tokens(s string) []word {
	var out []word
	var tok strings.Builder
	in, quoted := false, false
	var subs []string
	endWord := func() {
		if in {
			out = append(out, word{s: tok.String(), quoted: quoted, subs: subs})
			tok.Reset()
			in, quoted, subs = false, false, nil
		}
	}
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == '\'':
			in, quoted = true, true
			for i++; i < len(rs) && rs[i] != '\''; i++ {
				tok.WriteRune(rs[i])
			}
		case c == '"':
			in, quoted = true, true
			var body strings.Builder
			for i++; i < len(rs) && rs[i] != '"'; i++ {
				if rs[i] == '\\' && i+1 < len(rs) {
					i++
				}
				tok.WriteRune(rs[i])
				body.WriteRune(rs[i])
			}
			subs = append(subs, substitutions(body.String())...)
		case c == '\\' && i+1 < len(rs):
			in, quoted = true, true
			i++
			tok.WriteRune(rs[i])
		case c == ' ' || c == '\t':
			endWord()
		case c == '&' || c == '|' || c == '(' || c == ')' || c == '`':
			endWord()
			out = append(out, word{s: string(c), op: true})
		default:
			in = true
			tok.WriteRune(c)
		}
	}
	endWord()
	return out
}

var repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// valueFlags take the NEXT word as their value, so that word is not a repository.
var valueFlags = map[string]bool{
	"--evidence": true, "--issues-waiver": true, "--token-file": true, "--wait": true,
	"--waive": true, "--title": true, "--body-file": true, "--notes-file": true,
	"--ref": true, "--input": true, "--inputs-file": true, "--confirm": true,
	"--reason": true, "--prior-art": true, "--prior-art-waiver": true, "--allow-private-path": true,
	"--parity-waiver": true, "--base": true, "--backfill-max": true, "-o": true, "--push-option": true, "--receive-pack": true,
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
	push := inv.prog == "gitpush"
	if remote == "" && !push {
		remote = "origin" // the pull request's repository is the fetch side
	}
	if dir == "" {
		return "", errors.New("no repository named and no working directory")
	}
	u, err := g.remote(dir, remote, push)
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

// git runs git in dir and returns its trimmed stdout.
func git(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Output()
	return strings.TrimSpace(string(b)), err
}

// gitConfig is one config value, "" when it is unset (git exits 1 for that).
func gitConfig(dir, key string) string {
	v, _ := git(dir, "config", "--get", key)
	return v
}

// defaultPushRemote is the remote `git push` with no remote argument uses, in
// git's own order: branch.<b>.pushRemote, remote.pushDefault, branch.<b>.remote,
// origin.
func defaultPushRemote(dir string) string {
	b, _ := git(dir, "symbolic-ref", "--short", "-q", "HEAD")
	if b != "" {
		if r := gitConfig(dir, "branch."+b+".pushRemote"); r != "" {
			return r
		}
	}
	if r := gitConfig(dir, "remote.pushDefault"); r != "" {
		return r
	}
	if b != "" {
		if r := gitConfig(dir, "branch."+b+".remote"); r != "" && r != "." {
			return r
		}
	}
	return "origin"
}

// gitRemote reads the URL of a remote of the checkout in dir: the one a push
// goes to when push is set (pushurl honoured), else the fetch URL.
func gitRemote(dir, name string, push bool) (string, error) {
	if name == "" {
		name = defaultPushRemote(dir)
	}
	if strings.HasPrefix(name, "-") {
		return "", errors.New("not a remote name")
	}
	args := []string{"remote", "get-url"}
	if push {
		args = append(args, "--push")
	}
	return git(dir, append(args, name)...)
}
