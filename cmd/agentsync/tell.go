package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
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
func tell(args []string) error {
	fs := flag.NewFlagSet("tell", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return errors.New(`usage: agentsync tell <resource> <what the holder needs to know>`)
	}
	resource := fs.Arg(0)
	text := strings.Join(fs.Args()[1:], " ")

	s := store()
	if _, err := s.Tell(resource, owner(), text); err != nil {
		return err
	}

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

// messages prints what has been left about a resource, without claiming it.
//
// Separate from claim because the question "is anything waiting on X" should not
// require taking X: a session deciding where to work asks it of several
// resources it has no intention of holding.
func messages(args []string) error {
	fs := flag.NewFlagSet("messages", flag.ExitOnError)
	maxAge := fs.Duration("max-age", messageWindow, "hide messages older than this; 0 shows everything")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: agentsync messages [--max-age d] <resource>")
	}
	resource := fs.Arg(0)

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
