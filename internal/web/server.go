// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"html/template"
	"io/fs"
	"net"
	"net/http"

	"github.com/nemesis398/skra/internal/config"
	webassets "github.com/nemesis398/skra/web"
)

// Deps are the collaborators the handlers use. Later tasks add the CUPS
// client, the store and the printer query.
type Deps struct{}

// Server holds the page templates and the per-process token.
type Server struct {
	cfg    config.Config
	deps   Deps
	token  string
	layout *template.Template
	static http.Handler
}

// New builds a Server with a fresh token. The templates are embedded, so a
// parse error is a bug in the binary and panics at start.
func New(cfg config.Config, deps Deps) *Server {
	static, err := fs.Sub(webassets.Assets, "static")
	if err != nil {
		panic(err)
	}
	return &Server{
		cfg:    cfg,
		deps:   deps,
		token:  NewToken(),
		layout: template.Must(template.ParseFS(webassets.Assets, "templates/*.html")),
		static: http.StripPrefix("/static/", http.FileServer(http.FS(static))),
	}
}

// Handler returns the routes behind the request guard.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", s.static)
	mux.HandleFunc("GET /{$}", s.page)
	_, port, _ := net.SplitHostPort(s.cfg.Listen) // an empty port matches no Host: closed
	return Guard(mux, port, s.token)
}

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := s.layout.ExecuteTemplate(w, "layout.html", struct{ Token string }{s.token}); err != nil {
		http.Error(w, "Could not render the page.", http.StatusInternalServerError)
	}
}
