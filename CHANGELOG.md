# Changelog

## Unreleased

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
  Reading the payload's `tool_response` is still to do.

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
