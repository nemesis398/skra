// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net"
	"net/http"
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
	s := serverFor(&fakeCUPS{})
	// The guard wants the configured port in the Host header.
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	s.cfg.Listen = "127.0.0.1:" + port
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
