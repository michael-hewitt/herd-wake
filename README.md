# herd-wake

herd-wake is a lightweight local lifecycle supervisor that works alongside [Laravel Herd](https://herd.laravel.com) to run Node.js development servers only while they are actively needed: it starts a registered Node.js dev server when its Herd URL receives a request, holds the triggering request until the server is ready, proxies HTTP and WebSocket traffic (including Vite HMR), and stops the server again after a configurable period of inactivity — all without changing or disrupting Herd's normal PHP behavior.

See the full specification in [issue #1](https://github.com/michael-hewitt/herd-wake/issues/1).

## Usage

```sh
go build ./cmd/herd-wake
./herd-wake version
./herd-wake projects            # list registered projects
./herd-wake projects --config path/to/config.yaml
./herd-wake start               # run the supervisor daemon in the foreground
./herd-wake status              # daemon uptime and per-project state
```

`herd-wake start` binds one loopback listener per registered project on its
`supervisor_port` and reverse-proxies HTTP to the project's
`application_port`. A request to a stopped project starts its dev server on
demand: the request is held while the server starts (bounded by
`hold_max_wait_seconds` and `hold_max_requests`) and forwarded once it is
ready — the client just sees a slower first response. If startup fails, the
request gets a 503 diagnostic with recent process output, and automatic
retries back off exponentially (1s doubling to a 30s cap); a manual
`project:start`/`project:restart` retries immediately. The daemon serves a
control API on a unix socket at
`~/Library/Application Support/herd-wake/herd-wake.sock`, which
`herd-wake status` queries. Stop the daemon with Ctrl-C.

## Idle shutdown

A running project is stopped gracefully (the configured `shutdown_signal`,
then SIGKILL after `shutdown_timeout_seconds`) once it has seen no activity
for `idle_timeout_minutes` (default 15). The countdown only runs while
nothing is in flight:

- Every completed request restarts the full idle window.
- An in-flight request parks the countdown entirely — a long-running request
  or streaming response holds the stop off indefinitely.
- A request that arrives exactly while an idle stop is in progress is never
  forwarded to the dying process and never dropped: it waits for the stop to
  finish, then cold-starts the project and is served by the fresh process.

After an idle stop the project is `stopped`; the next request cold-starts it
again. Nothing auto-restarts just because the daemon or the machine
restarted — projects wake only on demand.

`herd-wake status` shows, per running project, when its last request
completed (`LAST ACTIVITY`) and when the pending idle stop is scheduled
(`IDLE STOP`), including what is currently holding it off (in-flight
requests, an activity lease, or `always_on`).

### Activity leases

External tools that generate no HTTP traffic (editors, test watchers, ...)
can mark a project active explicitly:

```sh
./herd-wake project:lease dashboard --ttl 45m   # no idle stop before the ttl expires (default 30m)
./herd-wake project:release dashboard           # release the lease early
```

A lease parks the idle countdown until it expires or is released; taking a
new lease replaces the old one. Leasing does not start a stopped project.
Over the control API: `POST /v1/projects/{name}/lease?ttl=45m` and
`DELETE /v1/projects/{name}/lease`.

### Always-on projects

A project with `always_on: true` starts as soon as the daemon starts and is
never idle-stopped. A failed always-on start never keeps the daemon from
running: the project is marked `failed` (see `herd-wake status` /
`herd-wake logs`) and the usual retry paths — a request, or a manual
`project:start` — still apply. Manual `project:stop`/`project:restart` work
as for any other project.

## Configuration

Projects are registered in a user-level config file at
`~/Library/Application Support/herd-wake/config.yaml` — nothing is stored in
your project repositories. See [config.sample.yaml](config.sample.yaml) for a
fully documented example covering a Node-only app and a Laravel Vite server.
