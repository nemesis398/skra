// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// shutdownGrace bounds how long Serve waits for the last connections to close
// once it has decided to exit.
const shutdownGrace = 2 * time.Second

// readHeaderTimeout bounds how long a connection may take to send its request
// headers, so a client that opens one and sends nothing does not hold it.
const readHeaderTimeout = 10 * time.Second

// idleClock tells when nothing has used the server for a while. Activity is a
// request in flight, an open event stream counting as one, and the end of the
// last request. Its one timer runs only while no request is in flight, and it
// is the timer that ends the process, by closing expired.
type idleClock struct {
	mu      sync.Mutex
	n       int
	timer   *time.Timer
	idle    time.Duration
	expired chan struct{}
	over    bool
}

func newIdleClock(idle time.Duration) *idleClock {
	c := &idleClock{idle: idle, expired: make(chan struct{})}
	c.timer = time.AfterFunc(idle, c.fire)
	return c
}

// start counts a request and stops the timer.
func (c *idleClock) start() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	c.timer.Stop()
}

// end uncounts a request and, when it was the last, starts the timer over.
func (c *idleClock) end() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n--; c.n == 0 {
		c.timer.Reset(c.idle)
	}
}

// fire is the timer's function. It decides and closes expired under the
// lock, so a request that starts as the timer fires either is counted first,
// and then the server stays and end arms the timer again, or starts after the
// decision. A request in flight here was started just as the timer fired,
// too late for its Stop.
func (c *idleClock) fire() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n > 0 || c.over {
		return
	}
	c.over = true
	close(c.expired)
}

// holdIdle keeps the idle clock from running until release is called, as a
// request in flight does: a scan job holds it for as long as it runs, so the
// service does not exit under a feeder that is still busy. A server without
// an idle clock holds nothing. Calling release more than once is harmless.
func (s *Server) holdIdle() (release func()) {
	s.idleMu.Lock()
	c := s.idle
	s.idleMu.Unlock()
	if c == nil {
		return func() {}
	}
	c.start()
	var once sync.Once
	return func() { once.Do(c.end) }
}

// httpServer is the server for the routes behind the guard of port.
func (s *Server) httpServer(port string) *http.Server {
	return &http.Server{Handler: s.handlerOn(port), ReadHeaderTimeout: readHeaderTimeout}
}

// loopbackPort returns the port of addr when it is TCP on 127.0.0.1, the one
// address skrá serves on. Config refuses any other listen host, but under
// socket activation the unit decides: a socket on every interface would let
// another host read the token from the page and write, and a unix socket has
// no port for the Host check.
func loopbackPort(addr net.Addr) (string, error) {
	if a, ok := addr.(*net.TCPAddr); ok && a.IP.Equal(net.IPv4(127, 0, 0, 1)) {
		return strconv.Itoa(a.Port), nil
	}
	return "", fmt.Errorf("refusing to serve on %s socket %s: skrá serves on 127.0.0.1 only; bind the socket unit to it with ListenStream=127.0.0.1:7631", addr.Network(), addr)
}

// Serve serves on ln until ln fails, or, when idle is above 0, until idle has
// passed with no request in flight, which ends it with a nil error. Idle 0
// never exits: the process was started by hand, not by its socket. A socket
// that is not TCP on 127.0.0.1 is closed and refused with an error. The
// request guard wants the port of ln in the Host header, so a socket unit on
// another port than listen in the config works.
func (s *Server) Serve(ln net.Listener, idle time.Duration) error {
	port, err := loopbackPort(ln.Addr())
	if err != nil {
		ln.Close()
		return err
	}
	srv := s.httpServer(port)
	if idle <= 0 {
		return srv.Serve(ln)
	}
	c := newIdleClock(idle)
	defer c.timer.Stop()
	s.idleMu.Lock()
	s.idle = c
	s.idleMu.Unlock()
	inner := srv.Handler
	srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.start()
		defer c.end()
		inner.ServeHTTP(w, r)
	})

	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	select {
	case err := <-done:
		return err
	case <-c.expired:
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		srv.Close()
	}
	if err := <-done; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
