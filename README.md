# herd-wake

herd-wake is a lightweight local lifecycle supervisor that works alongside [Laravel Herd](https://herd.laravel.com) to run Node.js development servers only while they are actively needed: it starts a registered Node.js dev server when its Herd URL receives a request, holds the triggering request until the server is ready, proxies HTTP and WebSocket traffic (including Vite HMR), and stops the server again after a configurable period of inactivity — all without changing or disrupting Herd's normal PHP behavior.

See the full specification in [issue #1](https://github.com/michael-hewitt/herd-wake/issues/1).

## Usage

```sh
go build ./cmd/herd-wake
./herd-wake version
```
