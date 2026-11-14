// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/nemesis398/skra/internal/config"
)

const testToken = "0123456789abcdef"

func guarded() http.Handler {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return Guard(ok, "7631", testToken)
}

func do(h http.Handler, method, path, host string, hdr map[string]string) int {
	req := httptest.NewRequest(method, path, nil)
	req.Host = host
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestGuard(t *testing.T) {
	tok := map[string]string{"X-Skra-Token": testToken}
	tests := []struct {
		name   string
		method string
		host   string
		hdr    map[string]string
		want   int
	}{
		{"GET with localhost", "GET", "localhost:7631", nil, 200},
		{"GET with 127.0.0.1", "GET", "127.0.0.1:7631", nil, 200},
		{"GET with no Origin (EventSource)", "GET", "localhost:7631", nil, 200},
		{"GET with the same-origin Origin (EventSource)", "GET", "localhost:7631", map[string]string{"Origin": "http://localhost:7631"}, 200},
		{"GET with a foreign host (DNS rebinding)", "GET", "evil.example:7631", nil, 403},
		{"GET with the right name on another port", "GET", "localhost:80", nil, 403},
		{"GET without a port", "GET", "localhost", nil, 403},
		{"GET with an empty host", "GET", "", nil, 403},
		{"POST without a token", "POST", "localhost:7631", nil, 403},
		{"POST with a wrong token", "POST", "localhost:7631", map[string]string{"X-Skra-Token": "nope"}, 403},
		{"POST with an empty token", "POST", "localhost:7631", map[string]string{"X-Skra-Token": ""}, 403},
		{"POST with token and same-origin Origin", "POST", "localhost:7631", map[string]string{"X-Skra-Token": testToken, "Origin": "http://localhost:7631"}, 200},
		{"POST with token and a foreign Origin", "POST", "localhost:7631", map[string]string{"X-Skra-Token": testToken, "Origin": "http://evil.example"}, 403},
		{"POST with token and the other loopback name as Origin", "POST", "localhost:7631", map[string]string{"X-Skra-Token": testToken, "Origin": "http://127.0.0.1:7631"}, 403},
		{"POST with token and an https Origin", "POST", "localhost:7631", map[string]string{"X-Skra-Token": testToken, "Origin": "https://localhost:7631"}, 403},
		{"POST with token and Origin null", "POST", "localhost:7631", map[string]string{"X-Skra-Token": testToken, "Origin": "null"}, 403},
		{"POST with token and no Origin", "POST", "localhost:7631", tok, 200},
		{"POST with token and a foreign host", "POST", "evil.example:7631", tok, 403},
		{"DELETE without a token", "DELETE", "localhost:7631", nil, 403},
		{"PUT without a token", "PUT", "localhost:7631", nil, 403},
		{"PATCH without a token", "PATCH", "localhost:7631", nil, 403},
	}
	h := guarded()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := do(h, tc.method, "/x", tc.host, tc.hdr); got != tc.want {
				t.Errorf("%s %s: status %d, want %d", tc.method, tc.host, got, tc.want)
			}
		})
	}
}

func TestNewToken(t *testing.T) {
	a, b := NewToken(), NewToken()
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(a) {
		t.Errorf("token %q is not 32 bytes of hex", a)
	}
	if a == b {
		t.Error("two tokens are equal")
	}
}

func newTestServer() *Server {
	return New(config.Config{Listen: "127.0.0.1:7631"}, Deps{})
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	req.Host = "localhost:7631"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestServerPage(t *testing.T) {
	s := newTestServer()
	rec := get(s.Handler(), "/")
	if rec.Code != 200 {
		t.Fatalf("GET /: status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<meta name="skra-token" content="` + s.token + `">`,
		`class="brand"`, "skrá", "Overview", "Printers", "History",
		`href="http://localhost:631/admin"`,
		`id="need"`, `id="clock"`, `id="msg"`, `id="fresh"`,
		`/static/skra.css`, `/static/skra.js`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content type %q", ct)
	}
}

func TestServerStaticAndGuard(t *testing.T) {
	s := newTestServer()
	h := s.Handler()
	for _, p := range []string{"/static/skra.css", "/static/skra.js"} {
		if rec := get(h, p); rec.Code != 200 || rec.Body.Len() == 0 {
			t.Errorf("GET %s: status %d, %d bytes", p, rec.Code, rec.Body.Len())
		}
	}
	if rec := get(h, "/nope"); rec.Code != 404 {
		t.Errorf("GET /nope: status %d, want 404", rec.Code)
	}
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "evil.example:7631"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 403 || strings.Contains(rec.Body.String(), s.token) {
		t.Errorf("foreign host: status %d", rec.Code)
	}
}

func TestGuardForbidsFraming(t *testing.T) {
	h := guarded()
	for _, tc := range []struct {
		name, method, host string
		want               int
	}{
		{"a 200", "GET", "localhost:7631", 200},
		{"a 403 for the host", "GET", "evil.example:7631", 403},
		{"a 403 for the token", "POST", "localhost:7631", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/x", nil)
			req.Host = tc.host
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d", rec.Code, tc.want)
			}
			if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
				t.Errorf("X-Frame-Options %q, want DENY", got)
			}
			if got := rec.Header().Get("Content-Security-Policy"); got != "frame-ancestors 'none'" {
				t.Errorf("Content-Security-Policy %q, want frame-ancestors 'none'", got)
			}
		})
	}
}
