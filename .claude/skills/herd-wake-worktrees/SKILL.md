---
name: herd-wake-worktrees
description: Give every git worktree of a Node.js repo its own on-demand https://<worktree>.test URL through Laravel Herd using herd-wake — install the binary, write a discovery entry, run the daemon and a folder watcher under launchd, sync, and hook the repo's worktree tooling. Use when the user wants Herd URLs for Node worktrees, wants to set up herd-wake on a machine, asks to "wake" dev servers on demand, or is adding a second repo/workspace folder to an existing herd-wake install.
---

# herd-wake for worktrees

Goal: a workspace folder full of git worktrees (one per branch, e.g. Orca's
`~/orca/workspaces/<repo>/<branch>`) where each worktree answers at
`https://<worktree-dir>.test`, the dev server starts on the first request and
stops when idle, and PHP sites already served by Herd are untouched.

herd-wake is the supervisor in this repo (README: "Worktrees: automatic URLs").
Herd keeps DNS, ports 80/443 and TLS; herd-wake only receives the URLs that
`herd proxy` explicitly points at it. **Never `herd park` the workspace folder** —
that is what would turn worktrees into (broken) PHP sites.

## 0. Learn the repo's dev server first

Before writing any config, answer these for the target repo (read its README,
`package.json` scripts, dev launcher, vite config):

| Question | Why it matters | Sing Your Part `webapp` answer |
|---|---|---|
| What command runs the dev server **in the foreground**, in its own process tree? | herd-wake owns that process group; a launcher that daemonises and exits (like `sypdev.mjs start`) cannot be supervised | `./start.sh` (concurrently: server, vite, watchers) |
| Which port does the UI listen on, per worktree? | becomes `application_port`; must be deterministic or derivable | `node scripts/sypdev.mjs url` prints `http://localhost:<issue-number>` → `port_command` |
| Does the dev server reject unknown `Host` headers? | Vite ≥5.4.12/6.0.9/8 answers `403 Blocked request` for `<name>.test` | yes → `rewrite_host: true` |
| Env vars the launcher expects? | copied into every generated project | `ENVIRONMENT=dev`, `SUPPRESS_DB_TESTS=1` |
| Where is node? | launchd has no shell PATH | `node_path: ~/.nvm/versions/node/<ver>/bin` |
| Are there non-worktree repos in the same folder? | they must not become projects | yes (data repos) → `repository:` + `require_files:` |
| Does the client build absolute `http://localhost:<port>` or `ws://` URLs? | breaks under an https URL (mixed content); needs an app fix | dev WebSocket did → fixed in crescendosw/webapp#3452 |
| Existing per-worktree "always on" servers? | they hold the ports; stop them before herd-wake takes over | `node scripts/sypdev.mjs stop` per worktree |

Verify the Host question empirically: start the dev server by hand and
`curl -H 'Host: x.test' http://127.0.0.1:<port>/` — 403 means `rewrite_host`.

## 1. Install the binary

```sh
cd <herd-wake checkout>
go build -ldflags "-X github.com/michael-hewitt/herd-wake/internal/version.version=$(git describe --always)" -o ~/.local/bin/herd-wake ./cmd/herd-wake
herd-wake version
```

`~/.local/bin` must be on PATH (it is for this user). Re-run this to upgrade;
then `launchctl kickstart -k gui/$(id -u)/us.hewitts.herd-wake` restarts the daemon
(it stops every dev server it owns; they come back on demand).

## 2. Config: one discovery entry per workspace folder

`~/Library/Application Support/herd-wake/config.yaml` (hand-written; `sync`
never rewrites it — generated projects go to `projects.d/<name>.yaml`):

```yaml
projects: {}

discovery:
  - name: webapp
    directory: ~/orca/workspaces/webapp
    repository: ~/dev/crescendo/webapp          # only linked worktrees of this repo
    require_files: [start.sh, package.json]
    url_template: https://{name}.test
    command: ./start.sh
    port_command: node scripts/sypdev.mjs url
    supervisor_port_range: [41000, 41999]         # pick a range per discovery entry
    env: { ENVIRONMENT: dev, SUPPRESS_DB_TESTS: "1" }
    node_path: /Users/<you>/.nvm/versions/node/v24.15.0/bin
    rewrite_host: true
    readiness_strategy: http
    startup_timeout_seconds: 180
    shutdown_timeout_seconds: 15
    idle_timeout_minutes: 30
    herd: true
```

A second repo is another list entry with its own `name`, `directory`, and a
disjoint `supervisor_port_range`. Validate with `herd-wake projects`, preview
with `herd-wake sync --dry-run` — it must list the worktrees you expect under
`added` and the stray repos under `skipped`.

