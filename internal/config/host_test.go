package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// hostProject renders a project block sharing supervisorPort under host.
func hostProject(name, host string, supervisorPort, applicationPort int, extra string) string {
	return "  " + name + ":\n" +
		"    host: " + host + "\n" +
		"    supervisor_port: " + itoa(supervisorPort) + "\n" +
		"    application_port: " + itoa(applicationPort) + "\n" +
		"    working_directory: /tmp\n" +
		"    command: npm run dev\n" + extra
}

func itoa(n int) string { return strconv.Itoa(n) }

func writeMain(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestHostProjectsShareSupervisorPort: projects that all set host may share
// a supervisor_port; host is lowercased and public_url derived from it.
func TestHostProjectsShareSupervisorPort(t *testing.T) {
	path := writeMain(t, "projects:\n"+
		hostProject("alpha", "A.Preview.Example.com", 7101, 17101, "")+
		hostProject("beta", "b.preview.example.com", 7101, 17102, "    public_url: http://custom.example.com\n"))
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	a, b := cfg.Projects["alpha"], cfg.Projects["beta"]
	if a.Host != "a.preview.example.com" || a.PublicURL != "https://a.preview.example.com" {
		t.Errorf("alpha host/public_url = %q/%q, want lowercased host and a derived URL", a.Host, a.PublicURL)
	}
	if b.PublicURL != "http://custom.example.com" {
		t.Errorf("beta public_url = %q, want the explicit one kept", b.PublicURL)
	}
	if a.SupervisorPort != 7101 || b.SupervisorPort != 7101 {
		t.Errorf("shared supervisor_port not preserved: %d / %d", a.SupervisorPort, b.SupervisorPort)
	}
}

// TestValidateHostSharingRules: a supervisor_port may not be shared with a
// project lacking host, hosts must be unique, listen_host must match
// across sharers, application ports stay exclusive, and host must be a
// hostname.
func TestValidateHostSharingRules(t *testing.T) {
	for name, tc := range map[string]struct {
		body    string
		project string
		field   string
		message string
	}{
		"host and no-host": {
			body: hostProject("alpha", "a.example", 7101, 17101, "") + `  beta:
    public_url: https://beta.test
    supervisor_port: 7101
    application_port: 17102
    working_directory: /tmp
    command: npm run dev
`,
			project: "beta", field: "supervisor_port", message: "all set host",
		},
		"duplicate host": {
			body:    hostProject("alpha", "a.example", 7101, 17101, "") + hostProject("beta", "A.EXAMPLE", 7102, 17102, ""),
			project: "beta", field: "host", message: `host "a.example" is already used by project "alpha"`,
		},
		"listen_host differs": {
			body:    hostProject("alpha", "a.example", 7101, 17101, "") + hostProject("beta", "b.example", 7101, 17102, "    listen_host: localhost\n"),
			project: "beta", field: "listen_host", message: "must bind the same listen_host",
		},
		"application port shared": {
			body:    hostProject("alpha", "a.example", 7101, 17101, "") + hostProject("beta", "b.example", 7101, 17101, ""),
			project: "beta", field: "application_port", message: "every port must be unique",
		},
		"supervisor port is another's application port": {
			body:    hostProject("alpha", "a.example", 7101, 17101, "") + hostProject("beta", "b.example", 17101, 17102, ""),
			project: "beta", field: "supervisor_port", message: `project "alpha" (application_port)`,
		},
		"invalid host": {
			body:    hostProject("alpha", "https://a.example/", 7101, 17101, ""),
			project: "alpha", field: "host", message: "invalid hostname",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeMain(t, "projects:\n"+tc.body))
			if err == nil {
				t.Fatal("Load should fail")
			}
			got := err.Error()
			wantError(t, got, tc.project, tc.field)
			if !strings.Contains(got, tc.message) {
				t.Errorf("error should contain %q; got:\n%s", tc.message, got)
			}
		})
	}
}

