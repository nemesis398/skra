// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"sync"

	"github.com/nemesis398/skra/internal/config"
	webassets "github.com/nemesis398/skra/web"
)

// Server holds the page templates and the per-process token.
type Server struct {
	cfg    config.Config
	deps   Deps
	token  string
	tpl    *template.Template
	static http.Handler
	hub    *Hub

	// pollMu guards the poll state: the tick count and the last good data.
	pollMu   sync.Mutex
	ticks    int
	haveDone bool
	good     Snapshot
}

// New builds a Server with a fresh token. The templates are embedded, so a
// parse error is a bug in the binary and panics at start.
func New(cfg config.Config, deps Deps) *Server {
	static, err := fs.Sub(webassets.Assets, "static")
	if err != nil {
		panic(err)
	}
	s := &Server{
		cfg:    cfg,
		deps:   deps,
		token:  NewToken(),
		tpl:    template.Must(template.ParseFS(webassets.Assets, "templates/*.html")),
		static: http.StripPrefix("/static/", http.FileServer(http.FS(static))),
	}
	s.hub = NewHub(s.tickPoll, pollEvery)
	return s
}

// Handler returns the routes behind the request guard.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", s.static)
	mux.HandleFunc("GET /{$}", s.page)
	mux.HandleFunc("GET /events", s.events)
	_, port, _ := net.SplitHostPort(s.cfg.Listen) // an empty port matches no Host: closed
	return Guard(mux, port, s.token)
}
