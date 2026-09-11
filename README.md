# herd-wake

**herd-wake starts your Node.js dev servers when their [Laravel Herd](https://herd.laravel.com) URL is visited and stops them again when you stop using them** — so a machine full of Vite, Next.js, and Express projects costs nothing while you are not looking at them.

It is a single-binary lifecycle supervisor that sits behind Herd. Visiting a registered URL starts the right dev server, holds the request until the server is ready, then forwards it; HTTP and WebSocket traffic (including Vite HMR) is proxied transparently; after a configurable idle period the server is stopped gracefully. Herd's normal PHP behavior is never touched: only URLs registered with `herd proxy` — by you, or by `herd-wake sync` for git worktrees it discovers — reach herd-wake at all. For a folder of git worktrees, one wildcard proxy (`*.webapp.test`) is enough: every worktree gets `https://<worktree>.webapp.test` the moment it exists, with nothing to register per branch.

The full specification lives in [issue #1](https://github.com/michael-hewitt/herd-wake/issues/1).

## Contents

- [How it works](#how-it-works)
- [Install](#install)
- [Quickstart](#quickstart)
- [Registering URLs with Herd](#registering-urls-with-herd)
  - [Node-only application](#node-only-application)
  - [Laravel + Vite](#laravel--vite)
- [Configuration reference](#configuration-reference)
  - [Sharing a port: routing by Host](#sharing-a-port-routing-by-host)
  - [The projects.d directory](#the-projectsd-directory)
- [Reloading the configuration](#reloading-the-configuration)
- [Worktrees: automatic URLs](#worktrees-automatic-urls)
  - [Wildcard mode (recommended)](#wildcard-mode-recommended)
  - [How a worktree is resolved](#how-a-worktree-is-resolved)
  - [Per-worktree mode (the alternative)](#per-worktree-mode-the-alternative)
  - [Running sync](#running-sync)
  - [Hooking into worktree tooling](#hooking-into-worktree-tooling)
  - [What happens on removal](#what-happens-on-removal)
  - [The Herd-collision safeguard](#the-herd-collision-safeguard)
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

The daemon (`herd-wake start`) binds a loopback listener on each `supervisor_port` and reverse-proxies to the right project's `application_port`. In the simplest setup that is **one listener per project**; projects that set a `host` can instead share one port, on which the daemon routes by the request's `Host` header, and a [wildcard discovery entry](#wildcard-mode-recommended) shares one port among every worktree of a repository. Herd is told (once, via `herd proxy`) to forward a public URL like `https://dashboard.test` — or a whole wildcard like `*.webapp.test` — to the supervisor port. Herd keeps doing DNS, ports 80/443, and HTTPS termination; herd-wake does everything after that.

Each project is always in one of five states:

```
stopped ──(first request or project:start)──▶ starting ──(ready)──▶ running
   ▲                                             │                     │
   │                                        (exit/timeout)     (idle timeout or
   │                                             ▼                project:stop)
   └────────(process exits)──── stopping ◀───────┴─ failed ──(retry)──▶ starting
```

A request arriving at a supervisor port goes through this lifecycle:

0. **Which project?** On a port owned by one project, that project. On a shared port, the project whose `host` matches the request's `Host` (port stripped, case-insensitive); on a wildcard listener, the worktree named by the first label of the host, materialised as a project on first sight. A hostname nothing claims gets a `404` diagnostic naming it — never a hang, never another project's server.
1. **Running?** Forward immediately — the supervisor adds sub-millisecond overhead.
2. **Stopped?** Transition to `starting`: run the configured `command` (via `/bin/sh -c`, in its own process group, from `working_directory`), and poll readiness (`http` polls `readiness_url`, `tcp` dials `application_port`).
3. **Hold the request** while the server starts (bounded by `hold_max_wait_seconds` / `hold_max_requests`); every concurrent request shares the *same single startup* — 20 simultaneous cold requests produce exactly one process. Request bodies are streamed, never buffered.
4. **Ready?** Forward the held request(s). The client just sees a slower first response.
5. **Failed or timed out?** Answer `503` with a diagnostic (state, exit status, recent process output). Automatic request-triggered retries back off exponentially (1s doubling to a 30s cap); `project:start`/`project:restart` retries immediately and resets the backoff.

Idle shutdown, WebSocket keep-alive, and manual controls are covered [below](#idle-shutdown-leases-and-websockets); [reloading the configuration](#reloading-the-configuration) applies config edits to the running daemon without touching unchanged projects. Process safety: herd-wake tracks the exact process group it spawned and signals only that group — never anything matched by name — and stops every group it owns before the daemon itself exits.

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
herd proxy dashboard http://127.0.0.1:7101 --secure
```

Now open `https://dashboard.test` in a browser. Herd terminates HTTPS and forwards to herd-wake; herd-wake wakes the project on demand. After `idle_timeout_minutes` (default 15) without traffic, the dev server is stopped again — the next visit revives it.

## Registering URLs with Herd

For hand-written projects you register each public URL once with `herd proxy`, pointing it at the project's `supervisor_port` (for discovered worktrees, [`herd-wake sync`](#worktrees-automatic-urls) runs the same command for you — once per repository in wildcard mode, since Herd's proxy site file and certificate cover `*.<name>.test`). Only those URLs reach herd-wake; every other Herd site behaves exactly as before, whether herd-wake is running, stopped, or uninstalled. herd-wake never edits Herd's global nginx configuration — the only Herd state it touches is the per-site proxy entry `herd proxy`/`herd unproxy` manage.

Always pass `--secure`: it makes Herd issue a trusted TLS certificate for the domain so the `https://` URL (and `wss://` HMR) actually works. Without it Herd registers the proxy HTTP-only and browsers/curl fail certificate verification on the `https://` URL.

### Node-only application

One URL, one project:

```sh
herd proxy dashboard http://127.0.0.1:7101 --secure
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
herd proxy vite.accounts http://127.0.0.1:7102 --secure
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
- First-class management of this handshake (an auto-maintained hot file) is deliberately out of scope for the MVP. Automatic Herd proxy registration exists for [discovered worktrees](#worktrees-automatic-urls); hand-written projects still register with `herd proxy` themselves.

## Configuration reference

Projects are registered in a user-level YAML file — nothing is stored in your project repositories:

```
~/Library/Application Support/herd-wake/config.yaml     (override: --config)
~/Library/Application Support/herd-wake/projects.d/     (optional, see below)
```

The file is a `projects:` map of project names to settings, plus an optional `discovery:` list of [worktree templates](#discovery-entries). Unknown fields are rejected (typos fail loudly), validation reports every problem with its project (or discovery entry) and field, and every `application_port` must be unique across the whole configuration (main file and `projects.d` together). A `supervisor_port` is unique too, except that projects which all set [`host`](#sharing-a-port-routing-by-host) may share one. [config.sample.yaml](config.sample.yaml) is a fully commented example kept in sync with these tables. A running daemon picks up edits with [`herd-wake reload`](#reloading-the-configuration).

### Required fields

| Field | Type | Meaning |
| --- | --- | --- |
| `public_url` | string | The Herd URL fronting this project, e.g. `https://dashboard.test`. Must be an absolute `http(s)` URL. Informational: shown in `status`/`projects`; routing is done by Herd (and, on a shared port, by `host`), not by this value. Optional when `host` is set: it defaults to `https://<host>`. |
| `supervisor_port` | int | Loopback port herd-wake listens on for this project — the `herd proxy` target. 1–65535; unique across the config unless shared by `host` projects (below). |
| `application_port` | int | Loopback port the dev server itself listens on; herd-wake proxies to `127.0.0.1:<application_port>`. Unique, and different from `supervisor_port`. |
| `working_directory` | string | Absolute directory the command runs in. Must exist. A leading `~/` is expanded. |
| `command` | string | The dev-server command, run via `/bin/sh -c` (so quoting, `$VARS`, and `&&` chains behave like your terminal) in its own process group. Pin it to `application_port`, make the port strict, and bind `127.0.0.1` explicitly (Vite: `--host 127.0.0.1 --port N --strictPort`). |

### Optional fields

| Field | Type | Default | Meaning |
| --- | --- | --- | --- |
| `host` | string | — | The exact hostname this project answers to, e.g. `feat-huddle.preview.example.com` (a hostname, no scheme or port; compared case-insensitively). Lets several projects share one `supervisor_port`, routed by the request's `Host` header — see [Sharing a port](#sharing-a-port-routing-by-host). |
| `readiness_strategy` | `http` \| `tcp` | `http` | How readiness is detected before held requests are forwarded: `http` polls `readiness_url` until it answers; `tcp` dials `application_port` until the connection succeeds. |
| `readiness_url` | string | `http://127.0.0.1:<application_port>/` | URL polled when `readiness_strategy` is `http`. |
| `startup_timeout_seconds` | int | `60` | How long a cold start may take before it is declared failed. |
| `idle_timeout_minutes` | int | `15` | Stop the dev server after this long without activity (requests, protected WebSockets, or a lease). |
| `idle_timeout_seconds` | int | unset | Seconds-granularity idle timeout that takes precedence over `idle_timeout_minutes` when set. Primarily a testing hook. |
| `hold_max_wait_seconds` | int | `startup_timeout_seconds + 5` | How long one request may be held while the project starts. The default outlives the startup attempt so the caller sees its real outcome. |
| `hold_max_requests` | int | `100` | How many requests may be held at once during a cold start; requests over the limit get a 503. |
| `websockets_keep_alive` | bool | `true` | Whether open WebSocket connections (e.g. Vite HMR) keep the project from being considered idle. With `false`, an upgrade counts only as momentary activity and an idle stop closes any open sockets. |
| `rewrite_host` | bool | `false` | How the dev server sees the `Host` header. `false` forwards the inbound public host (e.g. `dashboard.test`) unchanged. `true` sends `Host: 127.0.0.1:<application_port>` upstream while `X-Forwarded-Host` carries the public host (the other `X-Forwarded-*` headers are unaffected) — for servers that refuse non-localhost hosts, such as Vite's `server.allowedHosts` (`403 Blocked request`) or webpack-dev-server's `allowedHosts`. Applies to WebSocket upgrades (HMR) too. |
| `shutdown_signal` | string | `SIGTERM` | Graceful-termination signal sent to the process group. One of `SIGTERM`, `SIGINT`, `SIGQUIT`, `SIGHUP`, `SIGUSR1`, `SIGUSR2`, `SIGKILL`. |
| `shutdown_timeout_seconds` | int | `10` | How long to wait for the process group to exit after `shutdown_signal` before it is force-killed (SIGKILL). |
| `always_on` | bool | `false` | Start this project as soon as the daemon starts and never idle-stop it. A failed always-on start never keeps the daemon from running. Manual `project:stop`/`project:restart` still work. |
| `env` | map | — | Extra environment variables for the dev server (merged over the daemon's environment; project entries win). |
| `node_path` | string | — | Absolute path to the Node executable (or its directory) to put first on `PATH`, for projects pinned to a specific Node version. |
| `log_retention_days` | int | `7` | Log rotation/retention: on each project start, a log over 10 MiB is rotated to `<name>.log.old` (at most one rotation kept), and logs untouched for this many days are deleted. |
| `listen_host` | string | `127.0.0.1` | Address the supervisor listener binds for this project. Non-loopback values are rejected unless `allow_non_loopback: true`. |
| `allow_non_loopback` | bool | `false` | Explicit opt-in required to bind a non-loopback `listen_host`. |

### Sharing a port: routing by Host

By default every project owns its `supervisor_port`: whatever arrives there is that project's, and the `Host` header is not consulted. Projects that set `host` share instead:

```yaml
projects:
  preview-alpha:
    host: alpha.preview.example.com     # public_url defaults to https://alpha.preview.example.com
    supervisor_port: 7103
    application_port: 17103
    working_directory: ~/previews/alpha
    command: node --import tsx src/server.ts
  preview-beta:
    host: beta.preview.example.com
    supervisor_port: 7103               # the same port: routed by Host
    application_port: 17104
    working_directory: ~/previews/beta
    command: node --import tsx src/server.ts
```

On port 7103 the daemon reads each request's `Host` (port stripped, lowercased) and forwards to the project that claims it — WebSocket upgrades included. A hostname no project claims gets a `404` diagnostic naming the host and the hosts served there, never a hang and never another project's server. The rules, enforced at load time with a field-level error:

- A `supervisor_port` is used either by **exactly one** project without `host`, or **only** by projects that all set `host` — mixing the two on one port is rejected. Sharers must bind the same `listen_host`.
- Every `host` is unique across the configuration, and every `application_port` stays exclusive.
- A [wildcard discovery entry](#wildcard-mode-recommended)'s port is its own: no project may use it.

Reloads treat a `host` project like any other: it joins or leaves the shared listener without disturbing its neighbours, and the listener closes with its last member.

**Behind another proxy.** A shared listener is what you want when herd-wake runs behind something other than Herd — nginx on a Linux box fronting `*.preview.example.com`, say. One static `server` block forwards every hostname to the one supervisor port (`proxy_pass http://127.0.0.1:7103; proxy_set_header Host $host;` plus the usual `X-Forwarded-*` and WebSocket upgrade headers), and adding or removing a branch is a herd-wake config change only — nginx never needs a rewrite or a reload. herd-wake forwards the inbound `Host` unchanged (or rewrites it with `rewrite_host`), so the app sees the public origin.

### Discovery entries

Each entry of the top-level `discovery:` list is a template for one family of git worktrees — one directory per branch under a workspace folder. Its `mode` decides how the worktrees become projects: `wildcard` (one shared listener, worktrees resolved from the `Host` header on demand — see [Wildcard mode](#wildcard-mode-recommended)) or `proxy` (the default: [`herd-wake sync`](#per-worktree-mode-the-alternative) registers one project and Herd proxy per worktree in `projects.d/<name>.yaml`). Entries are only allowed in the main file.

| Field | Type | Default | Meaning |
| --- | --- | --- | --- |
| `name` | string | required | Identifies the entry. A DNS label (lowercase letters, digits, hyphens; ≤63 chars); unique across entries. In proxy mode it names the managed file `projects.d/<name>.yaml`; in wildcard mode it is the default `base_domain`'s first label. |
| `mode` | `proxy` \| `wildcard` | `proxy` | `wildcard`: the daemon binds `supervisor_port` once and serves `https://<label>.<base_domain>` from `<directory>/<label>` on demand; `sync` only registers one wildcard Herd proxy. `proxy`: `sync` writes one project per worktree with its own port and Herd proxy. |
| `base_domain` | string | `<name>.test` | Wildcard mode only: the domain worktrees are served under. At least two labels; its Herd site is the domain without its TLD (`webapp.test` → `herd proxy webapp …`, which covers `*.webapp.test`). |
| `supervisor_port` | int | required in wildcard mode | Wildcard mode only: the one loopback port the entry's shared listener binds — the `herd proxy` target. Its own: no project or other entry may use it. (Rejected in proxy mode, where ports come from `supervisor_port_range`.) |
| `directory` | string | required | The directory whose immediate subdirectories are worktree candidates (wildcard mode resolves `<directory>/<label>`; proxy mode scans it). Must exist; `~/` is expanded. |
| `repository` | string | — | Restrict candidates to *linked worktrees* of this git repository: the subdirectory's `.git` must be a file whose `gitdir:` resolves under `<repository>/.git/worktrees/`. Standalone repositories and worktrees of other repositories in the folder are skipped (and listed as such). Without it, any subdirectory with a `.git` file or directory is a candidate. Must exist and contain `.git`; `~/` is expanded. |
| `require_files` | list | — | Relative paths that must all exist inside a subdirectory for it to become a project. |
| `url_template` | string | `https://{name}.test` | Proxy mode only: each project's `public_url`; `{name}` (required) is the subdirectory name. (Rejected in wildcard mode, where the URL is always `https://<label>.<base_domain>`.) |
| `command` | string | required | The template's `command`, run in each worktree. It must make the server listen on `127.0.0.1:<application_port>` — since that port differs per worktree, derive it the way `port_command` does rather than hard-coding it. |
| `port_command` | string | — | Run in the worktree (`/bin/sh -c`, with the template's `env` and `node_path` on `PATH`, 15 s timeout); its output — a bare port on the last line, or a URL (explicit port, else 80/443 by scheme) — becomes `application_port`. Wildcard mode runs it when a worktree is first served (and again on `project:restart`, moving the project if the port changed); a failure is a `503` diagnostic with the output, retried with backoff. Proxy mode runs it on every sync; a changed port updates the project, and on failure the worktree is skipped this time (an already-registered project keeps its previous settings). |
| `application_port_range` | `[low, high]` | required without `port_command` | Where projects get their `application_port` when there is no `port_command`: the lowest port not used anywhere in the configuration (proxy mode) or by any live project (wildcard mode). |
| `supervisor_port_range` | `[low, high]` | required in proxy mode | Proxy mode only: where new projects get their `supervisor_port`: the lowest port not used anywhere in the configuration (main file and every `projects.d` file). Allocated ports are written to the managed file and never change afterwards, so URLs and Herd proxies stay stable across syncs. (Rejected in wildcard mode.) |
| `herd` | bool | `true` | Create/remove Herd proxy entries for this entry (one per worktree in proxy mode; one wildcard proxy for `base_domain` in wildcard mode). With `false` (or when the `herd` CLI is not found) sync prints the commands for you to run instead. |
| `exclude` | list | `[".*"]` | `path.Match` glob patterns on the subdirectory name; matches are never candidates. Setting it replaces the default. |
| *any per-project field* | | | `env`, `node_path`, `rewrite_host`, `readiness_strategy`, `startup_timeout_seconds`, `idle_timeout_minutes`, `websockets_keep_alive`, `shutdown_signal`, `shutdown_timeout_seconds`, `log_retention_days`, `always_on`, `hold_*`, `listen_host`, … are accepted in the template and copied verbatim to every generated project. `public_url`, `host`, `application_port`, and `working_directory` are generated and therefore rejected in a template (`supervisor_port` too, in proxy mode). |

### The projects.d directory

Next to the main config file, an optional `projects.d/` directory holds additional project files:

```
~/Library/Application Support/herd-wake/
├── config.yaml                 # hand-written, never touched by tooling
└── projects.d/
    ├── webapp.yaml             # managed by `herd-wake sync` (discovery "webapp"), one project per worktree
    └── preview-42.yaml         # or hand-written: one project (or several) per file
```

Each `projects.d/*.yaml` file has the same layout as the main file — a `projects:` map with one project or many — and may be empty. Files are read in name order and their projects are merged into the main file's; a project name defined more than once anywhere (main file or any `projects.d` file) is a validation error naming both sources, e.g. `project "webapp": name: defined in both config.yaml and projects.d/webapp.yaml`. Only `*.yaml` files directly inside the directory count (hidden files, other extensions, and subdirectories are ignored), and a missing directory is fine. Unknown top-level keys are rejected in these files exactly as in the main file.

The directory is always derived from the main file's location (`--config /some/dir/config.yaml` means `/some/dir/projects.d/`). This is the layout automation writes into — [`herd-wake sync`](#per-worktree-mode-the-alternative) keeps one file per proxy-mode discovery entry, opened by a `# Managed by herd-wake sync — do not edit.` header — so your hand-written `config.yaml` is never rewritten, and sync in turn never overwrites a `projects.d` file that lacks that header. `herd-wake projects` shows each project's source file. (Wildcard entries write nothing here: their projects live in the daemon's memory.)

## Reloading the configuration

The daemon re-reads its configuration without restarting and without disturbing projects whose settings did not change:

```sh
herd-wake reload            # over the control socket; prints the diff
kill -HUP $(pgrep -x herd-wake)   # the same reload, via signal
```

A reload re-loads and re-validates the main file plus `projects.d`, then diffs the result by project name against the live set:

| Project is… | What happens |
| --- | --- |
| **added** | Supervisor, activity tracker, idle monitor, and listener are created and the port bound. `always_on` projects start immediately; anything else cold-starts on its first request. |
| **removed** | Its listener closes first (the port stops accepting), then its process group is stopped gracefully (signal, force-kill after the shutdown timeout), and its idle monitor ends. |
| **changed** (any field differs) | If it is starting or running it is stopped (whole group drained). The new config is swapped in with a fresh supervisor (failure backoff reset); it cold-starts on the next request (`always_on`: immediately). When `supervisor_port`/`listen_host` are unchanged the listener is kept and requests that arrive during the swap simply wait for it — none are refused. When the address changes, the old port is released and the new one bound. Activity leases survive the change. |
| **unchanged** | Untouched: the running process keeps running, its idle countdown and lease are not reset, in-flight requests and open WebSockets are unaffected. |

Wildcard discovery entries are diffed the same way, by name: an **unchanged** entry keeps its listener and every worktree project it has materialised (running ones keep running); a **changed** or **removed** entry has those projects stopped and dropped — they are listed under `removed` — and its listener closed, then rebuilt if the entry still exists (its worktrees come back on demand); an **added** entry binds its listener. A static project that shares a live worktree project's name takes it over as a change.

Rules and guarantees:

- **Invalid config → nothing changes.** If the config fails to load or validate (including a duplicate name across files or a malformed `projects.d` file), the reload is rejected, the validation errors are returned to the caller, and the daemon keeps running exactly as before.
- **One bad bind does not abort the rest.** If an added project's port is already taken, that project is reported in the reload's errors and skipped; every other change is applied. If a *changed* project's new port cannot be bound, the project keeps its previous port and settings (it is stopped at that point and cold-starts on demand) and the error is reported; fix the conflict and reload again — it is retried as "changed".
- **Reloads are serialized** with each other and with daemon shutdown, and a reload runs to completion even if the caller disconnects.
- Requests to unchanged projects never wait on a reload: the request path does not take the reload lock.
- The response (and the CLI output) lists the added / removed / changed / unchanged names and any errors; `herd-wake reload` exits non-zero when the reload was rejected or reported errors. `herd-wake status` shows the config path and when it was last reloaded successfully.

`herd-wake start` runs in the foreground; because SIGHUP now means "reload", closing the terminal that runs it no longer terminates the daemon (it only loses its log output). Stop it with Ctrl-C or SIGTERM.

## Worktrees: automatic URLs

If you work in git worktrees — one directory per branch or issue under a workspace folder — herd-wake can make `https://<worktree-name>.<repo>.test` live the moment the worktree exists, with nothing to register per branch and nothing to clean up when it goes. That is **wildcard mode**: one Herd proxy per repository, one listener, worktrees resolved from the URL on demand. The older **per-worktree mode** — one project, port, and Herd proxy per worktree, kept in sync by a folder watcher — remains available for setups that need a flat `https://<worktree>.test` URL or a separate port per worktree.

### Wildcard mode (recommended)

Add a `discovery:` entry with `mode: wildcard` to `config.yaml` (only the main file may hold it) — the full field reference is [above](#discovery-entries):

```yaml
discovery:
  - name: webapp
    mode: wildcard
    base_domain: webapp.test              # default: <name>.test
    supervisor_port: 41000                # ONE shared listener for the whole entry
    directory: ~/orca/workspaces/webapp   # <directory>/<label> is served at https://<label>.webapp.test
    repository: ~/dev/webapp              # only linked worktrees of this repo; unrelated repos in the folder are ignored
    require_files: [start.sh, package.json]
    command: ./start.sh
    port_command: node scripts/dev-port.mjs   # prints the port (or URL) the dev server will use
    application_port_range: [42000, 42999]    # used when port_command is absent
    env: { ENVIRONMENT: dev }
    node_path: ~/.nvm/versions/node/v24.15.0/bin
    rewrite_host: true
    readiness_strategy: tcp
    startup_timeout_seconds: 180
    idle_timeout_minutes: 30
```

Then, once:

```sh
herd-wake sync        # runs: herd proxy webapp http://127.0.0.1:41000 --secure
herd-wake start       # or your launchd job
```

Herd's proxy site file and certificate for `webapp.test` both cover `*.webapp.test`, so that single `herd proxy` is the last Herd command you need for this repository. From now on:

- `git worktree add ~/orca/workspaces/webapp/issue-3265 …` — and `https://issue-3265.webapp.test` works on its first request. No sync, no reload, no folder watcher, no nginx restart.
- Delete the worktree and the URL answers `404` on the next request; its dev server (if running) is stopped and the project forgotten. Re-create it and it comes back fresh.
- `herd-wake url` (from inside a worktree) prints its URL, so hooks and scripts never need to know the scheme.

### How a worktree is resolved

A request reaching the entry's listener with `Host: <label>.<base_domain>` — exactly one label, directly under the base domain; the base domain itself, `www.…`, and deeper names get a `404` — is resolved to `<directory>/<label>`. The directory must pass the entry's candidate rules: it exists, is not `exclude`d (dot-directories by default), is a git worktree or repository (has a `.git` file or directory), is a *linked* worktree of `repository` when that is set (its `.git` file points into `<repository>/.git/worktrees/`), and contains every `require_files` entry. Otherwise the request gets a `404` diagnostic saying which rule failed (`no worktree named "nope" under …: missing required file(s): start.sh`). Rules are re-checked on every unresolved request, so a worktree that appears (or is fixed) is served without any restart.

The first sight of a label **materialises a project in memory**: name `<label>`, `working_directory` the worktree, `host`/`public_url` `https://<label>.<base_domain>`, the template's fields, and an `application_port` from `port_command` (run in the worktree with the template's `env` and `node_path`, 15 s timeout) or the lowest free port of `application_port_range`. Resolution runs at most once per label at a time — concurrent first requests share it, so `port_command` runs once and one process starts — and requests arriving meanwhile are held under the template's `hold_max_wait_seconds`/`hold_max_requests`. A `port_command` failure, or a port some other live project already uses, is a `503` diagnostic with the reason, retried with the same backoff as a failed start.

From then on the project is ordinary: single-flight cold start, request holding, idle stop (it stays registered, keeping its port, across idle stops), WebSocket keep-alive, leases, and every `project:*` command and `logs` work with `<label>` as the name. `herd-wake status` lists it with source `discovery:webapp (dynamic)` and shows each wildcard entry's URL pattern and how many worktrees it has materialised. `project:restart <label>` re-runs `port_command` first and moves the project to the new port if it changed. Names must be unique across the daemon: a static project called `issue-1` takes precedence over a worktree of that name, which gets a `503` naming the conflict.

When a worktree directory disappears, the next request for its label is a `404`, a running dev server is stopped gracefully, and the project is dropped from the table once stopped (the idle monitor and `project:stop` check for the missing directory too). [Reloading](#reloading-the-configuration) keeps an unchanged entry's projects; a changed or removed entry stops and drops them.

Per-request cost on the shared listener is one map lookup on the lowercased `Host` plus, for a worktree project, one `stat` of its directory (that is how a vanished worktree is noticed); only the first request for a label pays for resolution.

### Per-worktree mode (the alternative)

Without `mode: wildcard` (or with `mode: proxy`), `herd-wake sync` is the master of the URL↔worktree registry: it scans the entry's directory, keeps one generated project per worktree in a managed `projects.d` file, allocates stable ports, creates and removes the matching Herd proxy entries, and reloads the daemon. Use it when worktrees need flat `https://<worktree>.test` URLs (or any `url_template`), or a supervisor port each.

```yaml
discovery:
  - name: webapp                          # managed projects go to projects.d/webapp.yaml
    directory: ~/orca/workspaces/webapp   # immediate subdirectories are candidates
    repository: ~/dev/webapp
    require_files: [start.sh, package.json]
    url_template: https://{name}.test     # default; {name} = directory name
    command: ./start.sh
    port_command: node scripts/dev-port.mjs
    supervisor_port_range: [41000, 41999]
    application_port_range: [42000, 42999]
    env: { ENVIRONMENT: dev }
    rewrite_host: true
    readiness_strategy: tcp
    idle_timeout_minutes: 30
```

The candidate rules are the same as in wildcard mode, applied to every subdirectory at sync time; rejected directories are listed with the reason (a directory such as `Feature_X` gets a warning telling you to rename it — names must be DNS labels). The generated project is the template's fields plus:

- `working_directory` — the worktree itself;
- `public_url` — `url_template` with `{name}` filled in;
- `supervisor_port` — the lowest port in `supervisor_port_range` not used anywhere in the configuration, allocated once and kept forever after (Herd's proxy points at it);
- `application_port` — what `port_command` printed (re-run every sync; a changed port updates the project), or the lowest free port in `application_port_range` when there is no `port_command`.

Each `herd proxy` restarts Herd's nginx, and the registry has to be re-synced whenever a worktree is created or removed — which is why wildcard mode is the recommended setup.

### Running sync

```sh
herd-wake sync                 # reconcile, update Herd, reload the daemon
herd-wake sync --dry-run       # show what would change; writes nothing, changes nothing in Herd
herd-wake sync --json          # machine-readable result (the same shape, plus the reload outcome)
herd-wake sync --no-herd       # never run the herd CLI; print the Herd commands to run by hand
```

For a **wildcard entry**, sync does exactly one thing for Herd: ensure `herd proxy <base name> http://127.0.0.1:<supervisor_port> --secure` exists (skipped when Herd's site file already proxies to that port; re-pointed when it proxies elsewhere), guarded by the [collision safeguard](#the-herd-collision-safeguard) on the base name. It writes no `projects.d` file — a `projects.d/<name>.yaml` left over from per-worktree mode is reported with instructions, never deleted — records the registration in `sync-state.yaml` next to the config, and reports `wildcard: https://<label>.<base_domain> → <directory>/<label>`. Removing the entry (or changing its `base_domain`) and syncing again runs `herd unproxy` for the old site.

For a **proxy-mode entry**, sync rewrites `projects.d/<name>.yaml` atomically from the current worktree set (the file opens with `# Managed by herd-wake sync — do not edit.`; sync refuses to overwrite a file without that header), then for each project runs `herd proxy <name> http://127.0.0.1:<supervisor_port> --secure` unless Herd's site file for the host (`~/Library/Application Support/Herd/config/valet/Nginx/<host>`) already proxies to that port, and for each removed project whose site file proxies to its port runs `herd unproxy <name>`.

In both modes the CLI is `herd` on `PATH`, else the bundled `~/Library/Application Support/Herd/bin/herd`; if it is missing, or a command fails, sync keeps going and prints the exact commands for you to run. If the daemon is running, sync then triggers [`reload`](#reloading-the-configuration) over the control socket and reports the result; otherwise it says so — the daemon reads the files when it starts.

The output lists, per entry, the mode and URL pattern (wildcard) or the projects added, updated, unchanged, and removed and every skipped directory with its reason (proxy mode), each Herd action and its outcome, and whether a managed file was written. `sync` exits non-zero when an entry could not be synced (an unreadable directory, an unmanaged file in the way, a refused base name) or the daemon rejected the reload; Herd problems are reported but never fail the command. Nothing in `config.yaml` is ever changed. A worktree whose name is already used by a hand-written project (or by another discovery entry) is skipped, not hijacked.

### Hooking into worktree tooling

In wildcard mode there is nothing to hook: creating a worktree makes its URL work, deleting it makes the URL go away. The one thing a hook may want is the URL itself — `herd-wake url` prints it for the current directory (or a given one) and exits 1 with the reason when the directory is not a servable worktree, so a `post-checkout` hook can print "open https://issue-3265.webapp.test" without knowing the scheme.

In per-worktree mode, run `herd-wake sync` after creating a worktree and after removing one — from a wrapper script, a `post-checkout` hook in the repository (`git worktree add` runs it in the new worktree), or whatever your worktree manager offers. It is idempotent: running it again with nothing changed writes nothing and runs no Herd command. Because removal has to run *before* the directory disappears if you want the Herd proxy cleaned up immediately, an archive hook is the natural place for `herd-wake project:remove <name>` (below); a plain `sync` afterwards also works, since sync loads the configuration with missing worktree directories tolerated.

### What happens on removal

**Wildcard mode:** delete the worktree. The next request for its label is a `404`, its dev server (if running) is stopped gracefully, and the project is dropped. Herd is untouched — the wildcard proxy serves every label. Removing the *entry* from the config and syncing unproxies the base name.

**Per-worktree mode:** delete a worktree and run `sync`: its project is removed from the managed file, `herd unproxy <name>` runs (only when Herd's site file still proxies to that project's supervisor port — a site file pointing elsewhere is left alone), and the daemon reload stops the dev server if it was running and closes its supervisor port. The ports it held are free for the next new worktree.

`herd-wake project:remove <name> [--keep-herd]` does the same for one project on demand, managed or hand-written in `projects.d`: it removes the project from its file (a hand-written `projects.d` file is rewritten without comments; the file stays, possibly with an empty `projects:` map), runs `herd unproxy` unless `--keep-herd`, and reloads the daemon. It is meant for worktree-archive hooks that run before the directory is gone. A name that is not registered is a no-op (exit 0), so hooks can call it unconditionally; a project defined in the main `config.yaml` is never touched — the command prints what to remove by hand and exits 1. Note that removing a *managed* project whose worktree still exists is temporary: the next `sync` re-creates it. (Worktree projects of a wildcard entry are not in any file, so `project:remove` reports them as unknown; `project:stop` is the command for those.)

### The Herd-collision safeguard

herd-wake must never claim a domain that already belongs to a PHP site (spec §11): a proxy entry for `accounts.test` would silently shadow a parked or linked Laravel app of that name. So before registering a new name — each worktree's name in per-worktree mode, the base name (`webapp` for `webapp.test`) in wildcard mode — sync refuses it, with a message naming the conflict and without writing anything or touching Herd, when:

- a directory called `<name>` exists directly inside any Herd parked path (`herd paths`), or
- `herd links` lists a site of that name, or
- Herd already has a site file for the host that is not a proxy (a site secured with `herd secure`).

`herd paths` and `herd links` are each called at most once per sync, and only when there is a proxy to create, however many names are checked. Rename the worktree (or the PHP site, or the entry's `base_domain`) to resolve a conflict. With `--no-herd`, or when the `herd` CLI is unavailable, the check cannot run; sync says so and still writes the projects — the printed `herd proxy` commands are yours to vet.

## CLI reference

```
herd-wake <command> [flags] [args]
```

| Command | What it does |
| --- | --- |
| `herd-wake start` | Run the supervisor daemon in the foreground: binds the project listeners (one per port; shared ports route by `Host`) and each wildcard entry's listener, serves the control API on the unix socket, starts `always_on` projects. Ctrl-C (or SIGTERM) stops the daemon *and* every dev server it started; SIGHUP reloads the configuration. |
| `herd-wake status` | Daemon PID/uptime/version, the config path and last reload time, each wildcard entry (URL pattern, directory, port, worktrees materialised), plus a per-project table: state, PID, uptime, last activity, scheduled idle stop (and what is holding it off), last exit, URL, ports (with the `host` on a shared port), and source (`config.yaml`, `projects.d/<file>`, or `discovery:<entry> (dynamic)`). |
| `herd-wake reload` | Re-read the config file and `projects.d` and apply the difference to the running daemon (see [Reloading the configuration](#reloading-the-configuration)). Prints added / removed / changed / unchanged projects; exits 1 if the config is invalid (nothing changes) or a project could not be applied. |
| `herd-wake sync` | Reconcile every `discovery:` entry with Herd: one wildcard proxy per wildcard entry; for proxy-mode entries, discover worktrees, rewrite the managed `projects.d/<name>.yaml` files, and create/remove per-worktree proxies. Then reload the daemon if it is running (see [Worktrees: automatic URLs](#worktrees-automatic-urls)). `--dry-run` prints without writing or touching Herd; `--no-herd` skips the Herd CLI and prints the commands; `--json` for scripting. Exits 1 when an entry failed or the reload was rejected. |
| `herd-wake url [directory]` | Print the public URL a directory (default: the current one) is served at: a registered project's `public_url` for its `working_directory`, or `https://<label>.<base_domain>` for a servable worktree under a wildcard entry. Exits 1 with the reason otherwise (not a candidate, or a per-worktree-mode worktree that has not been synced). Works without the daemon running. |
| `herd-wake projects` | List every registered project from the config file and `projects.d` (with each project's source file) and every wildcard entry (works without the daemon running). |
| `herd-wake project:start <name>` | Start a project's dev server and wait until it is ready. Bypasses and resets the failure backoff. Worktrees of a wildcard entry are addressed by their label once they have been served. |
| `herd-wake project:stop <name>` | Gracefully stop a project's dev server (signal, then force-kill after its shutdown timeout). A wildcard worktree whose directory is gone is dropped once stopped. |
| `herd-wake project:restart <name>` | Stop (if needed) and start a project's dev server. For a wildcard worktree, re-runs `port_command` first and moves the project to the new port if it changed. |
| `herd-wake project:lease <name>` | Mark a project active for `--ttl` (default 30m) so it is not idle-stopped — for tools that generate no HTTP traffic. Does not start a stopped project; a new lease replaces the old one. |
| `herd-wake project:release <name>` | Release a project's activity lease early. |
| `herd-wake project:remove <name>` | Remove a project from its `projects.d` file, run `herd unproxy` for it (unless `--keep-herd`), and reload the daemon. Unknown names are a no-op; projects in the main `config.yaml` are left for you to remove by hand (exit 1). |
| `herd-wake logs <name>` | Print the project's recent combined stdout/stderr and the path of the full on-disk log. |
| `herd-wake version` | Print the herd-wake version. |

Flags (place them before positional arguments):

| Flag | Applies to | Default | Meaning |
| --- | --- | --- | --- |
| `--config <path>` | `start`, `projects`, `sync`, `url`, `project:remove` | `~/Library/Application Support/herd-wake/config.yaml` | Config file to load; `projects.d/` next to it is merged in. |
| `--socket <path>` | `start`, `status`, `reload`, `sync`, `project:*`, `logs` | `~/Library/Application Support/herd-wake/herd-wake.sock` | Control socket the daemon serves / clients query. |
| `--log-dir <path>` | `start` | `~/Library/Application Support/herd-wake/logs` | Directory for per-project process logs (`<name>.log`). |
| `--ttl <duration>` | `project:lease` | `30m` | How long the lease lasts, e.g. `45m`, `2h`. |
| `--lines <n>` | `logs` | `0` | Maximum lines to print (0 = everything buffered, up to 200). |
| `--dry-run` | `sync` | off | Compute and print the changes without writing files or touching Herd (`herd paths`/`herd links` are still read). |
| `--no-herd` | `sync` | off | Never run the `herd` CLI; print the Herd commands to run by hand. |
| `--json` | `sync` | off | Print the result as JSON. |
| `--keep-herd` | `project:remove` | off | Leave the project's Herd proxy in place. |

The control API behind the CLI is plain HTTP+JSON over the unix socket, versioned under `/v1/`: `GET /v1/status` (includes `config_path`, `last_reload_at`, per-project `host`/`source`/`dynamic`, and `wildcards`), `POST /v1/reload` (returns `{applied, added, removed, changed, unchanged, wildcards, errors}`; `applied` is false when the config was rejected), `POST /v1/projects/{name}/start|stop|restart`, `POST /v1/projects/{name}/lease?ttl=45m`, `DELETE /v1/projects/{name}/lease`, `GET /v1/projects/{name}/logs?lines=N`. Wildcard worktrees are addressed by their label like any project.

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

**Where everything lives.** Config `~/Library/Application Support/herd-wake/config.yaml` plus `projects.d/*.yaml` next to it; control socket `~/Library/Application Support/herd-wake/herd-wake.sock`; process logs `~/Library/Application Support/herd-wake/logs/<name>.log` (also surfaced by `herd-wake logs <name>`, and quoted in 503 diagnostics). All overridable with `--config` / `--socket` / `--log-dir`.

**Cold start returns 503 "readiness … timeout".** The command started but never answered the readiness probe within `startup_timeout_seconds`. In rough order of likelihood:

1. *The server bound the wrong interface.* herd-wake probes and proxies `127.0.0.1`; Vite (and some other servers) bind only `localhost`/`::1` unless told otherwise. Fix the command: `--host 127.0.0.1`.
2. *The server picked a different port.* Without `--strictPort` (or equivalent), a dev server finding its port busy silently moves to another one, which herd-wake is not watching. Always pin and strict the port.
3. *It is genuinely slow* (big install step, cold cache): raise `startup_timeout_seconds`.
4. *The readiness URL is wrong* for apps that 404 on `/`: point `readiness_url` at a path that answers, or use `readiness_strategy: tcp`.

`herd-wake logs <name>` shows what the server actually printed.

**Port conflict.** If something else already holds `application_port`, a `--strictPort`-style command exits immediately and the 503 diagnostic shows the exit and its output. If something holds a `supervisor_port`, `herd-wake start` itself refuses to start with a bind error naming the project — pick a different port or stop the squatter (`lsof -nP -iTCP:<port>` shows who it is).

**Vite answers `403 Blocked request. This host ("dashboard.test") is not allowed. To allow this host, add "dashboard.test" to server.allowedHosts`.** Vite (since 5.4.12 / 6.0.9, and in 7/8) only accepts requests whose `Host` is localhost-ish or listed in `server.allowedHosts`; webpack-dev-server's `allowedHosts` behaves the same. herd-wake forwards the public `Host` by default so servers that build absolute URLs see the real origin. Two fixes — pick one:

1. Set `rewrite_host: true` on the project. Upstream then sees `Host: 127.0.0.1:<application_port>` (its own address, always allowed) while `X-Forwarded-Host: dashboard.test` and `X-Forwarded-Proto: https` still tell it the public origin; HMR upgrades get the same treatment. Then `herd-wake reload`.
2. Or allow the host in the dev server: `server: { allowedHosts: ['dashboard.test'] }` in `vite.config.js` (or `allowedHosts: 'all'`).

**`herd-wake reload` says the config is invalid.** Nothing was changed; the daemon keeps running with its previous configuration. The listed errors name the project, field, and — for a name defined twice — both files. Fix them and reload again. `herd-wake projects` validates the same way without touching the daemon.

**Reload reported an error for one project.** The rest of the reload was applied. A `listen on 127.0.0.1:<port>: address already in use` error for an *added* project means it was skipped; for a *changed* project it means the project kept its previous port. Free the port (or pick another) and reload again.

**503s keep coming after a failure.** That is the backoff, not a hang: after a failed start, request-triggered retries wait 1s, 2s, 4s, … capped at 30s, and requests during the wait get an immediate 503 saying when the next retry may run. `herd-wake project:start <name>` (or `project:restart`) retries immediately and resets the backoff.

**`herd-wake status` says the daemon is not running.** Start it with `herd-wake start`. If you used a custom `--socket` for the daemon, pass the same one to every other command.

**Stale socket / "another daemon is already running".** The daemon refuses to start while another daemon answers on the control socket (the error names its PID). A *stale* socket file — left by a crash, with nothing accepting on it — is detected and removed automatically. If the path exists but is not a socket at all, herd-wake asks you to move it out of the way rather than deleting it.

**Herd URL gives 502/504 but `curl 127.0.0.1:<supervisor_port>` works.** The Herd proxy target does not match the project's `supervisor_port` — re-register with `herd proxy <name> http://127.0.0.1:<supervisor_port> --secure`. If the direct curl fails too, the daemon is not running.

**A shared port answers `404 no project for host …`.** Nothing on that listener claims the hostname: check the project's `host` (it must match what the browser sends, case-insensitively, without the port) or, for a wildcard entry, that the hostname is exactly `<label>.<base_domain>` and the diagnostic's reason for the worktree (`missing required file(s)`, `standalone git repository`, `no directory named …`). `curl -H 'Host: x.webapp.test' http://127.0.0.1:<port>/` reproduces it without Herd.

**A worktree answers `503 … cannot be served`.** Its `port_command` failed (the output is quoted) or printed a port another running project already uses. Fix the worktree and retry after the backoff, or `herd-wake project:restart <label>` once it exists.

**`herd-wake url` exits 1 in a worktree that used to work.** The reason is printed: the directory fails a candidate rule, or (per-worktree mode) it has not been synced. In wildcard mode the URL is `https://<directory name>.<base_domain>` by construction.

**`sync` refused a worktree (or a wildcard base name): "a directory named … exists in Herd parked path …".** A parked or linked Herd site already answers on that name, and a proxy would shadow it — see [the safeguard](#the-herd-collision-safeguard). Rename the worktree (or the site, or the entry's `base_domain`). Nothing was written for it.

**`sync` reports a stale `projects.d/<name>.yaml` for a wildcard entry.** The file is left over from per-worktree mode; its projects (and their Herd proxies) are still registered alongside the wildcard listener. Delete the file, `herd unproxy` each worktree it listed, and reload — sync never deletes files it did not just write.

**`sync` skipped a worktree.** The reason is printed next to it: not a DNS label (rename `Feature_X` to `feature-x`), a standalone repository or a worktree of another repository when `repository` is set, a missing `require_files` entry, a failing `port_command` (its output is quoted; an already-registered worktree keeps its previous settings until it succeeds), or a name already taken by a hand-written project.

**`sync` says the herd CLI was not found.** It looked for `herd` on `PATH` and at `~/Library/Application Support/Herd/bin/herd`. The projects were still written and the daemon reloaded; run the printed `herd proxy`/`herd unproxy` commands yourself.

**`sync` refuses to overwrite `projects.d/<name>.yaml`.** A file of that name exists without the managed header — probably hand-written. Move it aside or rename the discovery entry; sync never clobbers files it did not write.

**Project stops while I'm still working.** Idle detection sees HTTP traffic and open WebSockets. Tools that make neither (editors, test watchers) can hold a project up with `herd-wake project:lease <name> --ttl 2h`, or set `always_on: true` for permanently-up projects, or raise `idle_timeout_minutes`.

**A project I stopped came back.** Any request to its URL wakes it — that is the point. Stop traffic (close the browser tab with the HMR socket) or unregister the Herd proxy if you want it to stay down.

## Development

Layout: `cmd/herd-wake` (CLI), `internal/config` (config load/validate, `projects.d` merge, `discovery:` schema, port-sharing rules), `internal/discovery` (worktree candidate rules, port allocation, managed-file writer, `sync`/`project:remove`, wildcard proxy state), `internal/herd` (Herd CLI wrapper: `paths`, `links`, site files, `proxy`/`unproxy`), `internal/daemon` (wiring, Host router and shared listeners, wildcard resolver, states, control-API provider, live reload), `internal/proxy` (reverse proxy, on-demand holding, WebSocket tunneling, diagnostic pages), `internal/process` (process-group supervisor, logs, backoff), `internal/idle` (activity tracking, idle monitor), `internal/control` (unix-socket HTTP API + client), `internal/testproc` (test-only child-process helpers, WebSocket test client, and the fake `herd` script the discovery tests run against — no test ever invokes a real Herd).

```sh
go test -race ./...                          # fast inner loop (e2e tests skip themselves)
HW_E2E=1 go test -race ./e2e/ -v -count=1    # full E2E acceptance suite
golangci-lint run
```

The E2E suite (`e2e/`) builds the real binary, runs the daemon as a subprocess against the Vite fixture in `testdata/vite-fixture`, and exercises the spec's acceptance criteria — cold start, single-flight under 20 concurrent requests, warm-request overhead, HMR-WebSocket keep-alive, idle stop and revival, two-project isolation, failure diagnostics, and no-auto-start after a daemon restart — purely through public surfaces (supervisor ports, CLI, control API). It is guarded by `HW_E2E=1` (and skips under `-short`), needs `node`/`npm` on `PATH`, and installs the fixture's pinned dependencies automatically (`npm ci`) on first run. CI runs it on `macos-latest`; the unit jobs stay on Linux.
