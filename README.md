# herd-wake

**herd-wake starts your Node.js dev servers when their [Laravel Herd](https://herd.laravel.com) URL is visited and stops them again when you stop using them** — so a machine full of Vite, Next.js, and Express projects costs nothing while you are not looking at them.

It is a single-binary lifecycle supervisor that sits behind Herd. Visiting a registered URL starts the right dev server, holds the request until the server is ready, then forwards it; HTTP and WebSocket traffic (including Vite HMR) is proxied transparently; after a configurable idle period the server is stopped gracefully. Herd's normal PHP behavior is never touched: only URLs you explicitly register with `herd proxy` reach herd-wake at all.

The full specification lives in [issue #1](https://github.com/michael-hewitt/herd-wake/issues/1).

## Contents

- [How it works](#how-it-works)
- [Install](#install)
- [Quickstart](#quickstart)
- [Registering URLs with Herd](#registering-urls-with-herd)
  - [Node-only application](#node-only-application)
  - [Laravel + Vite](#laravel--vite)
- [Configuration reference](#configuration-reference)
- [CLI reference](#cli-reference)
- [Idle shutdown, leases, and WebSockets](#idle-shutdown-leases-and-websockets)
- [Troubleshooting](#troubleshooting)
- [Development](#development)

## How it works

```
browser ──▶ Herd (ports 80/443, TLS, .test DNS)
              ├─ php sites ──▶ PHP-FPM                 (unchanged)
              └─ registered Node URLs
                   └──▶ herd-wake supervisor port (127.0.0.1:71xx)
                          └──▶ your dev server   (127.0.0.1:171xx)
```

The daemon (`herd-wake start`) binds **one loopback listener per registered project** on that project's `supervisor_port` and reverse-proxies to the project's `application_port`. Herd is told (once, via `herd proxy`) to forward a public URL like `https://dashboard.test` to the supervisor port. Herd keeps doing DNS, ports 80/443, and HTTPS termination; herd-wake does everything after that.

Each project is always in one of five states:

```
stopped ──(first request or project:start)──▶ starting ──(ready)──▶ running
   ▲                                             │                     │
   │                                        (exit/timeout)     (idle timeout or
   │                                             ▼                project:stop)
   └────────(process exits)──── stopping ◀───────┴─ failed ──(retry)──▶ starting
```

A request arriving at a supervisor port goes through this lifecycle:

1. **Running?** Forward immediately — the supervisor adds sub-millisecond overhead.
2. **Stopped?** Transition to `starting`: run the configured `command` (via `/bin/sh -c`, in its own process group, from `working_directory`), and poll readiness (`http` polls `readiness_url`, `tcp` dials `application_port`).
3. **Hold the request** while the server starts (bounded by `hold_max_wait_seconds` / `hold_max_requests`); every concurrent request shares the *same single startup* — 20 simultaneous cold requests produce exactly one process. Request bodies are streamed, never buffered.
4. **Ready?** Forward the held request(s). The client just sees a slower first response.
5. **Failed or timed out?** Answer `503` with a diagnostic (state, exit status, recent process output). Automatic request-triggered retries back off exponentially (1s doubling to a 30s cap); `project:start`/`project:restart` retries immediately and resets the backoff.

Idle shutdown, WebSocket keep-alive, and manual controls are covered [below](#idle-shutdown-leases-and-websockets). Process safety: herd-wake tracks the exact process group it spawned and signals only that group — never anything matched by name — and stops every group it owns before the daemon itself exits.

## Install

Requirements:

- macOS with [Laravel Herd](https://herd.laravel.com) (for the `.test` URLs; herd-wake itself also works standalone against plain `127.0.0.1` ports)
- Go 1.27+ to build
- Node.js/npm (whatever your projects need) on `PATH` or via `node_path`

Build and install the binary:

```sh
git clone https://github.com/michael-hewitt/herd-wake.git
cd herd-wake
go build ./cmd/herd-wake
mv herd-wake /usr/local/bin/   # or anywhere on your PATH
herd-wake version
```

## Quickstart

From nothing to an on-demand dev server:

**1. Create the config file** at `~/Library/Application Support/herd-wake/config.yaml` (create the directory if needed):

```yaml
projects:
  dashboard:
    public_url: https://dashboard.test
    supervisor_port: 7101
    application_port: 17101
    working_directory: /Users/you/Code/dashboard
    command: npm run dev -- --host 127.0.0.1 --port 17101 --strictPort
```

Adjust `working_directory` to a real project. The command must make the dev server listen on `127.0.0.1:<application_port>` — for Vite that means passing `--host 127.0.0.1` (it binds only `localhost`/`::1` otherwise) and `--strictPort` (so a port conflict fails loudly instead of drifting to another port).

**2. Check the registration:**

```sh
herd-wake projects
```

**3. Start the daemon** (foreground; Ctrl-C stops it and every dev server it started):

```sh
herd-wake start
```

**4. Visit the supervisor port directly** — this works before any Herd setup:

```sh
curl http://127.0.0.1:7101/
```

The first request cold-starts the dev server (a second or two for Vite), then returns the page. Repeat the curl: it is now instant. `herd-wake status` shows the project `running`, its PID, and when the idle stop is scheduled.

**5. Register the URL with Herd** so `https://dashboard.test` reaches it:

```sh
herd proxy dashboard http://127.0.0.1:7101
```

Now open `https://dashboard.test` in a browser. Herd terminates HTTPS and forwards to herd-wake; herd-wake wakes the project on demand. After `idle_timeout_minutes` (default 15) without traffic, the dev server is stopped again — the next visit revives it.

## Registering URLs with Herd

herd-wake never touches Herd's configuration. You register each project's public URL once with `herd proxy`, pointing it at the project's `supervisor_port`. Only those URLs reach herd-wake; every other Herd site behaves exactly as before, whether herd-wake is running, stopped, or uninstalled.

### Node-only application

One URL, one project:

```sh
herd proxy dashboard http://127.0.0.1:7101
```

`https://dashboard.test` now fronts the supervisor port from the quickstart. To undo it, remove the proxy in Herd (`herd unproxy dashboard`, or via the Herd UI) — nothing else to clean up.

### Laravel + Vite

For a Laravel app with a Vite dev server, the pattern from the spec (§12) is:

- **Laravel stays a normal Herd PHP site** at `https://accounts.test`. herd-wake is not involved in serving PHP.
- **Only Vite is registered with herd-wake**, behind a *stable* companion URL such as `https://vite.accounts.test`. Because the URL is stable, Laravel's Vite integration can reference it permanently instead of a direct, session-dependent port.

Config entry (see [config.sample.yaml](config.sample.yaml) for the fully commented version):

```yaml
projects:
  accounts-vite:
    public_url: https://vite.accounts.test
    supervisor_port: 7102
    application_port: 17102
    working_directory: ~/Code/accounts
    command: npm run dev -- --host 127.0.0.1 --port 17102 --strictPort
```

Herd registration:

```sh
herd proxy vite.accounts http://127.0.0.1:7102
```

Point Vite (and Laravel's `laravel-vite-plugin`) at the stable URL in `vite.config.js`:

```js
export default defineConfig({
    plugins: [laravel({ input: ['resources/css/app.css', 'resources/js/app.js'] })],
    server: {
        host: '127.0.0.1',
        port: 17102,
        strictPort: true,
        // The URL browsers should use for assets and HMR — the stable Herd
        // proxy URL, not the raw port. laravel-vite-plugin writes this into
        // the public/hot file, so Blade's @vite tags emit it.
        origin: 'https://vite.accounts.test',
        hmr: {
            host: 'vite.accounts.test',
            clientPort: 443,
            protocol: 'wss',
        },
    },
});
```

With that in place: a page load on `https://accounts.test` emits asset URLs on `https://vite.accounts.test`, the first asset or HMR request wakes Vite through herd-wake, and HMR WebSockets are proxied end-to-end (an open HMR connection also keeps Vite from being considered idle).

**Honest caveats — Laravel integration polish is post-MVP:**

- `laravel-vite-plugin` decides between dev server and built assets by the presence of the `public/hot` file, which *it* creates while Vite runs and removes when Vite exits. So after herd-wake idle-stops Vite, Laravel falls back to built assets, and page loads alone will not wake Vite again (nothing references the vite URL anymore). Wake it explicitly (`herd-wake project:start accounts-vite`, or reload once Vite is running), pin the hot file yourself (`echo 'https://vite.accounts.test' > public/hot` — the plugin may still remove it on Vite's next exit), or sidestep the whole issue with `always_on: true` on the Vite project if you prefer it permanently up.
- Verify the `server.origin`-to-hot-file behavior against your `laravel-vite-plugin` version; older versions differ.
- First-class management of this handshake (auto-maintained hot file, automatic Herd proxy registration) is deliberately out of scope for the MVP.

## Configuration reference

Projects are registered in a single user-level YAML file — nothing is stored in your project repositories:

```
~/Library/Application Support/herd-wake/config.yaml     (override: --config)
```

The file is a `projects:` map of project names to settings. Unknown fields are rejected (typos fail loudly), validation reports every problem with its project and field, and all `supervisor_port`/`application_port` values must be unique across the whole file. [config.sample.yaml](config.sample.yaml) is a fully commented example kept in sync with this table.

### Required fields

| Field | Type | Meaning |
| --- | --- | --- |
| `public_url` | string | The Herd URL fronting this project, e.g. `https://dashboard.test`. Must be an absolute `http(s)` URL. Informational: shown in `status`/`projects`; routing is done by Herd, not by this value. |
| `supervisor_port` | int | Loopback port herd-wake listens on for this project — the `herd proxy` target. 1–65535, unique across the config. |
| `application_port` | int | Loopback port the dev server itself listens on; herd-wake proxies to `127.0.0.1:<application_port>`. Unique, and different from `supervisor_port`. |
| `working_directory` | string | Absolute directory the command runs in. Must exist. A leading `~/` is expanded. |
| `command` | string | The dev-server command, run via `/bin/sh -c` (so quoting, `$VARS`, and `&&` chains behave like your terminal) in its own process group. Pin it to `application_port`, make the port strict, and bind `127.0.0.1` explicitly (Vite: `--host 127.0.0.1 --port N --strictPort`). |

### Optional fields

| Field | Type | Default | Meaning |
| --- | --- | --- | --- |
| `readiness_strategy` | `http` \| `tcp` | `http` | How readiness is detected before held requests are forwarded: `http` polls `readiness_url` until it answers; `tcp` dials `application_port` until the connection succeeds. |
| `readiness_url` | string | `http://127.0.0.1:<application_port>/` | URL polled when `readiness_strategy` is `http`. |
| `startup_timeout_seconds` | int | `60` | How long a cold start may take before it is declared failed. |
| `idle_timeout_minutes` | int | `15` | Stop the dev server after this long without activity (requests, protected WebSockets, or a lease). |
| `idle_timeout_seconds` | int | unset | Seconds-granularity idle timeout that takes precedence over `idle_timeout_minutes` when set. Primarily a testing hook. |
| `hold_max_wait_seconds` | int | `startup_timeout_seconds + 5` | How long one request may be held while the project starts. The default outlives the startup attempt so the caller sees its real outcome. |
| `hold_max_requests` | int | `100` | How many requests may be held at once during a cold start; requests over the limit get a 503. |
| `websockets_keep_alive` | bool | `true` | Whether open WebSocket connections (e.g. Vite HMR) keep the project from being considered idle. With `false`, an upgrade counts only as momentary activity and an idle stop closes any open sockets. |
| `shutdown_signal` | string | `SIGTERM` | Graceful-termination signal sent to the process group. One of `SIGTERM`, `SIGINT`, `SIGQUIT`, `SIGHUP`, `SIGUSR1`, `SIGUSR2`, `SIGKILL`. |
| `shutdown_timeout_seconds` | int | `10` | How long to wait for the process group to exit after `shutdown_signal` before it is force-killed (SIGKILL). |
| `always_on` | bool | `false` | Start this project as soon as the daemon starts and never idle-stop it. A failed always-on start never keeps the daemon from running. Manual `project:stop`/`project:restart` still work. |
| `env` | map | — | Extra environment variables for the dev server (merged over the daemon's environment; project entries win). |
| `node_path` | string | — | Absolute path to the Node executable (or its directory) to put first on `PATH`, for projects pinned to a specific Node version. |
| `log_retention_days` | int | `7` | Log rotation/retention: on each project start, a log over 10 MiB is rotated to `<name>.log.old` (at most one rotation kept), and logs untouched for this many days are deleted. |
| `listen_host` | string | `127.0.0.1` | Address the supervisor listener binds for this project. Non-loopback values are rejected unless `allow_non_loopback: true`. |
| `allow_non_loopback` | bool | `false` | Explicit opt-in required to bind a non-loopback `listen_host`. |

## CLI reference

```
herd-wake <command> [flags] [args]
```

| Command | What it does |
| --- | --- |
| `herd-wake start` | Run the supervisor daemon in the foreground: binds one listener per project, serves the control API on the unix socket, starts `always_on` projects. Ctrl-C (or SIGTERM) stops the daemon *and* every dev server it started. |
| `herd-wake status` | Daemon PID/uptime/version plus a per-project table: state, PID, uptime, last activity, scheduled idle stop (and what is holding it off), last exit, URL, ports. |
| `herd-wake projects` | List every registered project from the config file (works without the daemon running). |
| `herd-wake project:start <name>` | Start a project's dev server and wait until it is ready. Bypasses and resets the failure backoff. |
| `herd-wake project:stop <name>` | Gracefully stop a project's dev server (signal, then force-kill after its shutdown timeout). |
| `herd-wake project:restart <name>` | Stop (if needed) and start a project's dev server. |
| `herd-wake project:lease <name>` | Mark a project active for `--ttl` (default 30m) so it is not idle-stopped — for tools that generate no HTTP traffic. Does not start a stopped project; a new lease replaces the old one. |
| `herd-wake project:release <name>` | Release a project's activity lease early. |
| `herd-wake logs <name>` | Print the project's recent combined stdout/stderr and the path of the full on-disk log. |
| `herd-wake version` | Print the herd-wake version. |

Flags (place them before positional arguments):

| Flag | Applies to | Default | Meaning |
| --- | --- | --- | --- |
| `--config <path>` | `start`, `projects` | `~/Library/Application Support/herd-wake/config.yaml` | Config file to load. |
| `--socket <path>` | `start`, `status`, `project:*`, `logs` | `~/Library/Application Support/herd-wake/herd-wake.sock` | Control socket the daemon serves / clients query. |
| `--log-dir <path>` | `start` | `~/Library/Application Support/herd-wake/logs` | Directory for per-project process logs (`<name>.log`). |
| `--ttl <duration>` | `project:lease` | `30m` | How long the lease lasts, e.g. `45m`, `2h`. |
| `--lines <n>` | `logs` | `0` | Maximum lines to print (0 = everything buffered, up to 200). |

The control API behind the CLI is plain HTTP+JSON over the unix socket, versioned under `/v1/`: `GET /v1/status`, `POST /v1/projects/{name}/start|stop|restart`, `POST /v1/projects/{name}/lease?ttl=45m`, `DELETE /v1/projects/{name}/lease`, `GET /v1/projects/{name}/logs?lines=N`.

## Idle shutdown, leases, and WebSockets

A running project is stopped gracefully (the configured `shutdown_signal`, then SIGKILL after `shutdown_timeout_seconds`) once it has seen no activity for its idle timeout. The countdown only runs while nothing is in flight:

- Every completed request restarts the full idle window.
- An in-flight request parks the countdown entirely — a long-running request or streaming response holds the stop off indefinitely.
- An open WebSocket connection (e.g. Vite HMR) parks the countdown too, unless the project sets `websockets_keep_alive: false`.
- An activity lease (`herd-wake project:lease`) parks the countdown until it expires or is released.
- A request that arrives exactly while an idle stop is in progress is never forwarded to the dying process and never dropped: it waits for the stop to finish, then cold-starts the project and is served by the fresh process.

After an idle stop the project is `stopped`; the next request cold-starts it again. Nothing auto-restarts just because the daemon or the machine restarted — projects wake only on demand (`always_on` projects being the deliberate exception).

WebSocket upgrades are proxied like any other traffic, including through a cold start: the upgrade is held while the server starts, so the first HMR connection can itself be the thing that wakes Vite. With `websockets_keep_alive: false`, a project idles out on HTTP traffic alone and any still-open sockets are closed as the process exits — auto-reconnecting clients (Vite HMR is one) cold-start the project again with their next attempt.

`herd-wake status` shows, per running project, when its last request completed (`LAST ACTIVITY`) and when the pending idle stop is scheduled (`IDLE STOP`), including what is currently holding it off (in-flight requests, a lease, or `always_on`).

## Troubleshooting

**Where everything lives.** Config `~/Library/Application Support/herd-wake/config.yaml`; control socket `~/Library/Application Support/herd-wake/herd-wake.sock`; process logs `~/Library/Application Support/herd-wake/logs/<name>.log` (also surfaced by `herd-wake logs <name>`, and quoted in 503 diagnostics). All overridable with `--config` / `--socket` / `--log-dir`.

**Cold start returns 503 "readiness … timeout".** The command started but never answered the readiness probe within `startup_timeout_seconds`. In rough order of likelihood:

1. *The server bound the wrong interface.* herd-wake probes and proxies `127.0.0.1`; Vite (and some other servers) bind only `localhost`/`::1` unless told otherwise. Fix the command: `--host 127.0.0.1`.
2. *The server picked a different port.* Without `--strictPort` (or equivalent), a dev server finding its port busy silently moves to another one, which herd-wake is not watching. Always pin and strict the port.
3. *It is genuinely slow* (big install step, cold cache): raise `startup_timeout_seconds`.
4. *The readiness URL is wrong* for apps that 404 on `/`: point `readiness_url` at a path that answers, or use `readiness_strategy: tcp`.

`herd-wake logs <name>` shows what the server actually printed.

**Port conflict.** If something else already holds `application_port`, a `--strictPort`-style command exits immediately and the 503 diagnostic shows the exit and its output. If something holds a `supervisor_port`, `herd-wake start` itself refuses to start with a bind error naming the project — pick a different port or stop the squatter (`lsof -nP -iTCP:<port>` shows who it is).

**503s keep coming after a failure.** That is the backoff, not a hang: after a failed start, request-triggered retries wait 1s, 2s, 4s, … capped at 30s, and requests during the wait get an immediate 503 saying when the next retry may run. `herd-wake project:start <name>` (or `project:restart`) retries immediately and resets the backoff.

**`herd-wake status` says the daemon is not running.** Start it with `herd-wake start`. If you used a custom `--socket` for the daemon, pass the same one to every other command.

**Stale socket / "another daemon is already running".** The daemon refuses to start while another daemon answers on the control socket (the error names its PID). A *stale* socket file — left by a crash, with nothing accepting on it — is detected and removed automatically. If the path exists but is not a socket at all, herd-wake asks you to move it out of the way rather than deleting it.

**Herd URL gives 502/504 but `curl 127.0.0.1:<supervisor_port>` works.** The Herd proxy target does not match the project's `supervisor_port` — re-register with `herd proxy <name> http://127.0.0.1:<supervisor_port>`. If the direct curl fails too, the daemon is not running.

**Project stops while I'm still working.** Idle detection sees HTTP traffic and open WebSockets. Tools that make neither (editors, test watchers) can hold a project up with `herd-wake project:lease <name> --ttl 2h`, or set `always_on: true` for permanently-up projects, or raise `idle_timeout_minutes`.

**A project I stopped came back.** Any request to its URL wakes it — that is the point. Stop traffic (close the browser tab with the HMR socket) or unregister the Herd proxy if you want it to stay down.

## Development

Layout: `cmd/herd-wake` (CLI), `internal/config` (config load/validate), `internal/daemon` (wiring, states, control-API provider), `internal/proxy` (reverse proxy, on-demand holding, WebSocket tunneling), `internal/process` (process-group supervisor, logs, backoff), `internal/idle` (activity tracking, idle monitor), `internal/control` (unix-socket HTTP API + client), `internal/testproc` (test-only child-process helpers and WebSocket test client).

```sh
go test -race ./...                          # fast inner loop (e2e tests skip themselves)
HW_E2E=1 go test -race ./e2e/ -v -count=1    # full E2E acceptance suite
golangci-lint run
```

The E2E suite (`e2e/`) builds the real binary, runs the daemon as a subprocess against the Vite fixture in `testdata/vite-fixture`, and exercises the spec's acceptance criteria — cold start, single-flight under 20 concurrent requests, warm-request overhead, HMR-WebSocket keep-alive, idle stop and revival, two-project isolation, failure diagnostics, and no-auto-start after a daemon restart — purely through public surfaces (supervisor ports, CLI, control API). It is guarded by `HW_E2E=1` (and skips under `-short`), needs `node`/`npm` on `PATH`, and installs the fixture's pinned dependencies automatically (`npm ci`) on first run. CI runs it on `macos-latest`; the unit jobs stay on Linux.
