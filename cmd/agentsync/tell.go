package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// tell leaves a message about a resource for whoever holds it.
//
// It closes the one gap the lock protocol had. A refusal names the holder and
// prints their note -- which tells the refused session where not to work, and
// gives it nowhere to answer. On 2026-10-11 a session was refused
// go-crdt/collab by a holder whose note said it was about to measure and fix
// four reachable x/net advisories; that work had landed two days earlier and
// was already tagged v0.77.0. The refused session knew. It had no way to say so.
//
// It REPORTS WHO WILL READ IT, which is the part that keeps it honest: a message
// left on a resource nobody holds is still worth leaving -- the next claimer
// sees it -- but "told nobody" must not look like "told someone".
// flagToken is a bare flag such as -f or --file: a dash, then a letter, then
// nothing but word characters. It deliberately does NOT match text that merely
// begins with a dash, because any whitespace disqualifies it.
var flagToken = regexp.MustCompile(`^--?[A-Za-z][A-Za-z0-9._-]*$`)

// tellUsage names every accepted form, because the refusal below points at it
// and a refusal that does not name the working form is half a refusal.
const tellUsage = `usage: agentsync tell [-f file] <resource> <what the holder needs to know>
       agentsync tell <resource> -- <text that begins with a dash>`

func tell(args []string) error {
	fs := flag.NewFlagSet("tell", flag.ExitOnError)
	// ⛔ -f EXISTS BECAUSE TWO SESSIONS REACHED FOR IT ON CONSECUTIVE DAYS and
	// lost their handovers to it. `gh` has --body-file, `git commit` has -F, and
	// this machine's own rules say to pass long text through a file; -f is the
	// obvious generalisation and nothing contradicted it.
	//
	// It is parsed BEFORE the resource, which is the only place flags can live
	// here: the long comment on parseAnywhere below explains why `tell` must
	// stop parsing at the first positional argument -- its last argument is free
	// text, and an index line beginning with "- [thing]" would otherwise be read
	// as a flag.
	file := fs.String("f", "", "read the message from this file instead of the command line")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) < 1 {
		return errors.New(tellUsage)
	}
	resource, words := rest[0], rest[1:]

	// `--` says the rest is literal, which is how a message that genuinely
	// begins with a dash gets through the refusal below.
	literal := false
	if len(words) > 0 && words[0] == "--" {
		literal, words = true, words[1:]
	}
	text := strings.Join(words, " ")

	if *file != "" {
		if text != "" {
			return errors.New("-f and inline text are two ways to say the same thing: pass one")
		}
		b, err := os.ReadFile(*file)
		if err != nil {
			return fmt.Errorf("reading the message from %s: %w", *file, err)
		}
		text = strings.TrimRight(string(b), "\n")
	}
	if text == "" {
		return errors.New(tellUsage)
	}

	// ⛔⛔ A FLAG IN THE MESSAGE POSITION IS A MISTAKE, NEVER A MESSAGE. Before
	// this refusal, `agentsync tell <repo> -f note.txt` stored the literal five
	// characters "-f" plus a path into the SENDER's volatile scratchpad, printed
	// the ordinary confirmation and exited 0. Two sessions did it on consecutive
	// days -- 2026-11-06 and 2026-11-07 -- and both handovers were lost in
	// silence, because exit 0 meant "something was stored", never "your message
	// was stored". On a machine where two dozen sessions coordinate only through
	// these queues, a silently dropped handover is the expensive kind: the
	// sender believes it landed and the receiver never learns there was anything
	// to receive.
	// ⚠ IT TESTS FOR A FLAG TOKEN, NOT FOR A LEADING DASH, and the difference is
	// the whole correctness of this check. Free text here routinely begins with
	// a dash -- "- [thing](thing.md): what it is" is the normal shape of an
	// index line, and TestFreeTextArgumentsMayBeginWithADash pins it precisely
	// so that a tidy-up cannot take it away. A dash followed by a space is
	// prose; a bare `-f` or `--file` is not.
	if !literal && len(words) > 0 && flagToken.MatchString(words[0]) {
		return fmt.Errorf("%q is a flag, and flags do not go after the resource: "+
			"a message is being read where one was meant to be sent.\n%s",
			words[0], tellUsage)
	}
	// A heuristic, so it warns rather than refuses: a message that is exactly a
	// path to a file that exists is almost never what anyone meant, but it is
	// not malformed the way a flag is, and refusing a valid message is its own
	// cost.
	if st, err := os.Stat(text); err == nil && !st.IsDir() {
		fmt.Fprintf(os.Stderr, "⚠ the message is the path of a file that exists; "+
			"did you mean `-f %s`? Sending it as text.\n", text)
	}

	s := store()
	if _, err := s.Tell(resource, owner(), text); err != nil {
		return err
	}
	// ⭐ SAY WHAT WAS STORED, not only that something was. `tell` used to report
	// delivery without echoing the content, so a message that had become "-f
	// /tmp/..." looked exactly like one that had not. One truncated line makes
	// every mistake of that family visible at the moment it is made.
	fmt.Printf("stored   %s\n", oneLine(text))

	// Who it reached, said plainly either way.
	ls, err := s.List()
	if err != nil {
		fmt.Printf("told     %s (who holds it could not be read: %v)\n", resource, err)
		return nil
	}
	now := time.Now()
	for _, l := range ls {
		if l.Resource == resource && !l.Expired(now) {
			fmt.Printf("told     %s — %s holds it until %s and will see this on their next claim or release\n",
				resource, short(l.Owner), l.Expires.Format("15:04:05"))
			return nil
		}
	}
	fmt.Printf("told     %s — NOBODY holds it right now, so this waits for whoever claims it next\n", resource)
	return nil
}

