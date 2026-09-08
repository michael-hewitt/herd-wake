package testproc

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FakeHerd describes the fake `herd` shell script WriteFakeHerd generates
// for tests. The script logs every invocation (one line of arguments per
// call) to LogFile and implements the subcommands herd-wake uses:
//
//   - paths: prints Parked as a JSON array
//   - links: prints a Symfony-style table listing Links
//   - proxy <name> <target> --secure: writes <NginxDir>/<name>.test with a
//     proxy_pass to the target (like Herd's real site file)
//   - unproxy <name>: removes that file
//
// Fail lists subcommands that exit 1 instead (to test degraded paths).
type FakeHerd struct {
	LogFile  string
	NginxDir string
	Parked   []string
	Links    []string
	Fail     []string
}

// WriteFakeHerd writes the fake script as <dir>/herd and returns its path.
// Put dir first on PATH, or pass the path as the explicit binary.
func WriteFakeHerd(dir string, f FakeHerd) (string, error) {
	if err := os.MkdirAll(f.NginxDir, 0o755); err != nil {
		return "", err
	}
	quoted := make([]string, 0, len(f.Parked))
	for _, p := range f.Parked {
		quoted = append(quoted, fmt.Sprintf("  %q", p))
	}
	var links strings.Builder
	links.WriteString("+------+---------+-----+------+\\n| Site | Secured | URL | Path |\\n+------+---------+-----+------+\\n")
	for _, l := range f.Links {
		fmt.Fprintf(&links, "| %s | X | https://%s.test | /tmp/%s |\\n", l, l, l)
	}
	links.WriteString("+------+---------+-----+------+\\n")

	var fail strings.Builder
	for _, sub := range f.Fail {
		fmt.Fprintf(&fail, "if [ \"$1\" = %q ]; then echo \"fake herd: %s failed\" >&2; exit 1; fi\n", sub, sub)
	}

	script := fmt.Sprintf(`#!/bin/sh
# fake herd for herd-wake tests
printf '%%s\n' "$*" >> %q
%s
case "$1" in
  paths)
    printf '[\n%s\n]\n'
    ;;
  links)
    printf '%s'
    ;;
  proxy)
    name="$2"; target="$3"
    printf 'server {\n    listen 127.0.0.1:443 ssl;\n    server_name %%s.test www.%%s.test *.%%s.test;\n    location / {\n        proxy_pass %%s;\n        proxy_set_header Host $host;\n    }\n}\n' "$name" "$name" "$name" "$target" > %q/"$name.test"
    ;;
  unproxy)
    rm -f %q/"$2.test"
    ;;
  *)
    echo "fake herd: unknown command $1" >&2
    exit 1
    ;;
esac
`, f.LogFile, fail.String(), strings.Join(quoted, ",\\n"), links.String(), f.NginxDir, f.NginxDir)

	path := filepath.Join(dir, "herd")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { //nolint:gosec // an executable test script
		return "", err
	}
	return path, nil
}

// ReadFakeHerdLog returns the fake script's logged invocations, one
// argument line per call, oldest first. A missing log means no calls.
func ReadFakeHerdLog(logFile string) []string {
	data, err := os.ReadFile(logFile)
	if err != nil {
		return nil
	}
	var calls []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			calls = append(calls, line)
		}
	}
	return calls
}
