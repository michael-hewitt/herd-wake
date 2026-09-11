---
name: herd-wake-worktrees
description: Give every git worktree of a Node.js repo its own on-demand https://<worktree>.<repo>.test URL through Laravel Herd using herd-wake's wildcard discovery — install the binary, write one discovery entry, run the daemon under launchd, one `herd proxy`, done. Use when the user wants Herd URLs for Node worktrees, wants to set up herd-wake on a machine, asks to "wake" dev servers on demand, is adding a second repo/workspace folder, or is migrating from per-worktree proxies.
---

# herd-wake for worktrees (wildcard mode)

Goal: a workspace folder full of git worktrees (one per branch, e.g. Orca's
`~/orca/workspaces/<repo>/<branch>`) where each worktree answers at
`https://<worktree-dir>.<repo>.test`, the dev server starts on the first request
and stops when idle, nothing is registered per worktree, and PHP sites already
served by Herd are untouched.

How it works: Herd proxy entries are wildcards — one `herd proxy webapp … --secure`
gives nginx `server_name *.webapp.test` and a certificate for `*.webapp.test`. Herd
forwards everything under it to one herd-wake listener; herd-wake maps the `Host`
header to `<directory>/<label>`, checks it is a real worktree, and wakes it.
(README: "Worktrees: automatic URLs".) **Never `herd park` the workspace folder** —
parked directories are routed to PHP-FPM, which is what would break Node worktrees.

## 0. Learn the repo's dev server first

Answer these for the target repo before writing config (read its README,
`package.json` scripts, dev launcher, vite config):

| Question | Why it matters | Sing Your Part `webapp` answer |
|---|---|---|
| What command runs the dev server **in the foreground**, in its own process tree? | herd-wake owns that process group; a launcher that daemonises and exits (like `sypdev.mjs start`) cannot be supervised | `./start.sh` (concurrently: server + watchers) |
| Which port does the UI listen on, per worktree? | becomes `application_port`; must be deterministic or derivable | `node scripts/sypdev.mjs url` prints `http://localhost:<issue-number>` → `port_command` |
| Does the dev server reject unknown `Host` headers? | Vite ≥5.4.12/6.0.9/8 answers `403 Blocked request` for `<name>.webapp.test`; Express does not care | worktrees on Vite branches do → `rewrite_host: true` (harmless otherwise — keep it on) |
| Env vars the launcher expects? | copied into every materialised project | `ENVIRONMENT=dev`, `SUPPRESS_DB_TESTS=1` |
| Where is node? | launchd has no shell PATH | `node_path: ~/.nvm/versions/node/<ver>/bin` |
| Are there non-worktree repos in the same folder? | they must not become projects | yes (data repos) → `repository:` + `require_files:` |
| Does the client build absolute `http://localhost:<port>` / `ws://` URLs? | breaks under an https URL (mixed content); needs an app fix | dev WebSocket did → fixed on webapp `main`; older branches still show the splash screen until they merge main |
| Existing per-worktree "always on" servers? | they hold the ports; stop them before herd-wake takes over | `node scripts/sypdev.mjs stop` per worktree |
| Does the repo's tooling ever start a server itself (`npm start`, `preview`)? | it would fight herd-wake for the port | crescendosw/webapp#3452 makes `preview` ask `herd-wake url` first |

Verify the Host question empirically: start the dev server by hand and
`curl -H 'Host: x.webapp.test' http://127.0.0.1:<port>/` — 403 means `rewrite_host`.

## 1. Install the binary

```sh
cd <herd-wake checkout>   # main
go build -ldflags "-X github.com/michael-hewitt/herd-wake/internal/version.version=$(git describe --always)" -o ~/.local/bin/herd-wake ./cmd/herd-wake
herd-wake version
```

`~/.local/bin` must be on PATH. Re-run this to upgrade, then
`launchctl kickstart -k gui/$(id -u)/us.hewitts.herd-wake` restarts the daemon
(it stops every dev server it owns; they come back on demand).

## 2. Config: one wildcard discovery entry per workspace folder

`~/Library/Application Support/herd-wake/config.yaml` (hand-written; nothing else writes it):

