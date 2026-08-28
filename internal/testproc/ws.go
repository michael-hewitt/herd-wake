// This file is minimal RFC 6455 WebSocket support for tests: a hand-rolled
// echo server handler (the server side of ModeWS, also usable directly in
// httptest servers) and a hand-rolled client (DialWS) for driving WebSocket
// traffic through the supervisor. Stdlib only, text/binary frames only, no
// fragmentation — just enough protocol for the proxy tests.

package testproc

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// wsMagicGUID is the fixed GUID from RFC 6455 §1.3 used to derive
// Sec-WebSocket-Accept from Sec-WebSocket-Key.
const wsMagicGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// maxWSPayload caps a single frame's payload; test traffic is tiny, so
// anything larger indicates frame-parsing gone wrong.
const maxWSPayload = 1 << 20

// WebSocket opcodes (RFC 6455 §5.2).
const (
	wsOpText   = 0x1
	wsOpBinary = 0x2
	wsOpClose  = 0x8
	wsOpPing   = 0x9
	wsOpPong   = 0xA
)

// errWSClosed is returned by ReadText when the peer sends a close frame.
var errWSClosed = errors.New("websocket closed by peer")

// WSDropMessage makes WSEchoHandler drop the TCP connection abruptly — no
// close frame — simulating the upstream side of an established tunnel dying.
const WSDropMessage = "testproc-drop-connection"

// WSEchoHandler answers WebSocket handshakes by echoing every text or binary
// frame back to the client (and answering pings with pongs) until the client
// starts the closing handshake or the connection dies. Non-upgrade requests
// get a plain-text echo like ModeHTTP, so readiness probes and mixed traffic
// work against the same port.
func WSEchoHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Sec-WebSocket-Key")
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || key == "" {
			fmt.Fprintf(w, "ok %s %s host=%s pid=%d\n", r.Method, r.URL.Path, r.Host, os.Getpid())
			return
		}

		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "response writer cannot hijack", http.StatusInternalServerError)
			return
		}
		conn, brw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close() //nolint:errcheck // tunnel teardown

		if _, err := brw.WriteString("HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\n" +
			"Connection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + wsAccept(key) + "\r\n\r\n"); err != nil {
			return
		}
		if err := brw.Flush(); err != nil {
			return
		}

		for {
			opcode, payload, err := readWSFrame(brw.Reader)
			if err != nil {
				return // client vanished (or sent garbage): drop the tunnel
			}
			switch opcode {
			case wsOpText, wsOpBinary:
				if string(payload) == WSDropMessage {
					return // abrupt upstream disconnect, no close frame
				}
				if err := writeWSFrame(conn, opcode, false, payload); err != nil {
					return
				}
			case wsOpPing:
				if err := writeWSFrame(conn, wsOpPong, false, payload); err != nil {
					return
				}
			case wsOpClose:
				_ = writeWSFrame(conn, wsOpClose, false, payload) // complete the closing handshake
				return
			default: // pong, continuation: nothing to do
			}
		}
	})
}

// serveWS is testproc mode "ws": WSEchoHandler on the port, exiting 0 on
// SIGTERM/SIGINT like ModeHTTP.
func serveWS() int {
	return serveHandler(WSEchoHandler(), "testproc serving ws on %s\n")
}

// WSClient is a minimal WebSocket client for tests. It is not safe for
// concurrent use.
type WSClient struct {
	conn net.Conn
	br   *bufio.Reader
}

// DialWS dials addr (host:port), performs the WebSocket opening handshake
// for path with the given Host header, and verifies the 101 response and
// its Sec-WebSocket-Accept. The handshake (dial included) is bounded by
// timeout — make it generous enough to cover a cold start.
func DialWS(addr, host, path string, timeout time.Duration) (*WSClient, error) {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		_ = conn.Close()
		return nil, err
	}

	var raw [16]byte
	binary.BigEndian.PutUint64(raw[:8], rand.Uint64())
	binary.BigEndian.PutUint64(raw[8:], rand.Uint64())
	key := base64.StdEncoding.EncodeToString(raw[:])

	if path == "" {
		path = "/"
	}
	if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\n"+
		"Host: %s\r\n"+
		"Upgrade: websocket\r\n"+
		"Connection: Upgrade\r\n"+
		"Sec-WebSocket-Key: %s\r\n"+
		"Sec-WebSocket-Version: 13\r\n\r\n", path, host, key); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("write handshake: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("read handshake response: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		_ = conn.Close()
		return nil, fmt.Errorf("handshake status %s: %s", resp.Status, body)
	}
	if got, want := resp.Header.Get("Sec-WebSocket-Accept"), wsAccept(key); got != want {
		_ = conn.Close()
		return nil, fmt.Errorf("Sec-WebSocket-Accept = %q, want %q", got, want)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &WSClient{conn: conn, br: br}, nil
}

