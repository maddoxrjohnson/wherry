# Why a failed deploy rejects the push

The first real decision in wherry was where a deploy happens relative to `git push`. It looks
like a detail, and it ended up deciding the shape of the whole daemon.

I wanted a broken build to behave like a rejected push: the terminal says why, the branch on the
server does not move, and the old release keeps serving. The place to get that is git's `pre-receive`
hook. It runs after the objects arrive and before any ref moves, and if it exits non-zero, git refuses
the whole push.

So the deploy has to finish inside that hook. Which raises the real question, because a hook is a
process git starts for each push, and everything a deploy needs lives somewhere else.

## The three ways to do it

**Deploy in the hook.** The hook would need a Docker client, the database, the routing table, and
whatever keeps two deploys of one app apart. All of that is state in the daemon. A second process
touching it means either sharing SQLite and a lock file or teaching a short-lived process to reach
into a long-lived one, and the routing table is a pointer in memory, so it cannot be shared at all.

**Accept the push, then deploy.** A `post-receive` hook or a watcher on the ref is the easy design,
and plenty of tools use it. The cost is the feature I wanted: by the time the build fails, the push
has succeeded. The branch on the server now names a commit that never ran, and the only report of
that is wherever the pusher thinks to look.

**Let the hook call back into the daemon.** The hook stays a thin client and the daemon does the
work. This is the one I built. The deploy runs where the state is, and the hook process exists only
to hold the push open and carry output back.

## How it works

The script git runs is two lines that exec `wherryd hook` by absolute path, so the hook is the same
binary as the daemon and cannot drift from its protocol. It reads the updated refs from stdin,
connects to a unix socket in the data directory, sends one JSON line naming the app, the commit, the
quarantine path, and a nonce, and then copies what comes back:

    git push ──ssh──▶ gitserver ──exec──▶ git receive-pack
                                              │ pre-receive
                                              ▼
                                         wherryd hook ──unix socket──▶ daemon: Deploy(app, sha, out)
                                              ▲                                    │
             remote: lines ◀── sideband ◀─────┴─── framed lines and an exit code ◀─┘

Every line the deploy writes goes down the socket, out of the hook's stdout, through git's sideband,
and appears as a `remote:` line. The daemon sends the exit code last, the hook exits with it, and git
either moves the ref or prints `remote rejected`. So the ref moves only if the release went live.

## The parts that were harder than the idea

**The commit does not exist yet.** During `pre-receive`, git keeps the new objects in a quarantine
directory and deletes it if the push is refused. A plain `git archive <sha>` in the repository
cannot see the commit being deployed. The hook forwards `GIT_QUARANTINE_PATH`, and the builder adds
it through `GIT_ALTERNATE_OBJECT_DIRECTORIES`. The daemon treats that path as untrusted, because it
is about to hand it to git, so it must sit inside that app's own `objects/` after cleaning. The check
has to clean the path first because git spells it with a `/./` in the middle.

**Any process running as the daemon's user can dial the socket.** Mode 0600 keeps other users out,
and nothing else. So the SSH server issues a nonce for each `git receive-pack` it admits, bound to that app,
and passes it in git's environment. The daemon redeems it once. It is not an identity, only proof
that this connection is the push the SSH server let in. There is no expiry, because a large pack can
upload for minutes before the hook runs; it is revoked when git exits instead.

**The pusher can vanish.** If the laptop dies during a build, the SSH connection drops, git is
killed, and the ref will never move. A release that went live anyway would disagree with the
repository. My first version cancelled the deploy when the hook's connection closed, and that was
not enough: git's death does not reach the hook, which sits reading the socket and only dies on its
next write. During a silent health wait that write never comes, so the release could still go live.
Rereading this paragraph against the code turned that up. The cancel now hangs off the nonce: when git exits,
the daemon revokes the nonce, and that cancels the deploy tied to it. A short window remains after
the route swaps and before git moves the ref, and closing it would take a two-phase protocol with git.

**A refusal can look like a network error.** Closing a socket with unread bytes queued makes the
kernel send a reset, so the client read "connection reset" where my refusal message should have been.
A failing test found this. The refusal path now drains a bounded amount before closing, which is
what `net/http` does for the same reason.

## What it costs

The push stays open for the whole deploy, so a slow build is a slow `git push`, and a health check
that takes thirty seconds is thirty seconds of waiting. I think that is the right default for a tool
this size, because the terminal is the deploy's UI. A larger system would queue the deploy and hand
back an id, and it would lose the single answer that makes this pleasant: the push succeeded, so it
is live.

There is also a mismatch I accepted. The hook is a yes or no for the whole push, so a push must update
exactly one ref, `main`, and anything else is refused rather than half-deployed. And once a push
succeeds, the daemon owns the release, so a config change or a rollback creates a release without any
push at all. The history of what ran lives in `wherry releases`, not in git, and that is by design.

The decision paid for itself later than I expected. Daemon restarts, cancelled deploys, and rejected
pushes all reduce to one rule: `main` moves only when the commit it would name went live.
