// Package envfile turns a project's configuration into the environment its
// child processes run with, and owns the one rule that says which variable
// names are secret.
//
// Two code paths spawn something on a project's behalf — the process
// supervisor starting the dev server, and the discovery package running a
// port_command in a worktree — and a port_command that cannot see the same
// variables as the server it is probing for is a bug waiting to happen. So
// the whole child environment is built in exactly one place (ChildEnv),
// including reading the project's env_file list.
//
// Env files are read at every start, never cached: rotating a password is a
// file edit, not a reload. Their contents are secrets, so nothing in this
// package ever renders a value — not in an error, not in a warning. A parse
// failure names the file and the line number and says what is wrong with
// it, and that is all. Callers that must show an environment (the projects
// listing, for one) show the inline env map only, through Display, which
// masks the values whose names look sensitive.
package envfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/michael-hewitt/herd-wake/internal/config"
)

// Var is one environment variable read from an env file. It is kept as a
// name and a value rather than a map so the order the files gave is
// preserved all the way to the child's environment, which is what makes
// precedence between files predictable.
type Var struct {
	// Name is a shell identifier: [A-Za-z_][A-Za-z0-9_]*.
	Name string
	// Value is the value after quote and escape processing. It is a secret
	// as far as this package is concerned and is never rendered.
	Value string
}

// validName is the shell-identifier rule every name in an env file must
// satisfy. Anything looser would produce a variable the dev server's shell
// could not name anyway.
var validName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Parse reads the dotenv subset herd-wake documents and returns the
// variables in file order, with each name appearing once: a name repeated
// later in the file keeps its last value, at its last position.
//
// The subset is deliberately small, because an env file is read by a
// daemon that must not surprise anyone:
//
//   - one NAME=value per line, with an optional "export " prefix;
//   - blank lines, and lines whose first non-space character is #, are
//     ignored; there is no inline-comment rule, so a # inside a value is an
//     ordinary character and a value containing one needs no quoting;
//   - a value wrapped in matching single quotes is taken literally between
//     the quotes; one wrapped in matching double quotes honours \n, \" and
//     \\, and keeps any other backslash sequence verbatim;
//   - an unquoted value is trimmed of surrounding whitespace and is
//     otherwise verbatim;
//   - CRLF line endings are tolerated and a leading UTF-8 BOM is skipped,
//     so a file written on another platform or by an editor that insists on
//     a BOM still loads.
//
// A malformed line yields an error of the form "line N: <what is wrong>".
// The message never contains any part of the line, because the line is
// where the password is.
func Parse(data []byte) ([]Var, error) {
	text := strings.TrimPrefix(string(data), "\uFEFF")
	var vars []Var
	for i, raw := range strings.Split(text, "\n") {
		lineNo := i + 1
		line := strings.TrimSuffix(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		v, err := parseLine(trimmed)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		vars = override(vars, v)
	}
	return vars, nil
}

// parseLine parses one already-trimmed, non-empty, non-comment line. Its
// errors describe the fault and nothing else; the caller adds the line
// number.
func parseLine(line string) (Var, error) {
	if rest, ok := cutExport(line); ok {
		line = rest
	}
	name, value, found := strings.Cut(line, "=")
	if !found {
		return Var{}, errors.New("no '=' in line")
	}
	name = strings.TrimSpace(name)
	if !validName.MatchString(name) {
		return Var{}, errors.New("variable name is not a shell identifier")
	}
	parsed, err := parseValue(strings.TrimSpace(value))
	if err != nil {
		return Var{}, err
	}
	return Var{Name: name, Value: parsed}, nil
}

// cutExport strips a leading "export " (with any run of spaces or tabs
// after the keyword), so a file that doubles as something you can `source`
// parses the same way here.
func cutExport(line string) (string, bool) {
	rest, ok := strings.CutPrefix(line, "export")
	if !ok || rest == "" || (rest[0] != ' ' && rest[0] != '\t') {
		return line, false
	}
	return strings.TrimLeft(rest, " \t"), true
}

// parseValue applies the quoting rules to the part after the '=', which the
// caller has already trimmed.
func parseValue(value string) (string, error) {
	switch {
	case strings.HasPrefix(value, "'"):
		body, rest, found := strings.Cut(value[1:], "'")
		if !found {
			return "", errors.New("unterminated single-quoted value")
		}
		if strings.TrimSpace(rest) != "" {
			return "", errors.New("trailing text after closing quote")
		}
		return body, nil
	case strings.HasPrefix(value, `"`):
		return parseDoubleQuoted(value[1:])
	default:
		return value, nil
	}
}

// parseDoubleQuoted reads a double-quoted value from body, which starts
// just after the opening quote. Only \n, \" and \\ mean anything; every
// other backslash sequence is kept as written, so a Windows path or a
// regular expression survives a round trip.
func parseDoubleQuoted(body string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		switch c := body[i]; c {
		case '"':
			if strings.TrimSpace(body[i+1:]) != "" {
				return "", errors.New("trailing text after closing quote")
			}
			return b.String(), nil
		case '\\':
			if i+1 >= len(body) {
				return "", errors.New("unterminated double-quoted value")
			}
			i++
			switch body[i] {
			case 'n':
				b.WriteByte('\n')
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			default:
				b.WriteByte('\\')
				b.WriteByte(body[i])
			}
		default:
			b.WriteByte(c)
		}
	}
	return "", errors.New("unterminated double-quoted value")
}