// WriteText sends one masked text frame.
func (c *WSClient) WriteText(msg string) error {
	return writeWSFrame(c.conn, wsOpText, true, []byte(msg))
}

// ReadText reads frames until a text or binary message arrives and returns
// its payload. A close frame from the peer yields an error (as does a dead
// connection or an expired read deadline).
func (c *WSClient) ReadText() (string, error) {
	for {
		opcode, payload, err := readWSFrame(c.br)
		if err != nil {
			return "", err
		}
		switch opcode {
		case wsOpText, wsOpBinary:
			return string(payload), nil
		case wsOpClose:
			return "", errWSClosed
		case wsOpPing:
			if err := writeWSFrame(c.conn, wsOpPong, true, payload); err != nil {
				return "", err
			}
		default: // pong, continuation: keep reading
		}
	}
}

// SetReadDeadline bounds subsequent ReadText calls.
func (c *WSClient) SetReadDeadline(t time.Time) error { return c.conn.SetReadDeadline(t) }

// Close performs the closing handshake — send a close frame, wait briefly
// for the peer's close (or connection teardown) — then closes the TCP
// connection.
func (c *WSClient) Close() error {
	_ = writeWSFrame(c.conn, wsOpClose, true, nil)
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		opcode, _, err := readWSFrame(c.br)
		if err != nil || opcode == wsOpClose {
			break
		}
	}
	return c.conn.Close()
}

// Abort drops the TCP connection without a closing handshake — an abruptly
// disappearing client.
func (c *WSClient) Abort() error { return c.conn.Close() }

// wsAccept derives the Sec-WebSocket-Accept value for a handshake key.
func wsAccept(key string) string {
	sum := sha1.Sum([]byte(key + wsMagicGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// writeWSFrame writes one unfragmented frame. Client-to-server frames must
// be masked (mask true); server-to-client frames must not be.
func writeWSFrame(w io.Writer, opcode byte, mask bool, payload []byte) error {
	header := make([]byte, 0, 14)
	header = append(header, 0x80|opcode) // FIN + opcode
	n := len(payload)
	switch {
	case n < 126:
		header = append(header, byte(n))
	case n < 1<<16:
		header = append(header, 126, byte(n>>8), byte(n))
	default:
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		header = append(header, 127)
		header = append(header, ext[:]...)
	}
	if mask {
		header[1] |= 0x80
		var key [4]byte
		binary.BigEndian.PutUint32(key[:], rand.Uint32())
		header = append(header, key[:]...)
		masked := make([]byte, n)
		for i, b := range payload {
			masked[i] = b ^ key[i&3]
		}
		payload = masked
	}
	if _, err := w.Write(header); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// readWSFrame reads one unfragmented frame, unmasking the payload if the
// peer masked it.
func readWSFrame(r *bufio.Reader) (opcode byte, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	if h[0]&0x80 == 0 {
		return 0, nil, errors.New("fragmented websocket frames are not supported")
	}
	opcode = h[0] & 0x0F
	masked := h[1]&0x80 != 0
	n := uint64(h[1] & 0x7F)
	switch n {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(r, ext[:]); err != nil {
			return 0, nil, err
		}
		n = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(r, ext[:]); err != nil {
			return 0, nil, err
		}
		n = binary.BigEndian.Uint64(ext[:])
	}
	if n > maxWSPayload {
		return 0, nil, fmt.Errorf("websocket frame payload %d exceeds test cap %d", n, maxWSPayload)
	}
	var key [4]byte
	if masked {
		if _, err = io.ReadFull(r, key[:]); err != nil {
			return 0, nil, err
		}
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= key[i&3]
		}
	}
	return opcode, payload, nil
}
