# wherry

[![ci](https://github.com/maddoxrjohnson/wherry/actions/workflows/ci.yml/badge.svg)](https://github.com/maddoxrjohnson/wherry/actions/workflows/ci.yml)

A small self-hosted platform I am building so that `git push` is the whole deploy: the
server receives the push, builds an image, starts the container, routes traffic to it by
hostname, and swaps releases without dropping requests. One Go binary, SQLite for state,
Docker underneath, and later my own container runtime as a second backend.

Work in progress. Today a push deploys. The daemon accepts `git push` over SSH with public
keys from its own key table, keeps one bare repository per app, and runs nothing except
`git receive-pack` and `git upload-pack`. A pre-receive hook hands each push to the daemon
over a unix socket, proving it belongs to that push with a one-time nonce. The daemon builds
the commit into an image through the Docker Engine API, records a numbered release, starts
the image as a container published on 127.0.0.1 only (memory, CPU, and process limits,
`no-new-privileges`), waits until the app holds a TCP connection on its port, points the
reverse proxy at it, stops the previous container, and marks the release live. Every line of
that streams back to the pusher's terminal; a build that fails or an app that never listens
rejects the push and the previous release keeps serving. The proxy routes by hostname from
a copy-on-write table, so requests never take a lock, and every response carries an
`X-Request-Id` that also appears in the access log. Config vars are per-app environment
variables: each release snapshots them when it is created, and changing one makes a new
release of the same image and redeploys it. A deploy drops no requests: the new instance
gets traffic only after its health check passes, the route moves in one atomic swap, and
the old instance keeps running for a drain period before it is stopped. An instance that
exits on its own is restarted with exponential backoff (1, 2, 4, up to 60 seconds) and
given up on after ten failures in ten minutes, which `ps` reports. The `wherry` command
talks to the daemon's API: apps, config, releases, rollback, restart, `ps`, keys. A daemon
restart changes nothing for the apps: containers keep serving while it is down, and on start
it reconciles what is running with what should be before it opens a single port. Every
line an instance prints is collected once, stamped, appended to a per-app log file, and
kept in memory for the readers that `logs -f` and the dashboard add next.

## Zero-downtime deploys, measured

`examples/slowstart` sleeps ten seconds before it listens and shuts down cleanly on SIGTERM.
With `scripts/loadloop` sending 200 requests a second through the router and
`scripts/deploys.sh` pushing ten deploys back to back:

    requests=24747 ok=24747 failed=0 (none) distinct bodies=11 p50=730µs p99=1.85ms max=15.7ms

Zero failed requests in 24,747 across ten deploys (measured 2026-10-12 on the Docker
runtime in WSL2). Eleven distinct bodies means every release answered during the run, so
the swaps happened; the daemon log shows eleven route swaps and ten old instances stopped
after the five-second drain. To reproduce:

    ./bin/wherry apps:create slowstart
    # push examples/slowstart once, then, with the daemon started with
    # WHERRY_RATE_LIMIT_RPS=0 (the load loop is one client sending 200 req/s):
    go run ./scripts/loadloop -host slowstart.localhost -rate 200 &
    sh scripts/deploys.sh /path/to/slowstart-checkout 10
    kill -INT %1; wait

`WHERRY_DRAIN_GRACE` (default `5s`) is how long a replaced instance keeps running after the
route moved away from it. The app's own `[process] grace` is the SIGTERM-to-SIGKILL window
once the stop begins.

## Crashes

A watcher per instance waits for its process to exit. An exit the daemon did not ask for
schedules a restart of the same release after 1 s, then 2, 4, 8, 16, 32, and 60 s for later
failures within a ten-minute window; the tenth failure in the window marks the app
`crashed` and nothing more is tried until a restart is asked for. The route stays where it
was, so requests to a crashed app get a 502 with a request id rather than a 404.
`examples/crasher` exits two seconds after it starts when `CRASH_AFTER=2s` is set, and
`wherry ps crasher` shows the state, the restart count, and when the next attempt is due.
`wherry restart crasher` ends a crash loop once its cause is fixed.

An optional `wherry.toml` at the root of the repository tunes the deploy. Every value is
optional; this is the full set with the defaults:

    [health]
    path = "/healthz"     # GET must return 2xx; without it, the check is a held TCP connection
    timeout = "30s"       # how long a new instance has to pass the check
    interval = "250ms"    # how often to probe

    [process]
    grace = "10s"         # SIGTERM to SIGKILL when an instance is stopped
    memory = "256m"       # memory limit (k, m, g, or bytes)
    cpu = "1.0"           # CPU quota

The effective values are stored with each release, so a rollback or a config change runs
with the settings of the release it reuses. A file that does not parse, or names a key the
daemon does not know, rejects the push with the error as the last line.

## Deploying an app locally

With Docker running and the daemon started (`make run`, text logs on stderr), log the CLI
in with the token the daemon wrote on its first start, then create an app and push:

    ./bin/wherry login http://127.0.0.1:9000 $(cat data/admin.token)
    ./bin/wherry apps:create hello            # app row, bare repo, prints the remote
    ./bin/wherry keys:add me ~/.ssh/id_ed25519.pub
    cd examples/hello-go
    git init -b main && git add -A && git commit -m "first"
    git remote add wherry ssh://wherry@localhost:2222/hello
    git push wherry main
    curl http://hello.localhost:8080/

The push prints `release v1`, the build steps, and `live at http://hello.localhost:8080`.
With a `wherry.toml` in the repository the push also prints the settings it read.
Change the string in `main.go`, commit, push again, and curl shows the new one. Break the
`Dockerfile`, push, and the push is rejected while the old version keeps serving.
`wherry releases hello` shows the history, the rejected push included as a `failed`
release; `wherry releases:info hello v2` shows one release's image, commit, config, and
manifest.

Config vars are set through the daemon, which redeploys the live release with the new
environment and streams the deploy to your terminal:

    ./bin/wherry config:set hello GREETING=hi       # release v3, same image, new env
    ./bin/wherry config:get hello                   # GREETING=hi
    ./bin/wherry config:unset hello GREETING        # release v4

`examples/config-reader` answers every request with `$GREETING`, so a curl after each
command shows the change. The daemon sets `PORT` itself and refuses it as a config var.
Values are never written to the daemon's log; `releases:info` shows the snapshot a release
ran with.

A rollback is a new release made from an old one's image, config, and manifest, so the
history never rewrites itself; a restart replaces the live release's instance and clears a
crash loop:

    ./bin/wherry releases:rollback hello v2         # release v5 (rollback v2)
    ./bin/wherry restart hello
    ./bin/wherry ps hello                           # instance, state, restarts, uptime
    ./bin/wherry apps:destroy hello                 # stops it, removes rows and repository

Every list and show command takes `--json`. `wherry login` stores the daemon's address and
token in `$XDG_CONFIG_HOME/wherry/config` (or `WHERRY_CONFIG`); `WHERRY_URL` and
`WHERRY_TOKEN` override it, which is how scripts and CI use the CLI.

The daemon's API listens on `WHERRY_INTERNAL_ADDR` (default `127.0.0.1:9000`) and requires
the admin token it writes to `data/admin.token` on first start, as a bearer token. It is
never exposed on the router's port; from another machine, reach it through an SSH tunnel.

The build context is `git archive` of the pushed commit, so only tracked files reach the
build and `.dockerignore` has no effect. To keep tracked files out of the image, mark them
`export-ignore` in `.gitattributes`.

## Logs

A collector goroutine per instance reads the container's output from the moment it
starts, splits it into lines at newlines (chunks from Docker end wherever the read did,
so a partial line waits for the rest), stamps each with the app, release, instance, stream,
and the time it arrived, and appends it to `data/logs/<app>/current.log` (rotated at
50 MB, five kept) and to a ring of the app's last 1000 lines in memory. Lines printed while
a release was still failing its health check are on record too. Readers subscribe to the
ring: each gets a bounded channel, sends to it never block, and a reader that falls behind
loses lines and is then handed one line saying how many, so a stalled tab can never slow
the app or the collector.

    ./bin/wherry logs chatty              # the last 100 lines
    ./bin/wherry logs -f -n 10 chatty     # ten lines, then follow until Ctrl-C
    ./bin/wherry logs --json chatty       # one JSON object per line

`examples/chatty` prints a line a second, so `logs -f` has something to show. The API
serves the same stream as server-sent events at `GET /api/v1/apps/<app>/logs?tail=N&follow=1`,
one `line` event per line with the line as JSON, and a `done` event when the app is
destroyed; the dashboard reads it with `EventSource`.

## HTTPS and the edge

Set `WHERRY_HTTPS_ADDR` (`:443` in production) and `WHERRY_ACME_EMAIL`, and the router
serves TLS with certificates from Let's Encrypt: the first request for an app's hostname
triggers an HTTP-01 challenge, which the plain-HTTP listener answers, and the certificate
is cached under `data/certs`. Only hostnames the router routes can get a certificate, so a
client that connects by IP and names a host of its choosing cannot spend the issuer's rate
limit. With TLS on, every other plain-HTTP request is redirected to `https://` with a 308.
Locally neither variable is set and everything stays plain HTTP.

Every client IP, or /64 for IPv6, is limited to `WHERRY_RATE_LIMIT_RPS` requests a second
(default 100, burst twice that, `0` disables) by a token bucket, before the route lookup, so
a flood of requests for unknown hosts is refused as cheaply as one for a real app; refused
requests get a 429 with `Retry-After`. Request bodies are capped at 32 MB, the listeners have
header and idle timeouts, and the client's own `X-Forwarded-*` headers are dropped before
the router sets its own. Streaming responses are flushed as they arrive and WebSocket
upgrades pass through, so an app can serve server-sent events or a socket behind the
router without any configuration.

## Metrics

The internal listener exposes Prometheus metrics at `/metrics` and a readiness check at
`/healthz` (503 until every listener is open, then `ok`). The series are:

| Metric | Type | Labels |
|---|---|---|
| `wherry_http_requests_total` | counter | `app`, `status` |
| `wherry_http_request_duration_seconds` | histogram | `app` |
| `wherry_http_upstream_errors_total` | counter | `app` |
| `wherry_deploys_total` | counter | `app`, `result` (`live`, `failed`) |
| `wherry_deploy_duration_seconds` | histogram | `stage` (`build`, `start`, `health`, `swap`) |
| `wherry_instances` | gauge | `app`, `status` |
| `wherry_instance_restarts_total` | counter | `app` |
| `wherry_log_lines_total` | counter | `app` |
| `wherry_log_lines_dropped_total` | counter | `app` |
| `wherry_build_info` | gauge | `version`, `go`, `runtime` |

plus the Go runtime and process collectors. Requests that match no route count under an
empty `app`. The router, supervisor, and log hub report through small observer interfaces,
so none of them imports the Prometheus client.

## Dashboard

The internal listener also serves a small dashboard at `http://127.0.0.1:9000/`: log in
with the admin token, see the apps, and open one for its instance, releases, config vars
(masked until revealed), build logs, and a live log view. It is server-rendered
`html/template` with a few dozen lines of JavaScript for the log stream and the reveal
buttons, and it is read-only: changes go through the CLI.

Set `WHERRY_DASHBOARD_HOST` to a bare hostname and the router serves the dashboard there too,
so I can use `wherry.dev` with apps at `<app>.apps.wherry.dev`. It needs `WHERRY_HTTPS_ADDR`,
because the admin token must not cross plain HTTP, and it cannot sit under `WHERRY_DOMAIN`,
because a name there belongs to an app; the domain itself is fine. Point the host's DNS at the
server and the certificate comes the same way as an app's. The router calls the dashboard's
handler directly rather than proxying to the internal listener, so `/api/`, `/metrics`, and
`/healthz` are not reachable on that host, and the handler sees the real client address and TLS
state. Login attempts are limited per client IP (an IPv6 client by its /64) to a burst of 5 and
one more every 30 seconds, then a 429 with `Retry-After`, and the session cookie has no `Domain`,
so it never reaches the apps.

## The native runtime

`WHERRY_RUNTIME=native` runs the same images without Docker at runtime. Docker still
builds them; the daemon then exports an image's filesystem once (`data/rootfs/<id>`)
and starts each instance as itself, re-executed into new PID, mount, UTS, and IPC
namespaces. That child mounts an overlay with the image as the read-only lower layer
and a per-instance upper layer, fills `/proc`, `/sys` (read-only), `/dev` (six device
nodes), `/tmp`, bind-mounts the host's `resolv.conf` and `hosts`, pivots into the
overlay, empties its capability sets, sets `no_new_privs`, switches to the image's user
(or 65534), and execs the app, which is then PID 1 of its own process tree. Limits come
from a cgroup v2 directory per instance under `WHERRY_CGROUP_ROOT` (default
`/sys/fs/cgroup/wherry`): `memory.max` with swap off, `cpu.max`, `pids.max`, and the
instance is created inside its cgroup rather than moved there afterwards. Output goes
to files the app itself holds open, so the log collector tails them and nothing is
lost across a daemon restart; `state.json` with the pid and its kernel start time is
how a restarted daemon finds and adopts its instances.

Without root the daemon still runs the native runtime, rootless: each instance gets a
user namespace in which the daemon's user is root, which is enough for the mounts and
the pivot. In that mode there are no cgroup limits, no uid switch (there is no other
uid to switch to), `/sys` is a read-only bind of the host's, and the device nodes are
bind-mounted from the host. Everything else is the same code, and the tests run in both
modes (`sudo go test ./internal/runtime/native/` for the privileged half).

What it does and does not isolate, next to Docker's defaults:

| | Docker (as wherry uses it) | native, root | native, rootless |
|---|---|---|---|
| PID, mount, UTS, IPC namespaces | yes | yes | yes |
| Network namespace | yes (bridge, port published on loopback) | no: host network, app binds `$PORT` on loopback (`HOST=127.0.0.1`) | same |
| User namespace | no | no | yes (the daemon's uid is root inside) |
| Memory, CPU, pids limits | yes | yes (cgroup v2) | no |
| Capabilities | Docker's default set | none | none |
| `no_new_privs` | yes | yes | yes |
| seccomp, AppArmor | Docker's defaults | no | no |
| Runs as | image user, root by default | image user, 65534 by default | the daemon's user |

Host networking is the deliberate v1 cut: it needs no veth pairs, bridges, or NAT, and
the router only ever dials loopback; the cost is that apps on one host can see each
other's loopback ports. An app that ignores `HOST` and binds all interfaces is reachable
on its port from the network, so the firewall in front matters more than under Docker.
Because the app is PID 1 of its namespace, a SIGTERM it has no handler for is ignored
by the kernel; `wherry.toml`'s `[process] grace` is how long it gets before SIGKILL,
exactly as under `docker stop`. The same ten-deploy load test as above, run through the
native runtime (rootless, in WSL2) on 2026-10-19:

    requests=23767 ok=23767 failed=0 (none) distinct bodies=11 p50=1.08ms p99=2.12ms max=17.9ms

## Daemon restarts

The containers belong to Docker, not to the daemon, so they keep serving while `wherryd` is
stopped. On start the daemon lists the containers it labelled and compares them with each
app's live release: a running instance of the live release is adopted (route restored,
crash watcher started), anything else running is an orphan and is stopped, exited containers
are removed, a live release with nothing running is started again, and a release left
`building` or `starting` by a cut-off deploy is marked `failed`. One log line reports the
counts:

    msg=reconciled adopted=1 restarted=0 orphans=0 removed=0 failed=0

This runs before any listener opens, so there is no window of 404s. Restart counts live in
memory and start over with the daemon; `ps` shows that.

## Build and run

Go 1.27 or newer and make.

    make build      # bin/wherryd (daemon) and bin/wherry (CLI)
    make test       # go test -race
    make lint       # go vet and golangci-lint
    make run        # start the daemon locally with text logs

`./bin/wherryd --version` prints the version stamped at build time.

## Running it under systemd

`deploy/systemd/wherryd.service` is the unit I deploy with. It expects the binary at
`/usr/local/bin/wherryd`, reads `/etc/wherry/env` (see `wherry.env.example`), keeps its data
in `/var/lib/wherry`, and needs `git` on the host. It follows Docker up and down, because
stopping Docker takes the containers with it and only wherryd brings them back.

It runs as root on purpose. Access to the Docker socket is already root-equivalent, so a
separate user in the `docker` group would look safer without being safer. Instead the unit
drops every capability except binding low ports, makes the filesystem read-only outside the
data directory and a private `/tmp`, and blocks new namespaces.

That also means the unit is Docker-only. The native runtime needs the capabilities,
namespaces, and cgroup access this unit removes, so it is something I run by hand with
`sudo -E ./bin/wherryd` on a test box, not under systemd.

Each `v*` tag builds `linux/amd64` and `linux/arm64` tarballs with both binaries and the
unit, and attaches them to a GitHub release. Tags with a hyphen, like `v0.1.0-rc1`, become
pre-releases. `make dist` builds the same tarballs locally.

## Creating the server

I build the whole box with `deploy/terraform/hetzner`: a server, an SSH key, a firewall, and a
first boot that installs Docker and git, verifies a release against its checksums, and starts
the unit. Port 22 is open only to the addresses in `admin_cidrs`.

    export HCLOUD_TOKEN=...
    cd deploy/terraform/hetzner
    cp terraform.tfvars.example terraform.tfvars
    terraform init
    terraform apply
    ssh root@<ip> cloud-init status --wait

The last line matters. `terraform apply` finishes when the server exists, before first boot
does, so only `cloud-init status` says whether the install worked. If SSH refuses the connection,
the box is still booting, so try again. Otherwise a non-zero exit means the install failed:
`/var/log/cloud-init-output.log` has the reason, or `journalctl -u wherryd` if the daemon would
not start.

`release_version` has to be a published release. First boot only happens once, so Terraform
ignores later changes to anything it feeds, along with the image and SSH key, rather than
replacing the server and losing its data disk. To retry a failed first boot, use
`terraform apply -replace=hcloud_server.wherry`. To change the domain, email, or dashboard host
on a running box, edit `/etc/wherry/env` and restart the unit.

The default is a CAX11, which is arm64; the release architecture follows the server type.

## Configuration

Every setting is an environment variable prefixed `WHERRY_`. The full list with defaults is
in `internal/config/config.go`. A bad value stops the daemon at startup with the variable
named in the error. The ones that pick what runs where: `WHERRY_RUNTIME` (`docker` or
`native`), `WHERRY_CGROUP_ROOT`, `WHERRY_HTTPS_ADDR` with `WHERRY_ACME_EMAIL`,
`WHERRY_DASHBOARD_HOST`, and `WHERRY_RATE_LIMIT_RPS`.

## License

MIT, see LICENSE.
