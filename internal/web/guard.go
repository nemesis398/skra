// SPDX-License-Identifier: AGPL-3.0-or-later

// Package web serves skra's pages and its JSON and event endpoints.
package web

import (
	"crypto/subtle"
	"net/http"
)

// Guard protects next from other web pages and from DNS rebinding.
//
// Every request must carry a Host of 127.0.0.1:port or localhost:port, which
// stops a hostile name that resolves to 127.0.0.1 from reading the page.
// Every request that is not GET, HEAD or OPTIONS must also carry the
// X-Skra-Token header equal to token, and an Origin that is absent or equal to
// "http://" + Host. Anything else gets 403. An empty token lets no write
// through: an empty header would match it. Every response, 403s included,
// forbids framing (X-Frame-Options and frame-ancestors).
func Guard(next http.Handler, port, token string) http.Handler {
	hosts := map[string]bool{
		"127.0.0.1:" + port: true,
		"localhost:" + port: true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No page may frame this one: a framed page would pass every check
		// below, and a click could be steered onto a button.
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		if !hosts[r.Host] {
			http.Error(w, "Forbidden: unexpected Host header.", http.StatusForbidden)
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host {
				http.Error(w, "Forbidden: unexpected Origin header.", http.StatusForbidden)
				return
			}
			got := r.Header.Get("X-Skra-Token")
			if token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				http.Error(w, "Forbidden: missing or wrong token. Reload the page.", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
