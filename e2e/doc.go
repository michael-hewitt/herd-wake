// Package e2e is the herd-wake end-to-end acceptance suite. It builds the
// real herd-wake binary, runs the daemon as a subprocess against the Vite
// fixture in testdata/vite-fixture, and drives it purely through its public
// surfaces: the per-project supervisor ports, the CLI, and the documented
// control API on the unix socket.
//
// The suite needs node and npm on PATH and starts real dev servers, so it is
// guarded: every test skips unless the environment variable HW_E2E=1 is set
// (they also skip under `go test -short`). Run it with:
//
//	HW_E2E=1 go test -race ./e2e/ -v -count=1
//
// The fixture's dependencies are installed automatically (`npm ci` in
// testdata/vite-fixture) when node_modules is missing.
package e2e
