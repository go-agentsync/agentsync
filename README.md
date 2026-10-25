# agentsync — coordinating the sessions that share this machine

On 2026-08-30, `ListAgents` reported **21 peer Claude sessions** working this
project at the same time. Nothing told any of them about the others, and two
failures followed. Both were silent, which is what makes them worth a tool.

**The index lost entries.** `MEMORY.md` is read-modify-written by every session:
read the whole file, insert a line, write it all back. Nothing is locked and
nobody is told. Three times in one evening the same two of my five entries
vanished — always the same two, because they sat between two lines that some
other writer re-emitted from a stale copy. Nothing broke. The memory files
stayed on disk, no link dangled, the file stayed valid Markdown. The entries
simply became unreachable, which is indistinguishable from never having written
them.

**A campaign was interrupted.** A `build.yml` run on `go-pkgx/packages` was
underway when this session dispatched two more onto the same repository, having
found it while sweeping for red branches. It ended well only because the peer
happened to message and ask.

## What it does

    agentsync claim [--ttl 45m] [--note "..."] [--wait 30m [--every 15s]] <resource>...
    agentsync release <resource>...
    agentsync claims [--all]
    agentsync tell <resource> <what the holder needs to know>
    agentsync messages [--max-age 24h] <resource>
    agentsync forget <resource>
    agentsync mem-add [--section "## Heading"] <memory-name> <index-line>
    agentsync mem-verify [--repair=false]
    agentsync orphans
    agentsync mem-fold [--section H] [--hub NAME] [--dry-run]
    agentsync whoami

`tell` is the answer to a refusal, which until 2026-10-11 was a dead end. A
refusal names the holder and prints their note — which tells the refused session
where not to work and gives it nowhere to reply. That day a session was refused
`go-crdt/collab` by a holder whose note said it was about to measure and fix four
reachable `x/net` advisories; that work had landed two days earlier and was
tagged `v0.77.0`. The refused session knew, and could not say so.

So a message is not chat: it is the one channel a lock protocol is missing, from
whoever was turned away back to whoever is holding. `claim` prints what is
waiting on **both** outcomes, and `release` prints it too, that being the last
moment a session will ever look at the resource.

Four properties, each the answer to something that has already gone wrong here:

- **One appended write per message, never read-modify-write.** That is what
  dropped index lines out of a shared `MEMORY.md` when several sessions edited
  it at once, and this file has the same several writers. Twenty concurrent
  `tell`s all arrive.
- **One spelling.** The message file is the lease's own path with another
  suffix, so `o/r`, `https://github.com/o/r` and `git@github.com:o/r` reach one
  conversation rather than three.
- **Stale is dropped and counted.** Older than `--max-age` (a day by default,
  where leases last 45 minutes) is noise about work that has moved — but
  discarding it silently would make a channel that loses things, which is the
  one property a channel must not have.
- **`tell` says who will read it.** A message on an unheld resource still waits
  for the next claimer, and "told nobody" must not read like "told someone".
- **The flag works where your hand puts it.** `claim` and `messages` take their
  options before, after or between the resources, because this program *prints*
  `agentsync messages o/r --max-age 0 for all` and that command used to answer
  `usage:` — Go's flag package stops at the first non-flag argument, so
  `--max-age` and `0` were being read as resources. An instruction a tool gives
  has to be one the tool accepts. ⛔ `tell` and `mem-add` deliberately still
  stop at their first positional argument: their last argument is free text,
  and `mem-add thing "- [thing](thing.md): …"` would otherwise fail on a line
  that begins with a dash. The rule is not style — it is whether an argument
  can look like a flag, and only the command knows.

Reading does not consume them: `forget` is separate, because if reading cleared
them two holders in a row would see different halves of one conversation, and a
session that read them and then died would have consumed them for everybody.

`claims` answers one question — what is taken **right now** — so it shows the
leases that are held and says how many expired ones it left out. On this machine
that was 19 live against 527 expired: a session scanning the list to avoid a
collision read 97% history to find the 3% that could stop it. `--all` brings the
history back, and nothing is ever dropped without being counted.

`claim` says out loud what you are working on. A refusal names the holder and
prints their note, because "it is taken" is not actionable and "held by
d53a55df, *packaging campaign, ~1h*" is. It exits non-zero, so
`agentsync claim X && work` is the whole protocol.

**`--wait` is for the resource somebody is five minutes from finishing with.**
Without it the protocol has no patience: a refusal is final, and the session has
to come back and ask by hand. The gap between *their lease lapsed* and *somebody
noticed* is where two sessions pick up the same item — or where the waiting one
decides to act anyway, which is the collision this tool exists to prevent.

    agentsync claim --wait 30m --note "tag v0.8.2 once their work lands" go-embedded-ruby/ruby

It says who holds it and why **once**, not on every retry, and then one line
when it gets in. If the deadline passes it returns the holder's refusal
unchanged, so a session that waited and still did not get in reads exactly what
it would have read at once.

It takes **one** resource. Waiting for several would acquire them as they free
and block on the rest, leaving the session holding part of what it asked for
while still unable to start — the half-held state `claim X && work` exists to
prevent. `--every 0` is refused too: that is a busy loop, not a wait.

⛔ It waits for a lease to **lapse**, not for the holder to finish. A holder who
renews keeps it, and a holder who dies releases it at the TTL rather than never
— which is why leases expire at all.

**A resource has one name, whatever you type.** `github.com/go-ansible/.github`,
`https://github.com/go-ansible/.github.git`, `git@github.com:go-ansible/.github`
and `go-ansible/.github` are one claim. They were four until 2026-09-06, when a
session holding the first and a session claiming the last were both granted it
and neither was told — the collision this tool exists for, reported by the tool
itself as no conflict, because the strings differ.