// TestValidateHostProjectsAndWildcardEntryCannotSharePort: a wildcard
// entry's supervisor_port is exclusive.
func TestValidateWildcardPortIsExclusive(t *testing.T) {
	workspace := t.TempDir()
	body := "projects:\n" + hostProject("alpha", "a.example", 41000, 17101, "") + `discovery:
  - name: webapp
    mode: wildcard
    supervisor_port: 41000
    directory: ` + workspace + `
    command: ./start.sh
    application_port_range: [42000, 42999]
  - name: other
    mode: wildcard
    supervisor_port: 41001
    directory: ` + workspace + `
    command: ./start.sh
    application_port_range: [42000, 42999]
  - name: dupe
    mode: wildcard
    supervisor_port: 41001
    directory: ` + workspace + `
    command: ./start.sh
    application_port_range: [42000, 42999]
`
	_, err := Load(writeMain(t, body))
	if err == nil {
		t.Fatal("Load should fail")
	}
	got := err.Error()
	if !strings.Contains(got, `discovery "webapp": supervisor_port: port 41000 is already used by project "alpha"`) {
		t.Errorf("errors missing the project/wildcard conflict; got:\n%s", got)
	}
	if !strings.Contains(got, `discovery "dupe": supervisor_port: port 41001 is already used by wildcard discovery entry "other"`) {
		t.Errorf("errors missing the wildcard/wildcard conflict; got:\n%s", got)
	}
}

