package discovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/michael-hewitt/herd-wake/internal/config"
	"github.com/michael-hewitt/herd-wake/internal/herd"
)

// ErrNoSuchProject is returned by Remove when no project has the name.
var ErrNoSuchProject = errors.New("no such project")

// MainConfigError is returned by Remove when the project is defined in the
// hand-written main config file, which herd-wake never rewrites.
type MainConfigError struct {
	Name       string
	ConfigPath string
}

func (e *MainConfigError) Error() string {
	return fmt.Sprintf("project %q is defined in %s, which herd-wake never rewrites; remove it there by hand", e.Name, e.ConfigPath)
}

// RemoveResult is the outcome of one Remove.
type RemoveResult struct {
	Name string `json:"name"`
	// Source is the projects.d file the project was removed from
	// (relative to the config directory, as Project.Source).
	Source string `json:"source"`
	File   string `json:"file"`
	// Managed reports whether that file is a discovery-managed file. The
	// next `herd-wake sync` re-creates the project if its worktree still
	// exists.
	Managed bool         `json:"managed"`
	Herd    []HerdAction `json:"herd"`
}

// RemoveOptions configures Remove.
type RemoveOptions struct {
	// Herd is the Herd CLI, or nil when unavailable (HerdNote says why).
	Herd     *herd.CLI
	HerdNote string
	// KeepHerd leaves the project's Herd proxy in place.
	KeepHerd bool
}

// Remove deletes the named project from the projects.d file that defines
// it and removes its Herd proxy (unless opts.KeepHerd). The caller reloads
// the daemon afterwards. A project defined in the main config file is
// reported with a *MainConfigError and left alone; an unknown name with
// ErrNoSuchProject.
func Remove(ctx context.Context, cfg *config.Config, name string, opts RemoveOptions) (*RemoveResult, error) {
	if cfg.Path == "" {
		return nil, errors.New("remove needs a configuration loaded from a file")
	}
	p, ok := cfg.Projects[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNoSuchProject, name)
	}
	rel, ok := strings.CutPrefix(p.Source, config.ProjectsDirName+"/")
	if !ok {
		return nil, &MainConfigError{Name: name, ConfigPath: cfg.Path}
	}
	file := filepath.Join(config.ProjectsDir(cfg.Path), rel)
	res := &RemoveResult{Name: name, Source: p.Source, File: file, Herd: []HerdAction{}}

	projects, err := config.DecodeFragment(file)
	if err != nil {
		return nil, err
	}
	delete(projects, name)

	header := ""
	if res.Managed, err = hasManagedMarker(file); err != nil {
		return nil, err
	}
	if res.Managed {
		header = existingHeader(file)
	}
	content, err := render(header, projects)
	if err != nil {
		return nil, err
	}
	if err := writeAtomic(file, content); err != nil {
		return nil, err
	}

	if !opts.KeepHerd {
		s := &syncer{ctx: ctx, opts: Options{Herd: opts.Herd, HerdNote: opts.HerdNote}}
		res.Herd = append(res.Herd, s.unproxyAction(true, p))
	}
	return res, nil
}

// hasManagedMarker reports whether an existing file starts with the
// managed marker (unlike IsManagedFile, a missing or empty file does not
// count).
func hasManagedMarker(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	return strings.HasPrefix(strings.TrimSpace(string(data)), managedMarker), nil
}

// existingHeader returns the leading comment block of a managed file so a
// rewrite keeps it verbatim.
func existingHeader(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return managedMarker + " — do not edit.\n"
	}
	var header strings.Builder
	for line := range strings.SplitSeq(string(data), "\n") {
		if !strings.HasPrefix(line, "#") {
			break
		}
		header.WriteString(line)
		header.WriteString("\n")
	}
	return header.String()
}