```yaml
projects: {}

discovery:
  - name: webapp
    mode: wildcard
    base_domain: webapp.test                 # default: <name>.test
    supervisor_port: 41000                   # the one shared listener; pick a distinct port per entry
    directory: ~/orca/workspaces/webapp
    repository: ~/dev/crescendo/webapp       # only linked worktrees of this repo
    require_files: [start.sh, package.json]
    command: ./start.sh
    port_command: node scripts/sypdev.mjs url
    env: { ENVIRONMENT: dev, SUPPRESS_DB_TESTS: "1" }
    node_path: /Users/<you>/.nvm/versions/node/v24.15.0/bin
    rewrite_host: true
    readiness_strategy: http
    startup_timeout_seconds: 180
    shutdown_timeout_seconds: 15
    idle_timeout_minutes: 30
    herd: true
```

A second repo is another list entry with its own `name`/`base_domain`, `directory`
and `supervisor_port`. Validate with `herd-wake projects`; `herd-wake url <worktree-dir>`
must print the URL for a real worktree and refuse a stray repo with the rule that failed.

## 3. Run the daemon under launchd

Template in `launchd/` next to this file; replace `__HOME__` and `__NODE_BIN__`:

```sh
mkdir -p ~/Library/Logs/herd-wake
sed -e "s|__HOME__|$HOME|g" -e "s|__NODE_BIN__|$HOME/.nvm/versions/node/v24.15.0/bin|g" \
    launchd/us.hewitts.herd-wake.plist > ~/Library/LaunchAgents/us.hewitts.herd-wake.plist
plutil -lint ~/Library/LaunchAgents/us.hewitts.herd-wake.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/us.hewitts.herd-wake.plist
herd-wake status          # "daemon running", wildcard line, 0 projects materialised
```

`herd-wake start` at login, KeepAlive, logs to `~/Library/Logs/herd-wake/daemon.log`.
No folder watcher is needed in wildcard mode: worktrees are resolved from the URL.

## 4. Register with Herd and prove it

1. Stop any always-on servers holding the application ports
   (`node scripts/sypdev.mjs stop` inside each worktree; check `lsof -nP -iTCP:<port> -sTCP:LISTEN`).
2. `herd-wake sync` — runs the single `herd proxy webapp http://127.0.0.1:41000 --secure`
   (one nginx restart; wait ~3 s before curling) and live-reloads the daemon.
3. `curl -w '%{http_code} %{time_total}s\n' https://<worktree>.webapp.test/` — first request
   cold-starts (webapp: 4–9 s), then `herd-wake status` shows it `running` with source
   `discovery:webapp (dynamic)`.
4. `https://nope.webapp.test/` → 404 diagnostic; a data repo in the folder → 404.
5. PHP untouched: `herd paths` unchanged; curl a parked PHP site.

## Migrating from per-worktree proxies (the older `mode: proxy` setup)

While the old daemon still runs: `herd-wake project:remove <name>` for every name in
`projects.d/<entry>.yaml` (each unproxies + reloads), `launchctl bootout gui/$(id -u)/us.hewitts.herd-wake-sync`
and delete its plist, delete the emptied `projects.d/<entry>.yaml`, then steps 1–4 above.

## Day-to-day

```sh
herd-wake status                        # states, PIDs, idle-stop times, materialised count
herd-wake url .                         # the URL for the current worktree
herd-wake logs <worktree>               # recent dev-server output
herd-wake project:restart <worktree>    # re-runs port_command and restarts
herd-wake project:lease <worktree> --ttl 2h   # keep it up without traffic
tail -f ~/Library/Logs/herd-wake/daemon.log
```

## Troubleshooting

- **403 "Blocked request… allowedHosts"** → `rewrite_host: true` on the entry, `herd-wake reload`.
- **App stuck on its splash screen under https** → app-side: the branch builds `ws://` URLs from an https page; merge main (webapp has the fix).
- **"Couldn't connect" right after `sync`** → Herd's nginx is restarting; retry in a few seconds.
- **404 for a real worktree** → `herd-wake url <dir>` prints which rule failed (`repository`, `require_files`, `exclude`).
- **503 "address already in use"** → an old always-on server holds the port; stop it, reload the page.
- **Readiness timeout** → run the `command` by hand in the worktree; missing `node_modules` after a fresh worktree is the usual cause.

## Uninstall

```sh
launchctl bootout gui/$(id -u)/us.hewitts.herd-wake
herd unproxy webapp                     # per wildcard entry
rm -f ~/Library/LaunchAgents/us.hewitts.herd-wake.plist ~/.local/bin/herd-wake
rm -rf "$HOME/Library/Application Support/herd-wake" ~/Library/Logs/herd-wake
```

PHP sites keep working throughout: herd-wake never parked, linked, or secured anything of its own.