## 3. Run the daemon and a folder watcher under launchd

Templates are in `launchd/` next to this file; replace `__HOME__`,
`__NODE_BIN__` (nvm bin dir) and `__WATCH_DIR__` (the workspace folder).

```sh
mkdir -p ~/Library/Logs/herd-wake
for f in us.hewitts.herd-wake us.hewitts.herd-wake-sync; do
  sed -e "s|__HOME__|$HOME|g" -e "s|__NODE_BIN__|$HOME/.nvm/versions/node/v24.15.0/bin|g" \
      -e "s|__WATCH_DIR__|$HOME/orca/workspaces/webapp|g" launchd/$f.plist > ~/Library/LaunchAgents/$f.plist
  plutil -lint ~/Library/LaunchAgents/$f.plist
  launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/$f.plist
done
herd-wake status          # "daemon running"
```

- `us.hewitts.herd-wake` — `herd-wake start` at login, KeepAlive, logs to
  `~/Library/Logs/herd-wake/daemon.log`.
- `us.hewitts.herd-wake-sync` — `WatchPaths` on the workspace folder: any
  worktree created or deleted (by Orca, `git worktree`, Finder…) runs
  `herd-wake sync` within seconds, so registration does not depend on repo hooks.
  Add one `<string>` per watched folder. Log: `sync.log`.

## 4. Take over existing worktrees

1. Stop any always-on servers holding the application ports (for webapp:
   `node scripts/sypdev.mjs stop` inside each worktree; check with
   `lsof -nP -iTCP:<port> -sTCP:LISTEN`).
2. `herd-wake sync` — writes `projects.d/<name>.yaml`, runs
   `herd proxy <worktree> http://127.0.0.1:<supervisor_port> --secure` per
   worktree (each restarts Herd's nginx for a moment) and live-reloads the daemon.
3. Prove it: `curl -w '%{http_code} %{time_total}s\n' https://<worktree>.test/`
   (first request cold-starts; webapp takes ~7 s), then `herd-wake status`.
4. Prove PHP is untouched: `herd paths` unchanged; curl a parked PHP site.

## 5. Hook the repo's worktree tooling (optional but recommended)

The watcher already handles create/delete on this machine. A repo-side hook makes
the flow explicit and works for teammates too. For Orca (`orca.yaml`, honoured
when the repo's Command Source is "orca.yaml only"):

```yaml
scripts:
  setup: |
    …npm install…
    if command -v herd-wake >/dev/null 2>&1; then herd-wake sync; else npm start; fi
  archive: |
    if command -v herd-wake >/dev/null 2>&1; then herd-wake project:remove "$(basename "$PWD")" || true; fi
```

Reference: crescendosw/webapp#3452 (also fixes the client's dev WebSocket URL for
https and adds `sypdev.mjs public-url`, which prints the Herd URL).

## Day-to-day

```sh
herd-wake status                       # states, PIDs, idle-stop times
herd-wake logs <worktree>              # recent dev-server output
herd-wake project:restart <worktree>   # after changing deps/env
herd-wake project:lease <worktree> --ttl 2h   # keep it up without traffic
herd-wake sync --dry-run               # what would change
tail -f ~/Library/Logs/herd-wake/{daemon,sync}.log
```

## Troubleshooting

- **403 "Blocked request… allowedHosts"** → `rewrite_host: true` on the entry, `herd-wake sync`.
- **SSL error on the URL** → the proxy was registered without `--secure`; `herd unproxy <name>` then `herd-wake sync`.
- **503 with "address already in use"** → an old always-on server still holds the port; stop it (step 4.1), then reload the page.
- **Readiness timeout** → run the `command` by hand in the worktree; missing `node_modules` is the usual cause after a fresh worktree (the hook's `npm install` may still be running).
- **A repo in the folder got a URL it should not have** → tighten `repository`/`require_files`/`exclude`, `herd-wake sync` removes it and unproxies.

## Uninstall

```sh
launchctl bootout gui/$(id -u)/us.hewitts.herd-wake-sync; launchctl bootout gui/$(id -u)/us.hewitts.herd-wake
for n in $(herd-wake projects | awk 'NR>1 && /^[a-z0-9-]+$/'); do herd unproxy "$n"; done   # or: herd-wake project:remove <name> per project while the daemon runs
rm -rf ~/Library/LaunchAgents/us.hewitts.herd-wake*.plist "~/Library/Application Support/herd-wake" ~/.local/bin/herd-wake
```

PHP sites keep working throughout: herd-wake never parked, linked, or secured anything of its own.
