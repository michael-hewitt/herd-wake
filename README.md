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
`application_port` (for now the dev server must be started manually — process
management arrives in a later slice). The daemon serves a control API on a
unix socket at `~/Library/Application Support/herd-wake/herd-wake.sock`,
which `herd-wake status` queries. Stop the daemon with Ctrl-C.

## Configuration

Projects are registered in a user-level config file at
`~/Library/Application Support/herd-wake/config.yaml` — nothing is stored in
your project repositories. See [config.sample.yaml](config.sample.yaml) for a
fully documented example covering a Node-only app and a Laravel Vite server.
