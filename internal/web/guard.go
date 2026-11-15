// SPDX-License-Identifier: AGPL-3.0-or-later

// Package web serves skra's pages and its JSON and event endpoints.
package web

import (
	"crypto/subtle"
	"net/http"
)

// contentSecurityPolicy lets a page load scripts from skrá only, no plugin,
// no base URL and no form target, and be framed by nobody.
const contentSecurityPolicy = "script-src 'self'; object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// Guard protects next from other web pages and from DNS rebinding.
//
// Every request must carry a Host of 127.0.0.1:port or localhost:port, which
// stops a hostile name that resolves to 127.0.0.1 from reading the page.
// Every request that is not GET, HEAD or OPTIONS must also carry the
// X-Skra-Token header equal to token, and an Origin that is absent or equal to
// "http://" + Host; an empty token lets no write through, since an empty
// header would match it. A request that the browser marks as coming from
// another site (Sec-Fetch-Site cross-site or same-site) is refused unless it
// is a top-level navigation to one of the pages, so another site can link to
// a tab but cannot read the event stream, the log or a panel; a request
// without Sec-Fetch headers passes, and the token still guards writes.
// Anything else gets 403. Every response, 403s included, forbids framing
// (X-Frame-Options and frame-ancestors) and content sniffing, and carries the
// Content-Security-Policy.
func Guard(next http.Handler, port, token string) http.Handler {
	hosts := map[string]bool{
		"127.0.0.1:" + port: true,
		"localhost:" + port: true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No other page may frame skrá: a framed page would pass every check
		// below, and a click could be steered onto a button. The page runs
		// its own script file and nothing else; styles stay open, because
		// the ink bars set their width inline.
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		if !hosts[r.Host] {
			http.Error(w, "Forbidden: unexpected Host header.", http.StatusForbidden)
			return
		}
		if crossSite(r) && !pageNavigation(r) {
			http.Error(w, "Forbidden: another site may link to skrá's pages but not read from it.", http.StatusForbidden)
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

// crossSite reports whether the browser says the request comes from another
// site. Another port of localhost, such as CUPS's own page, is same-site.
func crossSite(r *http.Request) bool {
	site := r.Header.Get("Sec-Fetch-Site")
	return site == "cross-site" || site == "same-site"
}

// pages are the full pages by path, each with the name render draws it
// under. handlerOn serves them from this list, and they are the paths another
// site may navigate to. Each draws from a fresh poll, so it works before the
// event stream connects; the history tab's rows come from the hub's
// completed-jobs read, with no polling of its own.
var pages = map[string]string{"/": "overview", "/printers": "printers", "/history": "history"}

// pageNavigation reports whether r opens one of the pages as a document of
// its own: a link followed, not a frame, a fetch or a form post.
func pageNavigation(r *http.Request) bool {
	_, page := pages[r.URL.Path]
	return r.Method == http.MethodGet && page &&
		r.Header.Get("Sec-Fetch-Mode") == "navigate" && r.Header.Get("Sec-Fetch-Dest") == "document"
}