// TestLoadWildcardEntry: a wildcard entry loads with its defaults and its
// helpers derive hosts, URLs, labels, and dynamic projects.
func TestLoadWildcardEntry(t *testing.T) {
	workspace := t.TempDir()
	path := writeDiscoveryConfig(t, `  - name: webapp
    mode: wildcard
    supervisor_port: 41000
    directory: `+workspace+`
    command: ./start.sh
    port_command: cat port
    env: { ENVIRONMENT: dev }
    rewrite_host: true
    idle_timeout_minutes: 30
  - name: preview
    mode: wildcard
    base_domain: Preview.Example.com
    supervisor_port: 41001
    directory: `+workspace+`
    command: ./start.sh
    application_port_range: [42000, 42999]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	d := cfg.Discovery[0]
	if !d.Wildcard() || d.Mode != ModeWildcard || d.BaseDomain != "webapp.test" || d.SupervisorPort() != 41000 {
		t.Errorf("webapp = mode %q base %q port %d", d.Mode, d.BaseDomain, d.SupervisorPort())
	}
	if d.URLTemplate != "" {
		t.Errorf("url_template should stay empty in wildcard mode, got %q", d.URLTemplate)
	}
	if d.Host("issue-1") != "issue-1.webapp.test" || d.PublicURL("issue-1") != "https://issue-1.webapp.test" || d.URLPattern() != "https://<label>.webapp.test" {
		t.Errorf("host/url helpers = %q %q %q", d.Host("issue-1"), d.PublicURL("issue-1"), d.URLPattern())
	}
	if d.DynamicSource() != "discovery:webapp" || d.ListenHost() != DefaultListenHost {
		t.Errorf("DynamicSource/ListenHost = %q/%q", d.DynamicSource(), d.ListenHost())
	}
	for host, want := range map[string]string{
		"issue-1.webapp.test":     "issue-1",
		"webapp.test":             "",
		"www.issue-1.webapp.test": "",
		"issue_1.webapp.test":     "",
		"issue-1.other.test":      "",
		"":                        "",
	} {
		label, ok := d.Label(host)
		if label != want || ok != (want != "") {
			t.Errorf("Label(%q) = %q, %v; want %q", host, label, ok, want)
		}
	}

	if err := os.MkdirAll(filepath.Join(workspace, "issue-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := d.Project("issue-1", filepath.Join(workspace, "issue-1"), 45001)
	if p.Name != "issue-1" || p.Source != "discovery:webapp" || p.Host != "issue-1.webapp.test" || p.PublicURL != "https://issue-1.webapp.test" {
		t.Errorf("Project identity = %+v", p)
	}
	if p.SupervisorPort != 41000 || p.ApplicationPort != 45001 || p.WorkingDirectory != filepath.Join(workspace, "issue-1") {
		t.Errorf("Project generated fields = %+v", p)
	}
	if p.Command != "./start.sh" || !p.RewriteHost || p.IdleTimeoutMinutes != 30 || p.Env["ENVIRONMENT"] != "dev" {
		t.Errorf("Project template fields = %+v", p)
	}
	if p.ReadinessStrategy != ReadinessHTTP || p.ReadinessURL != "http://127.0.0.1:45001/" || p.ShutdownSignal != DefaultShutdownSignal || p.ListenHost != DefaultListenHost {
		t.Errorf("Project defaults not applied: %+v", p)
	}
	p.Env["X"] = "y"
	if _, leaked := d.Template.Env["X"]; leaked {
		t.Error("Project shares the template's env map")
	}
	if err := (&Config{Projects: map[string]*Project{"issue-1": p}}).Validate(); err != nil {
		t.Errorf("a materialised project should validate: %v", err)
	}

	preview := cfg.Discovery[1]
	if preview.BaseDomain != "preview.example.com" || preview.Host("x") != "x.preview.example.com" {
		t.Errorf("explicit base_domain = %q", preview.BaseDomain)
	}
	if !d.Equivalent(d) || d.Equivalent(preview) {
		t.Error("Equivalent should compare whole entries")
	}
	// A proxy-mode entry keeps its url_template default and no port.
	proxyPath := writeDiscoveryConfig(t, `  - name: legacy
    directory: `+workspace+`
    command: npm run dev
    supervisor_port_range: [7100, 7199]
    application_port_range: [17100, 17199]
`)
	proxyCfg, err := Load(proxyPath)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if l := proxyCfg.Discovery[0]; l.Mode != ModeProxy || l.Wildcard() || l.SupervisorPort() != 0 || l.BaseDomain != "" || l.URLTemplate != DefaultURLTemplate {
		t.Errorf("proxy entry = %+v", l)
	}
}

// TestValidateWildcardEntryFields: wildcard mode rejects proxy-mode fields
// and requires its own; proxy mode rejects base_domain; unknown modes fail.
func TestValidateWildcardEntryFields(t *testing.T) {
	workspace := t.TempDir()
	path := writeDiscoveryConfig(t, `  - name: bad
    mode: wildcard
    base_domain: "not a domain"
    url_template: https://{name}.test
    supervisor_port_range: [41000, 41999]
    directory: `+workspace+`
    command: ./start.sh
    host: fixed.test
  - name: noport
    mode: wildcard
    directory: `+workspace+`
    command: ./start.sh
    application_port_range: [42000, 42999]
  - name: tld
    mode: wildcard
    base_domain: webapp
    supervisor_port: 41002
    directory: `+workspace+`
    command: ./start.sh
    application_port_range: [42000, 42999]
  - name: legacy
    base_domain: legacy.test
    directory: `+workspace+`
    command: ./start.sh
    supervisor_port_range: [41000, 41999]
    application_port_range: [42000, 42999]
  - name: weird
    mode: sideways
    directory: `+workspace+`
    command: ./start.sh
    supervisor_port_range: [41000, 41999]
    application_port_range: [42000, 42999]
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load should reject the entries")
	}
	got := err.Error()
	for _, want := range []string{
		`discovery "bad": base_domain: invalid domain`,
		`discovery "bad": url_template: not used in wildcard mode`,
		`discovery "bad": supervisor_port_range: not used in wildcard mode`,
		`discovery "bad": supervisor_port: required in wildcard mode`,
		`discovery "bad": application_port_range: required`,
		`discovery "bad": host: not allowed`,
		`discovery "noport": supervisor_port: required in wildcard mode`,
		`discovery "tld": base_domain: invalid domain "webapp"`,
		`discovery "legacy": base_domain: only used in wildcard mode`,
		`discovery "weird": mode: unknown mode "sideways"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("errors missing %q; got:\n%s", want, got)
		}
	}
}

func TestIsHostname(t *testing.T) {
	for host, want := range map[string]bool{
		"a.example":                true,
		"webapp.test":              true,
		"feat-huddle.preview.x.io": true,
		"localhost":                true,
		"A.example":                false,
		"a..example":               false,
		".a":                       false,
		"a.example:443":            false,
		"a_b.example":              false,
		"":                         false,
	} {
		if got := IsHostname(host); got != want {
			t.Errorf("IsHostname(%q) = %v, want %v", host, got, want)
		}
	}
}
