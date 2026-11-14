// SPDX-License-Identifier: AGPL-3.0-or-later

// Package web serves skra's pages and its JSON and event endpoints.
package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
)

// NewToken returns a fresh per-process token: 32 random bytes as hex.
func NewToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("skra: no randomness: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// Guard protects next from other web pages and from DNS rebinding.
//
// Every request must carry a Host of 127.0.0.1:port or localhost:port, which
// stops a hostile name that resolves to 127.0.0.1 from reading the page.
// Every request that is not GET, HEAD or OPTIONS must also carry the
// X-Skra-Token header equal to token, and an Origin that is absent or equal to
// "http://" + Host. Anything else gets 403.
func Guard(next http.Handler, port, token string) http.Handler {
	hosts := map[string]bool{
		"127.0.0.1:" + port: true,
		"localhost:" + port: true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				http.Error(w, "Forbidden: missing or wrong token. Reload the page.", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
