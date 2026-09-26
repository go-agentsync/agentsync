package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// readLimit is the reader's, not ours: the harness loads MEMORY.md up to about
// this many bytes and CUTS THE REST, saying so in a warning nobody acts on.
//
// ⛔ The warning is not the missing piece. It has been printed at the start of
// every session since 2026-09-23, and the index went from 18.2 KB to 36.4 KB
// anyway — I read that warning myself and worked on something else for five
// hours. What was missing is that ACTING on it cost forty minutes of careful
// hand-editing. This command is that forty minutes.
const readLimit = 24400

// memFold moves one section's rows out of the index and into a hub file.
//
// A hub costs ONE index row for hundreds of memories, and the index reaches
// them in two hops instead of one — which is reachability, not loss. See the
// comment on reachableFrom: the repair used to disagree about exactly this.
func memFold(args []string) error {
	fs := flag.NewFlagSet("mem-fold", flag.ExitOnError)
	section := fs.String("section", "", "heading whose rows to fold; default is the LAST section, where mem-add appends")
	hubName := fs.String("hub", "defect-lessons-index", "hub file the rows move into")
	dry := fs.Bool("dry-run", false, "say what would move, change nothing")
	idx := fs.String("index", memoryIndex(), "path to MEMORY.md")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: agentsync mem-fold [--section H] [--hub NAME] [--dry-run]")
	}
	if *idx == "" {
		return errors.New("agentsync: no memory index for this directory")
	}
	dir := filepath.Dir(*idx)
	hubFile := strings.TrimSuffix(*hubName, ".md") + ".md"
	hubPath := filepath.Join(dir, hubFile)
	if _, err := os.Stat(hubPath); err != nil {
		return fmt.Errorf("hub %s: %w — a hub must already exist and say what it collects", hubFile, err)
	}

	before, err := os.ReadFile(*idx)
	if err != nil {
		return err
	}
	head, rows, heading, tail, err := cut(string(before), *section)
	if err != nil {
		return err
	}
	if len(rows) < 2 {
		return fmt.Errorf("%q holds %d row(s): folding one row costs an index row for an index row", heading, len(rows))
	}

	all, err := memoriesIn(dir)
	if err != nil {
		return err
	}
	was := reachableFrom(*idx, dir, all)

	newIndex := head + "\n" + heading + "\n\n" +
		fmt.Sprintf("> Replié le %s dans [%s](%s) — un hub coûte UNE ligne pour\n",
			time.Now().Format("2006-01-02"), strings.TrimSuffix(hubFile, ".md"), hubFile) +
		"> des centaines de mémoires, et l'index les atteint en deux sauts. Le contrôle\n" +
		"> est `agentsync orphans`, pas la taille.\n" + tail

	oldHub, err := os.ReadFile(hubPath)
	if err != nil {
		return err
	}
	newHub := strings.TrimRight(string(oldHub), "\n") + fmt.Sprintf(
		"\n\n## %s, replié de MEMORY.md le %s\n\n"+
			"Ces %d lignes s'étaient accumulées dans l'index, où chaque session ajoute la\n"+
			"sienne. Rien n'a été perdu : chacune est recopiée telle quelle.\n\n",
		strings.TrimLeft(heading, "# "), time.Now().Format("2006-01-02"), len(rows)) +
		strings.Join(rows, "\n") + "\n"

	if *dry {
		fmt.Printf("would move %d rows from %q into %s\n", len(rows), heading, hubFile)
		fmt.Printf("index %d → %d bytes (read limit %d)\n", len(before), len(newIndex), readLimit)
		return nil
	}

	// ⛔ WRITE, THEN RE-READ AND COUNT. A fold that drops a memory is the one
	// failure this must never have, and it is invisible in the diff: the rows
	// are all there, in a file nothing points at. So the check is reachability
	// measured from disk afterwards, and anything lost puts both files back.
	//
	// I WROTE HERE THAT A CORRECT FOLD COULD NOT TRIP IT, and it tripped on the
	// first run: cut() returned everything BEFORE the folded section and
	// nothing after, so folding an older block deleted every newer one. Two
	// memories stopped being reachable, this refused, and both files went back.
	// The argument that a post-condition is unreachable is exactly the argument
	// for keeping it.
	if err := os.WriteFile(hubPath, []byte(newHub), 0o644); err != nil {
		return err
	}
	restore := func() {
		_ = os.WriteFile(hubPath, oldHub, 0o644)
		_ = os.WriteFile(*idx, before, 0o644)
	}
	if err := withIndexLease(func() error { return os.WriteFile(*idx, []byte(newIndex), 0o644) }); err != nil {
		restore()
		return err
	}
	now := reachableFrom(*idx, dir, all)
	lost := lostBy(was, now)
	if len(lost) > 0 {
		restore()
		return fmt.Errorf("refusing: %d memories would stop being reachable (%s); nothing was changed",
			len(lost), strings.Join(lost, " "))
	}

	after, _ := os.ReadFile(*idx)
	fmt.Printf("moved %d rows from %q into %s\n", len(rows), heading, hubFile)
	fmt.Printf("index %d → %d bytes, read limit %d: %s\n", len(before), len(after), readLimit,
		map[bool]string{true: "under", false: "STILL OVER"}[len(after) <= readLimit])
	fmt.Printf("%d memories reachable, unchanged\n", len(now))
	return nil
}

// cut splits the index into everything before a section, that section's rows,
// and its heading. With no name it takes the LAST section, which is where
// mem-add appends and therefore where the growth is.
func cut(body, section string) (head string, rows []string, heading, tail string, err error) {
	lines := strings.Split(body, "\n")
	at := -1
	for i, l := range lines {
		if !strings.HasPrefix(l, "## ") {
			continue
		}
		if section == "" || strings.HasPrefix(l, section) || strings.Contains(l, section) {
			at = i
			if section != "" {
				break
			}
		}
	}
	if at < 0 {
		return "", nil, "", "", fmt.Errorf("no section matching %q", section)
	}
	heading = lines[at]
	end := len(lines)
	for i := at + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") {
			end = i
			break
		}
	}
	for _, l := range lines[at+1 : end] {
		// A quote block is the pointer a previous fold left; it is not a row.
		if strings.TrimSpace(l) == "" || strings.HasPrefix(strings.TrimSpace(l), ">") {
			continue
		}
		rows = append(rows, l)
	}
	// ⛔ AND EVERYTHING AFTER IT. Returning only the head silently dropped every
	// later section: asking to fold an OLDER block deleted the newer ones.
	// The post-condition caught it on the first run — after I had written, in
	// the comment above it, that a correct fold could not trip it.
	if end < len(lines) {
		tail = "\n" + strings.Join(lines[end:], "\n")
	}
	return strings.TrimRight(strings.Join(lines[:at], "\n"), "\n"), rows, heading, tail, nil
}

// lostBy names what was reachable and is not any more.
//
// It is a function of its own so it can be tested: the fold cannot make it
// answer anything but "nothing", and a comparison that is never exercised is
// one that quietly stops comparing.
func lostBy(was, now map[string]bool) []string {
	var lost []string
	for n := range was {
		if !now[n] {
			lost = append(lost, strings.TrimSuffix(n, ".md"))
		}
	}
	sort.Strings(lost)
	return lost
}