// override appends v, dropping any earlier variable of the same name, so
// the list keeps one entry per name and the last definition wins — both
// within a file and across the files Load reads.
func override(vars []Var, v Var) []Var {
	for i := range vars {
		if vars[i].Name == v.Name {
			vars = append(vars[:i], vars[i+1:]...)
			break
		}
	}
	return append(vars, v)
}

// Load reads the env files in refs, in order, and returns their variables
// merged into one list: one entry per name, a later file beating an earlier
// one, in the position of the winning definition. Applying the list in
// order therefore gives the documented precedence whichever way the caller
// applies it.
//
// A ref marked Optional that does not exist is skipped — most worktrees
// have no per-worktree file. A required file that is missing or unreadable
// is an error naming the path, because a shared secrets file that has
// vanished is exactly the thing an operator wants to hear about loudly. A
// parse failure is wrapped as "env_file <path>: line N: ...".
//
// The warnings are advisory and never a refusal: one per file whose mode
// grants read access to group or other, telling the operator to chmod it.
// They are returned rather than logged so the caller decides where they go;
// they contain a path and a mode, never a value.
func Load(refs []config.EnvFileRef) ([]Var, []string, error) {
	var (
		vars     []Var
		warnings []string
	)
	for _, ref := range refs {
		data, err := os.ReadFile(ref.Path)
		if err != nil {
			if ref.Optional && errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, warnings, fmt.Errorf("env_file %s: %w", ref.Path, err)
		}
		if w, ok := permissionWarning(ref.Path); ok {
			warnings = append(warnings, w)
		}
		parsed, err := Parse(data)
		if err != nil {
			return nil, warnings, fmt.Errorf("env_file %s: %w", ref.Path, err)
		}
		for _, v := range parsed {
			vars = override(vars, v)
		}
	}
	return vars, warnings, nil
}

// permissionWarning reports whether the file at path is readable by anyone
// but its owner, and the warning to show if so. A stat failure after a
// successful read is not worth a word to the operator, so it yields no
// warning rather than an error.
func permissionWarning(path string) (string, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	perm := info.Mode().Perm()
	if perm&0o044 == 0 {
		return "", false
	}
	return fmt.Sprintf("%s is readable by other users (mode %#o); run chmod 600 on it", path, perm), true
}

// ChildEnv builds the complete environment for a process run on the
// project's behalf — the dev server itself, or a port_command probing for
// it — with dir the directory that process runs in, which is also what the
// project's relative env_file entries resolve against.
//
// The slice is assembled in precedence order, lowest first: the daemon's
// own environment, then a PATH entry prepending node_path, then the env
// files (a later file having already beaten an earlier one in Load), then
// the inline env map in sorted key order. os/exec keeps the LAST occurrence
// of a duplicated name, so the result is: inline env > env files > the
// node_path PATH > the inherited environment.
//
// The warnings are Load's, and are returned even alongside an error so a
// caller that is about to fail a start can still tell the operator that a
// secrets file is world-readable.
func ChildEnv(p *config.Project, dir string) (env []string, warnings []string, err error) {
	env = os.Environ()
	if p.NodePath != "" {
		// node_path is documented as the node executable's path; prepend its
		// directory to PATH (or the path itself if it already is a directory).
		nodeDir := p.NodePath
		if info, statErr := os.Stat(nodeDir); statErr != nil || !info.IsDir() {
			nodeDir = filepath.Dir(nodeDir)
		}
		env = append(env, "PATH="+nodeDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	vars, warnings, err := Load(p.EnvFileRefs(dir))
	if err != nil {
		return nil, warnings, err
	}
	for _, v := range vars {
		env = append(env, v.Name+"="+v.Value)
	}
	keys := make([]string, 0, len(p.Env))
	for k := range p.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+p.Env[k])
	}
	return env, warnings, nil
}

// sensitive matches the variable names whose values herd-wake masks. The
// test is a substring match anywhere in the name, case-insensitively, so
// DB_LOGIN, databaseUrl and MY_API_KEY_2 all match. It over-matches on
// purpose — PUBLIC_URL is masked although it is harmless — because the
// cost of masking a public value in a listing is a shrug and the cost of
// printing a private one is a rotated credential.
var sensitive = regexp.MustCompile("(?i)PASSWORD|SECRET|TOKEN|KEY|LOGIN|DSN|URL")

// Redacted is what stands in for a value herd-wake will not show.
const Redacted = "[redacted]"

// Sensitive reports whether a variable of this name should have its value
// masked wherever herd-wake renders an environment. See sensitive for the
// rule and why it deliberately over-matches.
func Sensitive(name string) bool {
	return sensitive.MatchString(name)
}

// Display renders a variable's value for human output: the value itself,
// or Redacted when the name looks sensitive. It is the only way a value
// should reach a log line, a diagnostic page, or the CLI — and it is never
// used on an env file's contents, which are not rendered at all.
func Display(name, value string) string {
	if Sensitive(name) {
		return Redacted
	}
	return value
}
