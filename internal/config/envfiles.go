package config

import (
	"fmt"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// EnvFiles is the list of env files a project reads its environment from,
// the value of a project's (or a discovery template's) env_file. It exists
// so secrets — a database password, an API token — live in files of their
// own, outside the config the user hands around, and are never rendered by
// herd-wake.
//
// The YAML form is either one path or a list of paths, so the common case
// (`env_file: ~/secrets/preview.env`) stays a one-liner while a preview box
// can layer a shared file and a per-worktree one
// (`env_file: [/etc/herd-wake/preview.env, .env.preview]`). Both forms
// decode to this slice, and it marshals back to the form it came in
// (scalar for one entry, a sequence for more), because `herd-wake sync`
// round-trips a Project through yaml.v3 when it writes a managed file and
// the result should read like something a human wrote.
//
// An entry is either absolute — a shared file, required to exist — or
// relative, resolved against the directory the command runs in and optional
// (see EnvFileRefs). Nothing here reads a file: the contents are read at
// every process start, so editing one needs no reload.
type EnvFiles []string

// UnmarshalYAML accepts either a single path or a sequence of paths. An
// explicit null (`env_file:` with nothing after it) is the same as leaving
// the field out.
func (f *EnvFiles) UnmarshalYAML(node *yaml.Node) error {
	switch {
	case node.Tag == "!!null":
		*f = nil
		return nil
	case node.Kind == yaml.ScalarNode:
		var one string
		if err := node.Decode(&one); err != nil {
			return err
		}
		*f = EnvFiles{one}
		return nil
	case node.Kind == yaml.SequenceNode:
		var many []string
		if err := node.Decode(&many); err != nil {
			return err
		}
		*f = EnvFiles(many)
		return nil
	default:
		return fmt.Errorf("line %d: env_file must be a path or a list of paths", node.Line)
	}
}

// MarshalYAML writes a single entry back as a scalar and several as a
// sequence, so a managed file keeps the shape its template had.
func (f EnvFiles) MarshalYAML() (any, error) {
	if len(f) == 1 {
		return f[0], nil
	}
	return []string(f), nil
}

// copyEnvFiles returns a copy of the list (nil for an empty one) so a
// template and the projects built from it never share a backing array.
func copyEnvFiles(files EnvFiles) EnvFiles {
	if len(files) == 0 {
		return nil
	}
	return append(EnvFiles(nil), files...)
}

// EnvFileRef is one env file to read at process start, resolved to an
// absolute path.
type EnvFileRef struct {
	// Path is the absolute path of the file.
	Path string
	// Optional reports whether a missing file is fine. A per-worktree file
	// is optional — most worktrees have none — whereas a shared file that
	// has vanished is a configuration error worth failing the start for.
	Optional bool
}

// EnvFileRefs resolves the project's env_file entries against dir, in the
// order they were configured (later files override earlier ones when they
// are read). An absolute entry is taken as it stands and is required; a
// relative entry is joined onto dir and is optional.
//
// dir is supplied by the caller rather than taken from the project because
// the callers differ: a process start resolves against the project's
// working directory, a port_command run against the worktree it runs in,
// and a discovery template against a candidate worktree that has no project
// yet.
func (p *Project) EnvFileRefs(dir string) []EnvFileRef {
	if len(p.EnvFile) == 0 {
		return nil
	}
	refs := make([]EnvFileRef, 0, len(p.EnvFile))
	for _, entry := range p.EnvFile {
		if filepath.IsAbs(entry) {
			refs = append(refs, EnvFileRef{Path: entry})
			continue
		}
		refs = append(refs, EnvFileRef{Path: filepath.Join(dir, entry), Optional: true})
	}
	return refs
}

// DiagnosticLogsEnabled reports whether herd-wake's 404 and 503 diagnostics
// may quote this project's process output and filesystem paths. Unset means
// true: only a project that opted out with diagnostic_logs: false — a
// preview server whose URL is reachable from the internet — gets the terse
// pages.
func (p *Project) DiagnosticLogsEnabled() bool {
	return p.DiagnosticLogs == nil || *p.DiagnosticLogs
}
