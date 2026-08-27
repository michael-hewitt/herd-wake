// Package version exposes build-time version information for herd-wake.
package version

// version is the herd-wake version string. It defaults to "dev" and can be
// overridden at build time with:
//
//	go build -ldflags "-X github.com/michael-hewitt/herd-wake/internal/version.version=v1.2.3" ./cmd/herd-wake
var version = "dev"

// String returns the current herd-wake version.
func String() string {
	return version
}
