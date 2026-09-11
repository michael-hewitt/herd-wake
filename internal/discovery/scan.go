package discovery

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/michael-hewitt/herd-wake/internal/config"
)

// candidate is a subdirectory that passed every discovery rule.
type candidate struct {
	name string
	dir  string
}

// scan lists the candidates in a discovery entry's directory, sorted by
// name, plus the subdirectories that were rejected by a rule and why.
//
// A subdirectory is a candidate when it is not excluded, is a git worktree
// or repository (has a .git file or directory), passes the repository
// filter (when set: its .git file must point into <repository>/.git/
// worktrees/), contains every require_files entry, and has a DNS-label
// name. Excluded and non-git directories are ignored silently; the other
// rejections are reported.
func scan(d *config.Discovery) ([]candidate, []Skip, error) {
	entries, err := os.ReadDir(d.Directory)
	if err != nil {
		return nil, nil, err
	}
	in := newInspector(d)
	var candidates []candidate
	var skips []Skip
	for _, entry := range entries {
		name := entry.Name()
		dir, reason, silent := in.inspect(name)
		switch {
		case reason == "":
			candidates = append(candidates, candidate{name: name, dir: dir})
		case !silent:
			skips = append(skips, Skip{Name: name, Directory: dir, Reason: reason})
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].name < candidates[j].name })
	sort.Slice(skips, func(i, j int) bool { return skips[i].Name < skips[j].Name })
	return candidates, skips, nil
}

// Check applies a discovery entry's candidate rules to the single
// subdirectory named label — the on-demand counterpart of a sync scan, used
// by a wildcard listener resolving <label>.<base_domain> and by `herd-wake
// url`. It returns the worktree's absolute path when it passes, or the
// rule it failed (an empty reason means it passed). label must be a DNS
// label (the caller validates it), so it can never name anything outside
// the entry's directory.
func Check(d *config.Discovery, label string) (dir string, reason string) {
	if !config.IsDNSLabel(label) {
		return filepath.Join(d.Directory, label), fmt.Sprintf("%q is not a valid DNS label", label)
	}
	dir, reason, _ = newInspector(d).inspect(label)
	return dir, reason
}

// inspector applies one discovery entry's candidate rules to
// subdirectories of its directory.
type inspector struct {
	d *config.Discovery
	// worktreesDir is <repository>/.git/worktrees (symlinks resolved) when
	// the repository filter is set.
	worktreesDir string
}

func newInspector(d *config.Discovery) *inspector {
	in := &inspector{d: d}
	if d.Repository != "" {
		root := d.Repository
		if resolved, err := filepath.EvalSymlinks(root); err == nil {
			root = resolved
		}
		in.worktreesDir = filepath.Join(root, ".git", "worktrees")
		if resolved, err := filepath.EvalSymlinks(in.worktreesDir); err == nil {
			in.worktreesDir = resolved
		}
	}
	return in
}

// inspect checks the subdirectory called name and returns its path, why it
// is not a candidate (empty when it is), and whether a scan stays silent
// about that rejection (excluded, missing, or plain non-git directories are
// not worth listing in sync output; every other reason is).
func (in *inspector) inspect(name string) (dir string, reason string, silent bool) {
	d := in.d
	dir = filepath.Join(d.Directory, name)
	if d.Excluded(name) {
		return dir, fmt.Sprintf("excluded by the entry's exclude patterns %v", d.Exclude), true
	}
	info, err := os.Stat(dir) // follows symlinks, unlike entry.IsDir
	if err != nil {
		return dir, fmt.Sprintf("no directory named %q under %s", name, d.Directory), true
	}
	if !info.IsDir() {
		return dir, fmt.Sprintf("%s is not a directory", dir), true
	}
	gitInfo, err := os.Stat(filepath.Join(dir, ".git"))
	if err != nil {
		return dir, "not a git worktree or repository (no .git inside)", true
	}

	if d.Repository != "" {
		if gitInfo.IsDir() {
			return dir, fmt.Sprintf("standalone git repository, not a linked worktree of %s", d.Repository), false
		}
		gitdir, err := readGitdir(filepath.Join(dir, ".git"))
		if err != nil {
			return dir, fmt.Sprintf("cannot read its .git file: %v", err), false
		}
		if !filepath.IsAbs(gitdir) {
			gitdir = filepath.Join(dir, gitdir)
		}
		resolved, err := filepath.EvalSymlinks(gitdir)
		if err != nil {
			return dir, fmt.Sprintf("its .git file points at %s, which does not exist (stale worktree?)", gitdir), false
		}
		if !strings.HasPrefix(resolved, in.worktreesDir+string(filepath.Separator)) {
			return dir, fmt.Sprintf("linked worktree of a different repository (%s), not of %s", gitdir, d.Repository), false
		}
	}

	if missing := missingFiles(dir, d.RequireFiles); len(missing) > 0 {
		return dir, fmt.Sprintf("missing required file(s): %s", strings.Join(missing, ", ")), false
	}

	if !config.IsDNSLabel(name) {
		return dir, fmt.Sprintf("%q is not a valid DNS label (use lowercase letters, digits, and hyphens; at most 63 characters); rename the directory to give it a URL", name), false
	}
	return dir, "", false
}

// readGitdir parses a worktree's .git file ("gitdir: <path>").
func readGitdir(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck // read-only
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if rest, ok := strings.CutPrefix(line, "gitdir:"); ok {
			if gitdir := strings.TrimSpace(rest); gitdir != "" {
				return gitdir, nil
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("no gitdir: line in %s", path)
}

// missingFiles returns the require_files entries absent from dir.
func missingFiles(dir string, files []string) []string {
	var missing []string
	for _, file := range files {
		if _, err := os.Stat(filepath.Join(dir, file)); err != nil {
			missing = append(missing, file)
		}
	}
	return missing
}
