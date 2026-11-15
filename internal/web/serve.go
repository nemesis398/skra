// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"
)

// shutdownGrace bounds how long Serve waits for the last connections to close
// once it has decided to exit.
const shutdownGrace = 2 * time.Second

// idleClock tells when nothing has used the server for a while. Activity is a
// request in flight, an open event stream counting as one, and the end of the
// last request. Its one timer runs only while no request is in flight, and it
// is the timer that ends the process.
type idleClock struct {
	mu    sync.Mutex
	n     int
	timer *time.Timer
	idle  time.Duration
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

// Serve serves on ln until ln fails, or, when idle is above 0, until idle has
// passed with no request in flight, which ends it with a nil error. Idle 0
// never exits: the process was started by hand, not by its socket. The
// request guard wants the port of ln in the Host header, so a socket unit on
// another port than listen in the config works.
func (s *Server) Serve(ln net.Listener, idle time.Duration) error {
	_, port, _ := net.SplitHostPort(ln.Addr().String()) // no port matches no Host: closed
	srv := &http.Server{Handler: s.handlerOn(port)}
	if idle <= 0 {
		return srv.Serve(ln)
	}
	expired := make(chan struct{})
	var once sync.Once
	c := &idleClock{idle: idle}
	c.timer = time.AfterFunc(idle, func() {
		c.mu.Lock()
		quiet := c.n == 0 // a request that began as the timer fired wins
		c.mu.Unlock()
		if quiet {
			once.Do(func() { close(expired) })
		}
	})
	defer c.timer.Stop()
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
	case <-expired:
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