**Options go before the resources.** Go's flag package stops parsing at the
first non-flag argument, so `agentsync claim repo --note "why"` used to claim
three things: the repository, the literal `--note`, and the sentence. 25 of the
158 leases in the shared directory arrived that way. They are now named and
dropped — the repository beside them is still claimed, so `claim X && work`
still proceeds.

**A claim covers what is inside it.** Claiming `go-ansible` refuses
`go-ansible/docs` to another session, and the other way round: a sweep over the
organisation reaches the repository somebody is working in. The comparison is by
path segment, so `go-tex` does not enclose `go-texinfo`.

`mem-add` takes a lease on the index, inserts once, and **reads the file back**
to confirm the line survived. Trusting the write would reproduce the bug.

`mem-verify` re-reads every entry this session registered and puts back the ones
that vanished, in the section they came from.

`orphans` reports the memories that NOTHING points at. It is the half
`mem-verify` cannot do: that one repairs what this session wrote, and a memory
written by one session and overwritten by a third belongs to nobody. Sixty-four
of six hundred and seventy-nine were unreachable when this was added, holding
real work that could not be found again.

⛔ It follows links TRANSITIVELY, because the fleet deliberately uses hub files
to keep the index small enough to be read at all — a memory linked from a
memory linked from the index is reachable. Counting only what `MEMORY.md`
mentions reported 351 unreachable when the true number was 64, and would have
sent somebody reorganising five times more than was broken.

The remedy it suggests is a hub rather than sixty-four new index lines: the
index is already past the size at which it is read whole, which is how those
memories were lost in the first place.

`mem-fold` moves one section's rows out of the index and into a hub, which
costs **one** index row for hundreds of memories — the index reaches them in two
hops instead of one, and that is reachability, not loss.

⛔ **The warning was never the missing piece.** The harness prints "MEMORY.md is
34.3KB, only part of it was loaded" at the start of every session, and the index
went 18.2 KB → 36.4 KB in the three days after the last compaction anyway. What
was missing is that ACTING on it meant forty minutes of careful hand-editing.
This is that forty minutes:

    $ agentsync mem-fold --dry-run
    would move 134 rows from "## Atterrissage" into defect-lessons-index.md
    index 36417 → 12159 bytes (read limit 24400)

It writes, then **re-reads both files and counts reachability**, and puts them
back if a single memory stopped being reachable. That post-condition earned its
place on its first run: an early `cut()` returned everything before the folded
section and nothing after, so folding an older block deleted every newer one.
Two memories went unreachable, the fold refused, and both files were restored —
after I had written in the comment above it that a correct fold could not trip
it.

It refuses a hub that does not exist, because a hub says what it collects and an
empty one would leave the rows somewhere with no explanation. It refuses a
section of one row, which trades an index row for an index row. And the quote
block a fold leaves behind is not a row, so running it twice is a no-op rather
than a fold of its own pointer.

## The discipline

1. **Claim before you touch a repository** that another session might be inside
   — anything with a running workflow, a campaign, or an open PR you did not
   open. Add a `--note`; the next session reads it instead of guessing.
2. **Set a TTL you can defend.** The default is 45 minutes. A lease that outlives
   its work blocks a resource; a lease that expires under it invites a
   collision. `claim` again to extend — re-claiming your own is not an error.
3. **`agentsync mem-fold` when the startup warning says the index is truncated.**
   It is one command and it is checked; leaving it undone hides a quarter of the
   index from every session, including yours.
4. **`agentsync mem-verify` before you finish.** Not after writing — at the end.
   The same two entries were lost three times here, and each loss happened
   minutes to hours after a successful write.
5. **If a line keeps vanishing, look at its neighbourhood, not the file.** Three
   of five entries survived every overwrite. Only those inside one contested
   two-line block were lost. Moving them a few lines away fixed it for good.

## Which index it writes to

**The session's, not the working directory's.**

A session's project directory is fixed when it starts. Deriving the index from
`os.Getwd` meant that `cd`ing two levels into a checkout named a *different*
project directory — and Claude creates one per directory it has ever been started
in, so that other directory frequently **exists**, with its own `MEMORY.md`.
`mem-add` would then write to a real file nobody reads, and report success: the
silent split this tool was written to prevent.

Observed 2026-10-07: run from `$HOME`, it aimed at
`~/.claude/projects/-Users-david-delavennat/memory/MEMORY.md`. That one did not
exist, so it failed loudly. From a repository subdirectory it would have
succeeded, into the wrong index.

So the lookup follows the **transcript**, which is written at
`<projects>/<slug>/<session id>.jsonl`: the directory holding it *is* this
session's project directory, whatever the working directory is now. No slug
arithmetic, no guess. Two matches are **refused** rather than ranked, because a
session id is unique and picking one would mean writing to an index chosen by
directory ordering.

Outside a Claude session — a person at a shell — there is no transcript to
follow, and the working directory is the best there is. `--index` overrides
everything, and `AGENTSYNC_MEMORY` overrides the lookup.

## What it is not

Advisory. Nothing enforces a lease at the filesystem level: it coordinates
sessions that ask, and cannot stop one that does not. That is precisely why
`mem-verify` repairs rather than trusts, and why a lease expires rather than
waiting for a release that a dead session will never send.

## Layout

    lease/          the expiring advisory lock, with its tests
    cmd/agentsync/  the CLI

    go test ./...
    go build -o ~/.local/bin/agentsync ./cmd/agentsync