// parseAnywhere parses flags that appear BEFORE, AFTER or BETWEEN the
// positional arguments, and returns the positional ones in order.
//
// ⛔ Go's flag package stops at the first non-flag argument, so
// `messages o/r --max-age 0` leaves `--max-age` and `0` as positional
// arguments and the command answers "usage:". That is not a theory: this
// program PRINTS that exact command. A lease refusal ends with
//
//	(3 older message(s) not shown; agentsync messages o/r --max-age 0 for all)
//
// and running it as printed fails. An instruction a tool gives has to be one
// the tool accepts, and the cheaper of the two repairs -- reordering the hint
// -- would leave every person who types the flag last in the same place.
//
// The loop is the idiom from the flag package's own documentation: parse, take
// the positional argument that stopped it, and parse the rest. Parse keeps the
// pairing of a flag with its value, which a partition by "starts with -" would
// get wrong for exactly the `--max-age 0` that started this.
//
// ⛔ IT IS WRONG FOR `tell` AND `mem-add`, and they do not use it. Their last
// argument is free text, and continuing to parse after a positional argument
// makes any text beginning with a dash a flag error:
//
//	agentsync mem-add thing "- [thing](thing.md): what it is"
//
// is the normal shape of an index line, and `- [thing]…` would be read as a
// flag named " [thing]…". Stopping at the first positional argument is
// exactly right there. The difference is not style: it is whether an argument
// can look like a flag, and only the command knows.
func parseAnywhere(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// messages prints what has been left about a resource, without claiming it.
//
// Separate from claim because the question "is anything waiting on X" should not
// require taking X: a session deciding where to work asks it of several
// resources it has no intention of holding.
func messages(args []string) error {
	fs := flag.NewFlagSet("messages", flag.ExitOnError)
	maxAge := fs.Duration("max-age", messageWindow, "hide messages older than this; 0 shows everything")
	rest, err := parseAnywhere(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return errors.New("usage: agentsync messages [--max-age d] <resource>")
	}
	resource := rest[0]

	live, dropped, err := store().Messages(resource, *maxAge)
	if err != nil {
		return err
	}
	if len(live) == 0 && dropped == 0 {
		fmt.Printf("nothing has been said about %s\n", resource)
		return nil
	}
	for _, m := range live {
		fmt.Printf("%s  %s  %s\n", m.At.Format("2006-01-02 15:04:05"), short(m.From), oneLine(m.Text))
	}
	if dropped > 0 {
		fmt.Fprintf(os.Stderr, "%d older or unreadable message(s) not shown; --max-age 0 for all\n", dropped)
	}
	return nil
}

// forget drops every message about a resource, which is what a holder does once
// it has acted on them.
//
// Not done by reading, deliberately: if reading cleared them, two holders in a
// row would see different halves of one conversation, and a session that read
// them and then died would have consumed them for everybody.
func forget(args []string) error {
	fs := flag.NewFlagSet("forget", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: agentsync forget <resource>")
	}
	resource := fs.Arg(0)
	live, _, err := store().Messages(resource, 0)
	if err != nil {
		return err
	}
	if err := store().Forget(resource); err != nil {
		return err
	}
	fmt.Printf("forgot   %d message(s) about %s\n", len(live), resource)
	return nil
}
