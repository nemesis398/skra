// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"sync"

	"github.com/nemesis398/skra/internal/config"
	"github.com/nemesis398/skra/internal/printer"
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
	// good is only written with goodMu held too, so an action can read the
	// names in it without waiting for a poll that is under way.
	pollMu   sync.Mutex
	ticks    int
	haveDone bool
	good     Snapshot
	goodMu   sync.RWMutex

	// live is the last answer of each queried printer, by queue name. The
	// hub's live loop writes it and every CUPS poll reads it.
	liveMu sync.Mutex
	live   map[string]printer.Live
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
	if deps.Query != nil {
		s.hub.WithLive(s.livePoll, liveEvery)
	}
	return s
}

// Handler returns the routes behind the request guard.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", s.static)
	mux.HandleFunc("GET /{$}", s.page)
	mux.HandleFunc("GET /events", s.events)
	s.actionRoutes(mux)
	s.printerRoutes(mux)
	mux.HandleFunc("GET /history", s.historyPage)
	_, port, _ := net.SplitHostPort(s.cfg.Listen) // an empty port matches no Host: closed
	return Guard(mux, port, s.token)
}
