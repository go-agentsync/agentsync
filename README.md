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

    agentsync claim [--ttl 45m] [--note "..."] <resource>...
    agentsync release <resource>...
    agentsync claims
    agentsync mem-add [--section "## Heading"] <memory-name> <index-line>
    agentsync mem-verify [--repair=false]
    agentsync whoami

`claim` says out loud what you are working on. A refusal names the holder and
prints their note, because "it is taken" is not actionable and "held by
d53a55df, *packaging campaign, ~1h*" is. It exits non-zero, so
`agentsync claim X && work` is the whole protocol.

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

## The discipline

1. **Claim before you touch a repository** that another session might be inside
   — anything with a running workflow, a campaign, or an open PR you did not
   open. Add a `--note`; the next session reads it instead of guessing.
2. **Set a TTL you can defend.** The default is 45 minutes. A lease that outlives
   its work blocks a resource; a lease that expires under it invites a
   collision. `claim` again to extend — re-claiming your own is not an error.
3. **`agentsync mem-verify` before you finish.** Not after writing — at the end.
   The same two entries were lost three times here, and each loss happened
   minutes to hours after a successful write.
4. **If a line keeps vanishing, look at its neighbourhood, not the file.** Three
   of five entries survived every overwrite. Only those inside one contested
   two-line block were lost. Moving them a few lines away fixed it for good.

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
