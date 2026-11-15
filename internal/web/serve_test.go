// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type served struct {
	addr string
	done chan error
	ln   net.Listener
}

func startServe(t *testing.T, idle time.Duration) *served {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := serverFor(&fakeCUPS{}) // its config says 7631; the guard takes the listener's port
	sv := &served{addr: ln.Addr().String(), done: make(chan error, 1), ln: ln}
	go func() { sv.done <- s.Serve(ln, idle) }()
	t.Cleanup(func() { ln.Close() })
	return sv
}

func (sv *served) exited(within time.Duration) (bool, error) {
	select {
	case err := <-sv.done:
		return true, err
	case <-time.After(within):
		return false, nil
	}
}

func TestIdleExitWithoutRequests(t *testing.T) {
	sv := startServe(t, 100*time.Millisecond)
	start := time.Now()
	ok, err := sv.exited(2 * time.Second)
	if !ok || err != nil {
		t.Fatalf("Serve did not return nil after the idle time (returned %t, err %v)", ok, err)
	}
	if d := time.Since(start); d < 90*time.Millisecond {
		t.Errorf("returned after %v, before the idle time", d)
	}
}

func TestIdleExitWaitsForTheLastRequest(t *testing.T) {
	sv := startServe(t, 150*time.Millisecond)
	for range 4 { // each request moves the end of activity
		time.Sleep(60 * time.Millisecond)
		req, _ := http.NewRequest("GET", "http://"+sv.addr+"/static/skra.css", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if ok, _ := sv.exited(50 * time.Millisecond); ok {
		t.Fatal("Serve returned while requests were still coming")
	}
	if ok, err := sv.exited(2 * time.Second); !ok || err != nil {
		t.Fatalf("Serve did not return nil afterwards (returned %t, err %v)", ok, err)
	}
}

func TestIdleExitNotWhileAnEventStreamIsOpen(t *testing.T) {
	sv := startServe(t, 100*time.Millisecond)
	req, _ := http.NewRequest("GET", "http://"+sv.addr+"/events", nil)
	ctx := t.Context()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := sv.exited(500 * time.Millisecond); ok {
		t.Fatal("Serve returned with an event stream open")
	}
	resp.Body.Close()
	if ok, err := sv.exited(3 * time.Second); !ok || err != nil {
		t.Fatalf("Serve did not return nil after the stream closed (returned %t, err %v)", ok, err)
	}
}

func TestServeWithoutIdleTimeNeverExits(t *testing.T) {
	sv := startServe(t, 0)
	if ok, _ := sv.exited(300 * time.Millisecond); ok {
		t.Fatal("Serve returned with idle 0")
	}
	sv.ln.Close()
	if ok, err := sv.exited(2 * time.Second); !ok || err == nil {
		t.Errorf("closing the listener gave (returned %t, err %v), want an error", ok, err)
	}
}

// The guard takes its port from the socket it serves, not from listen in the
// config: a socket unit on another port works, and the configured port is
// refused there.
func TestServeGuardsTheListenersPort(t *testing.T) {
	sv := startServe(t, 0)
	_, port, _ := net.SplitHostPort(sv.addr)
	for _, tc := range []struct {
		host string
		want int
	}{
		{"127.0.0.1:" + port, 200},
		{"localhost:" + port, 200},
		{"127.0.0.1:7631", 403},
	} {
		req, _ := http.NewRequest("GET", "http://"+sv.addr+"/static/skra.css", nil)
		req.Host = tc.host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("Host %s: status %d, want %d", tc.host, resp.StatusCode, tc.want)
		}
	}
}

// The timer decides under the clock's lock whether to end the server, and
// closes expired under it too, so a request that starts as it fires either
// counts first and keeps the server up, or comes after the decision.
func TestIdleClockFireKeepsARequestInFlight(t *testing.T) {
	c := newIdleClock(time.Hour)
	defer c.timer.Stop()
	c.start()
	c.fire() // the timer fired just as the request began
	select {
	case <-c.expired:
		t.Fatal("expired with a request in flight")
	default:
	}
	c.end() // the last request ends and starts the timer over
	if !c.timer.Stop() {
		t.Error("the timer was not armed again after the last request")
	}
	c.fire()
	select {
	case <-c.expired:
	default:
		t.Fatal("not expired with nothing in flight")
	}
	c.fire() // a second firing does not close twice
}

func TestServeBoundsTheWaitForHeaders(t *testing.T) {
	srv := serverFor(&fakeCUPS{}).httpServer("7631")
	if srv.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want 10s", srv.ReadHeaderTimeout)
	}
}

// Under socket activation the unit decides the socket. Serve refuses one that
// is not TCP on 127.0.0.1, which another host could reach or which has no
// port for the Host check, and names the socket and the fix.
func TestServeRefusesASocketOffLoopback(t *testing.T) {
	sockets := map[string]func() (net.Listener, error){
		"another loopback address": func() (net.Listener, error) { return net.Listen("tcp", "127.0.0.2:0") },
		"a unix socket":            func() (net.Listener, error) { return net.Listen("unix", filepath.Join(t.TempDir(), "s")) },
	}
	if ln, err := net.Listen("tcp6", "[::1]:0"); err == nil {
		ln.Close()
		sockets["IPv6 loopback"] = func() (net.Listener, error) { return net.Listen("tcp6", "[::1]:0") }
	}
	for name, listen := range sockets {
		t.Run(name, func(t *testing.T) {
			ln, err := listen()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { ln.Close() })
			done := make(chan error, 1)
			go func() { done <- serverFor(&fakeCUPS{}).Serve(ln, 0) }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("Serve returned nil")
				}
				msg := err.Error()
				if !strings.Contains(msg, ln.Addr().String()) || !strings.Contains(msg, "127.0.0.1:7631") {
					t.Errorf("error %q does not name the socket %s and the fix", msg, ln.Addr())
				}
			case <-time.After(time.Second):
				t.Fatalf("Serve served on %s %s", ln.Addr().Network(), ln.Addr())
			}
		})
	}
}

// The wildcard sockets a unit gets from ListenStream=7631 or 0.0.0.0:7631 are
// refused too; they are checked by address, so no test listens on them.
func TestLoopbackPort(t *testing.T) {
	for _, tc := range []struct {
		addr net.Addr
		want string // the port, or "" for a refusal
	}{
		{&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 7631}, "7631"},
		{&net.TCPAddr{IP: net.IPv4zero, Port: 7631}, ""},
		{&net.TCPAddr{IP: net.IPv6unspecified, Port: 7631}, ""},
		{&net.TCPAddr{IP: net.IPv4(192, 168, 1, 20), Port: 7631}, ""},
		{&net.TCPAddr{IP: net.IPv6loopback, Port: 7631}, ""},
		{&net.UnixAddr{Name: "/run/user/1000/skra.sock", Net: "unix"}, ""},
	} {
		port, err := loopbackPort(tc.addr)
		if port != tc.want || (err == nil) != (tc.want != "") {
			t.Errorf("loopbackPort(%s %s) = %q, %v; want %q", tc.addr.Network(), tc.addr, port, err, tc.want)
		}
		if err != nil && (!strings.Contains(err.Error(), tc.addr.String()) || !strings.Contains(err.Error(), "ListenStream=127.0.0.1:7631")) {
			t.Errorf("error %q does not name %s and the fix", err, tc.addr)
		}
	}
}
