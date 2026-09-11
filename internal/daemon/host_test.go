package daemon

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/michael-hewitt/herd-wake/internal/testproc"
)

func netListen(port int) (net.Listener, error) {
	return net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
}

func discardLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// TestHostRoutingSharedListener: two projects with different hosts share
// one supervisor_port and are told apart by the request's Host header —
// for plain requests (case and port ignored) and WebSocket upgrades alike;
// a hostname nobody claims gets a 404 diagnostic naming it and the hosts
// served; reloads touch only the project that changed; the listener stays
// up while any member remains and closes with the last one.
func TestHostRoutingSharedListener(t *testing.T) {
	f := newReloadFixture(t)
	shared := freePort(t)
	alpha := projectSpec{name: "alpha", host: "a.example", supervisorPort: shared, mode: testproc.ModeWS}
	beta := projectSpec{name: "beta", host: "b.example", supervisorPort: shared, mode: testproc.ModeWS}
	f.writeMain(f.yaml(alpha), f.yaml(beta))
	f.start()

	code, body := getHost(t, shared, "a.example", "/one", nil)
	if code != http.StatusOK || !strings.Contains(body, "ok GET /one host=a.example") {
		t.Fatalf("a.example = %d; body:\n%s", code, body)
	}
	alphaPid := echoPid(t, body)
	code, body = getHost(t, shared, "B.EXAMPLE:443", "/two", nil)
	if code != http.StatusOK || !strings.Contains(body, "host=B.EXAMPLE:443") {
		t.Fatalf("B.EXAMPLE:443 = %d; body:\n%s", code, body)
	}
	betaPid := echoPid(t, body)
	if alphaPid == betaPid {
		t.Fatalf("alpha and beta served by the same process %d", alphaPid)
	}
	if st := projectStatusByName(t, f.socket, "alpha"); st.Host != "a.example" || st.PublicURL != "https://alpha.test" || st.SupervisorPort != shared {
		t.Errorf("alpha status = %+v", st)
	}

	// WebSocket upgrades route the same way.
	addr := fmt.Sprintf("127.0.0.1:%d", shared)
	for _, host := range []string{"a.example", "b.example"} {
		c, err := testproc.DialWS(addr, host, "/hmr", 30*time.Second)
		if err != nil {
			t.Fatalf("websocket to %s: %v", host, err)
		}
		wsEcho(t, c, "hello "+host)
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := testproc.DialWS(addr, "c.example", "/hmr", 5*time.Second); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("websocket to an unknown host = %v, want a 404 handshake failure", err)
	}

	// Unknown host: a 404 diagnostic, never a hang or another project.
	code, body = getHost(t, shared, "c.example", "/", nil)
	if code != http.StatusNotFound {
		t.Fatalf("c.example = %d, want 404; body:\n%s", code, body)
	}
	for _, want := range []string{`no project for host "c.example"`, "a.example, b.example", "host: c.example"} {
		if !strings.Contains(body, want) {
			t.Errorf("404 body missing %q:\n%s", want, body)
		}
	}
	code, body = getHost(t, shared, "c.example", "/", http.Header{"Accept": []string{"text/html"}})
	if code != http.StatusNotFound || !strings.Contains(body, "<title>herd-wake: no project for host") {
		t.Errorf("HTML 404 = %d; body:\n%s", code, body)
	}

	// Reload: only alpha changes; beta keeps its process on the shared port.
	f.writeMain(f.yaml(projectSpec{name: "alpha", host: "a.example", supervisorPort: shared, mode: testproc.ModeWS, commandPrefix: "echo v2"}), f.yaml(beta))
	resp := f.reload()
	assertDiff(t, resp, "", "", "alpha", "beta")
	waitProcessGone(t, alphaPid)
	if _, body := getHost(t, shared, "b.example", "/", nil); echoPid(t, body) != betaPid {
		t.Errorf("beta was disturbed by alpha's change")
	}
	code, body = getHost(t, shared, "a.example", "/", nil)
	if code != http.StatusOK || echoPid(t, body) == alphaPid {
		t.Errorf("alpha after change = %d; body:\n%s", code, body)
	}

	// Removing beta leaves the listener up for alpha; b.example is now
	// unknown. Removing alpha closes the port.
	f.writeMain(f.yaml(projectSpec{name: "alpha", host: "a.example", supervisorPort: shared, mode: testproc.ModeWS, commandPrefix: "echo v2"}))
	resp = f.reload()
	assertDiff(t, resp, "", "beta", "", "alpha")
	waitProcessGone(t, betaPid)
	if code, _ := getHost(t, shared, "b.example", "/", nil); code != http.StatusNotFound {
		t.Errorf("b.example after beta's removal = %d, want 404", code)
	}
	if code, _ := getHost(t, shared, "a.example", "/", nil); code != http.StatusOK {
		t.Errorf("a.example after beta's removal = %d, want 200", code)
	}
	f.writeMain()
	resp = f.reload()
	assertDiff(t, resp, "", "alpha", "", "")
	if portAccepts(shared) {
		t.Errorf("shared port %d still accepts connections after its last project was removed", shared)
	}
}

