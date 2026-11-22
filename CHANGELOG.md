# Changelog

## Unreleased

- **Every block of `cmd/agentsync` and `lease` is entered by a test, and CI
  refuses a change that leaves one out** (#43). Measured before: 80.1% of the
  command's statements, 84.6% of both packages, 174 blocks no test entered.
  After: 0. The gate counts blocks (`covblocks --max 0`), not a percentage,
  because `go tool cover -func` rounds and a gate written against it passes at
  99.95%.
- **A bad flag is an exit status the tests can reach.** Every command parsed its
  flags with `flag.ExitOnError`, which calls `os.Exit` inside the command, so no
  test could ever see it. `newFlags`/`parseFlags` return an `exitError` instead
  (2 for a bad flag, 0 for `-h`: what `ExitOnError` did) and `run()` turns it
  into the status, leaving `main()` one line. No behaviour changes.
- `gate --hook` refuses `agentsync claim --wait` when the Bash call has
  `run_in_background: true`: the wait outlives its turn and the lease is held by
  nobody (lesson recurred 2026-11-22).
- `gate --post` releases only when the payload shows a zero exit (fail closed):
  PostToolUseFailure, `error`, `is_error`, a missing or string `tool_response`,
  `interrupted` and a non-zero `exit_code`/`exitCode` release nothing. The
  payload documents no exit status, so a failure delivered as a success-shaped
  object with none of those fields still releases.
- **A test can no longer reach the shared claims directory.** `root()` panics
  under `go test` when `AGENTSYNC_DIR` is unset, instead of returning the path
  where every live session's leases are. It found two writers on its first run:
  the fold tests, which sandboxed the memory index and then took real leases
  through `withIndexLease`, and — in the shared store itself — `o/r` held by
  `11111111`, a test's fixture owner, written by a clone older than 2db6442.
  `go build` keeps the default path; the binary grows 16,848 bytes (+0.34%).
- **`.`, `..` and a path are no longer claimed as resources.** `claim .` took a
  lease on the literal string `.`, which every session's working directory maps
  onto: one global lock that two sessions in unrelated checkouts both hold while
  believing they have coordinated, and that tells a third nothing. Measured
  2026-11-22, both live the same day: `.` held until 20:02, and `..` written at
  14:54. Dropped and named like the other non-resources, so the repository
  beside the mistake is still claimed, and the reason says to name the
  repository as `git remote -v` spells it. A name without a slash stays valid —
  worktree topics are claimed that way here.
- `tell` refuses a resource that is a **session id** — `short()`'s eight hex
  characters, or a raw UUID — because a message left on one opens a queue nobody
  will ever claim. The refusal names what that session holds, so the message has
  somewhere to go. Measured 2026-11-22: twice in one hour, by one session, and
  both times the output said in full "NOBODY holds it right now". A true line is
  not a gate. A resource the store has already seen is never refused, so a
  repository whose name happens to be hex keeps working.
- `gate --hook`'s tracker notice told every session to run the command the point
  above now refuses: its `agentsync tell` argument was `short(l.Owner)`, a
  **session id**, where `tell` takes a resource. It names the repository now.
  This is the cause, not a second symptom — the session that lost two handovers
  was doing exactly what the notice said. A test takes the command out of the
  notice and hands it to `tell`, so the two sides cannot drift apart again.

## v0.11.1

- `gate --hook` no longer refuses a command that publishes nothing: `-h`,
  `--help`, a leading `help`, `--version` and `--dry-run` need no claim at all.
  Measured 2026-11-22, `ghrelease --help` was refused in a clone of a repository
  nobody held — and reading a program's flags is what one does before deciding
  whether to claim anything. (#30)
- The predicate both gates now share is stricter than the one `gate --post`
  shipped with, because calling it from the pre-tool gate removed its safe
  direction: a bare `help` counts only as the first argument (`--title help` is a
  value), and `--dry-run=false` is a publish rather than a dry run. An
  unparseable value counts as publishing.
- `gate --post` no longer releases a lease for an invocation that cannot have
  published anything: `-h`, `--help`, `help` and `--version` join `--dry-run`.
  Measured 2026-11-22, a plain `ghrelease -h` released a lease on a repository
  whose work was unfinished; help exits 0 and writes no release, so the exit
  status could never have told them apart.
- The claim that Claude Code runs PostToolUse only for a command that succeeded
  is removed from the code comment and the README, because it is false: a `Bash`
  call running a program that exits non-zero is a tool call that worked. A
  `ghmerge` refused with a 403 released the lease on a merge that never happened.

## v0.11.0

- A succeeded `ghmerge`/`ghrelease` releases the holder's lease on that repository
  (`agentsync gate --post`, a PostToolUse hook), unless it was claimed with
  `--keep-lease "<why>"`; the reason is stored and shown in `claims`. (#33)
- A lease idle for 15 minutes (`AGENTSYNC_IDLE`, `0` disables) may be taken by
  another session; `claims` shows `idle N`, the take is logged in `takes.log` and
  the previous holder is left a message. A gated command the holder is allowed to
  run counts as activity. (#33)
- `claim --wait` is dropped when its session's transcript has been silent for
  12 minutes (`AGENTSYNC_WAIT_GRACE`). (#33)
- Take, renew, touch and release of one lease are serialised by a per-lease lock,
  so a holder renewing cannot lose its lease to a taker reading the old one.

## v0.9.0

- `mem-add` refuses (exit 2) an index line that does not link `<memory-name>.md`,
  before writing anything. `<memory-name>` may carry `.md`. (#27)
- `mem-add <memory-name>` alone builds the line from the memory's frontmatter
  (`name`, `description`); a missing file or description is refused (exit 2).
- `mem-verify` no longer answers `ok` for text that names a memory without
  linking it: reachability is the `orphans` walk. A legacy record whose line has
  no link is reported `MISSING` and not re-inserted. (#27)
