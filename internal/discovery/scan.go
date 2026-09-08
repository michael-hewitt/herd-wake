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

	var worktreesDir string
	if d.Repository != "" {
		root := d.Repository
		if resolved, err := filepath.EvalSymlinks(root); err == nil {
			root = resolved
		}
		worktreesDir = filepath.Join(root, ".git", "worktrees")
		if resolved, err := filepath.EvalSymlinks(worktreesDir); err == nil {
			worktreesDir = resolved
		}
	}

	var candidates []candidate
	var skips []Skip
	for _, entry := range entries {
		name := entry.Name()
		dir := filepath.Join(d.Directory, name)
		if d.Excluded(name) {
			continue
		}
		info, err := os.Stat(dir) // follows symlinks, unlike entry.IsDir
		if err != nil || !info.IsDir() {
			continue
		}
		gitInfo, err := os.Stat(filepath.Join(dir, ".git"))
		if err != nil {
			continue // not a git worktree or repository
		}
		skip := func(format string, args ...any) {
			skips = append(skips, Skip{Name: name, Directory: dir, Reason: fmt.Sprintf(format, args...)})
		}

		if d.Repository != "" {
			if gitInfo.IsDir() {
				skip("standalone git repository, not a linked worktree of %s", d.Repository)
				continue
			}
			gitdir, err := readGitdir(filepath.Join(dir, ".git"))
			if err != nil {
				skip("cannot read its .git file: %v", err)
				continue
			}
			if !filepath.IsAbs(gitdir) {
				gitdir = filepath.Join(dir, gitdir)
			}
			resolved, err := filepath.EvalSymlinks(gitdir)
			if err != nil {
				skip("its .git file points at %s, which does not exist (stale worktree?)", gitdir)
				continue
			}
			if !strings.HasPrefix(resolved, worktreesDir+string(filepath.Separator)) {
				skip("linked worktree of a different repository (%s), not of %s", gitdir, d.Repository)
				continue
			}
		}

		missing := missingFiles(dir, d.RequireFiles)
		if len(missing) > 0 {
			skip("missing required file(s): %s", strings.Join(missing, ", "))
			continue
		}

		if !config.IsDNSLabel(name) {
			skip("%q is not a valid DNS label (use lowercase letters, digits, and hyphens; at most 63 characters); rename the directory to give it a URL", name)
			continue
		}

		candidates = append(candidates, candidate{name: name, dir: dir})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].name < candidates[j].name })
	sort.Slice(skips, func(i, j int) bool { return skips[i].Name < skips[j].Name })
	return candidates, skips, nil
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