// TestHostProjectJoinsAndLeavesSharedListener: a reload can add a host
// project to an existing shared port, move a project between a shared
// and an exclusive port, and change a project's host in place.
func TestHostProjectJoinsAndLeavesSharedListener(t *testing.T) {
	f := newReloadFixture(t)
	shared := freePort(t)
	f.writeMain(f.yaml(projectSpec{name: "alpha", host: "a.example", supervisorPort: shared}))
	f.start()
	_, body := getHost(t, shared, "a.example", "/", nil)
	alphaPid := echoPid(t, body)

	// beta joins the shared port; alpha is untouched.
	f.writeMain(
		f.yaml(projectSpec{name: "alpha", host: "a.example", supervisorPort: shared}),
		f.yaml(projectSpec{name: "beta", host: "b.example", supervisorPort: shared}))
	assertDiff(t, f.reload(), "beta", "", "", "alpha")
	if code, body := getHost(t, shared, "b.example", "/", nil); code != http.StatusOK || !strings.Contains(body, "host=b.example") {
		t.Fatalf("beta on the shared port = %d; body:\n%s", code, body)
	}
	if _, body := getHost(t, shared, "a.example", "/", nil); echoPid(t, body) != alphaPid {
		t.Error("alpha was disturbed by beta joining")
	}

	// alpha's host changes in place (same port): swapped, beta untouched.
	f.writeMain(
		f.yaml(projectSpec{name: "alpha", host: "a2.example", supervisorPort: shared}),
		f.yaml(projectSpec{name: "beta", host: "b.example", supervisorPort: shared}))
	assertDiff(t, f.reload(), "", "", "alpha", "beta")
	if code, _ := getHost(t, shared, "a.example", "/", nil); code != http.StatusNotFound {
		t.Errorf("old host a.example = %d, want 404", code)
	}
	if code, _ := getHost(t, shared, "a2.example", "/", nil); code != http.StatusOK {
		t.Errorf("new host a2.example = %d, want 200", code)
	}

	// beta moves to a port of its own (no host): the shared port keeps
	// serving alpha; beta's new port ignores the Host header.
	own := freePort(t)
	f.writeMain(
		f.yaml(projectSpec{name: "alpha", host: "a2.example", supervisorPort: shared}),
		f.yaml(projectSpec{name: "beta", supervisorPort: own}))
	assertDiff(t, f.reload(), "", "", "beta", "alpha")
	if code, body := getHost(t, own, "whatever.example", "/", nil); code != http.StatusOK || !strings.Contains(body, "host=whatever.example") {
		t.Errorf("beta on its own port = %d; body:\n%s", code, body)
	}
	if code, _ := getHost(t, shared, "b.example", "/", nil); code != http.StatusNotFound {
		t.Errorf("b.example on the shared port after beta left = %d, want 404", code)
	}
	if code, _ := getHost(t, shared, "a2.example", "/", nil); code != http.StatusOK {
		t.Errorf("alpha after beta left = %d, want 200", code)
	}

	// Status still answers and lists both.
	status, err := f.client.Status(context.Background())
	if err != nil || len(status.Projects) != 2 {
		t.Errorf("status = %+v, %v", status, err)
	}
}
