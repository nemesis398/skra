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

// Server holds the page templates and the token that every write must carry.
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

// New builds a Server that wants token on every write; LoadToken reads it
// from the state directory, so it outlives the process. The templates are
// embedded, so a parse error is a bug in the binary and panics at start.
func New(cfg config.Config, deps Deps, token string) *Server {
	static, err := fs.Sub(webassets.Assets, "static")
	if err != nil {
		panic(err)
	}
	s := &Server{
		cfg:    cfg,
		deps:   deps,
		token:  token,
		tpl:    template.Must(template.ParseFS(webassets.Assets, "templates/*.html")),
		static: http.StripPrefix("/static/", http.FileServer(http.FS(static))),
	}
	s.hub = NewHub(s.tickPoll, pollEvery)
	if deps.Query != nil {
		s.hub.WithLive(s.livePoll, liveEvery)
	}
	return s
}

// Handler returns the routes behind the request guard, for the port of
// listen in the config. Serve guards the port of its listener instead.
func (s *Server) Handler() http.Handler {
	_, port, _ := net.SplitHostPort(s.cfg.Listen) // an empty port matches no Host: closed
	return s.handlerOn(port)
}

// handlerOn returns the routes behind the request guard for port.
func (s *Server) handlerOn(port string) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", s.static)
	for path, which := range pages {
		pattern := "GET " + path
		if path == "/" {
			pattern = "GET /{$}" // the overview, not every path
		}
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) { s.render(w, r, which) })
	}
	mux.HandleFunc("GET /events", s.events)
	s.actionRoutes(mux)
	s.printerRoutes(mux)
	return Guard(mux, port, s.token)
}
