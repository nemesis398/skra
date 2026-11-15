// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

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

// tokenForm is a token: 32 bytes as unpadded base64url.
var tokenForm = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

func TestNewToken(t *testing.T) {
	a, b := NewToken(), NewToken()
	if !tokenForm.MatchString(a) {
		t.Errorf("token %q is not 32 bytes of base64url", a)
	}
	if a == b {
		t.Error("two tokens are equal")
	}
}

// Without a token to compare with, the guard lets no write through: an empty
// header must not match an empty token.
func TestGuardWithoutATokenRefusesEveryWrite(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := Guard(ok, "7631", "")
	for _, hdr := range []map[string]string{nil, {"X-Skra-Token": ""}} {
		if got := do(h, "POST", "/x", "localhost:7631", hdr); got != 403 {
			t.Errorf("POST with %v: status %d, want 403", hdr, got)
		}
	}
	if got := do(h, "GET", "/x", "localhost:7631", nil); got != 200 {
		t.Errorf("GET: status %d, want 200", got)
	}
}

func TestLoadTokenMakesOneAndKeepsIt(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !tokenForm.MatchString(a) {
		t.Errorf("token %q is not 32 bytes of base64url", a)
	}
	fi, err := os.Stat(filepath.Join(dir, "token"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("token file mode %v, want 0600", fi.Mode().Perm())
	}
	b, err := LoadToken(dir)
	if err != nil || b != a {
		t.Errorf("second load = %q, %v; want the first token %q", b, err, a)
	}
}

// A page that outlived the process, which exited when idle, still holds a
// token the next process accepts.
func TestTokenSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{Listen: "127.0.0.1:7631", User: "mika", StateDir: dir}
	tok, err := LoadToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	first := New(cfg, Deps{CUPS: &fakeCUPS{}}, tok)
	page := get(first.Handler(), "/").Body.String()
	m := regexp.MustCompile(`<meta name="skra-token" content="([^"]+)">`).FindStringSubmatch(page)
	if m == nil {
		t.Fatal("the page carries no token")
	}

	tok2, err := LoadToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	second := New(cfg, Deps{CUPS: &fakeCUPS{}}, tok2)
	req := httptest.NewRequest("POST", "/jobs/12/hold", nil)
	req.Host = "localhost:7631"
	req.Header.Set("X-Skra-Token", m[1])
	rec := httptest.NewRecorder()
	second.Handler().ServeHTTP(rec, req)
	if rec.Code == 403 {
		t.Errorf("the second server refused the first one's token: %q", rec.Body.String())
	}
}

func TestLoadTokenRefusesABadFile(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		mode          os.FileMode
		want          string
	}{
		{"empty", "", 0o600, "is empty"},
		{"short", "c2hvcnQ", 0o600, "too short"},
		{"not base64url", strings.Repeat("!", 43), 0o600, "not a token"},
		{"too long", strings.Repeat("A", 2048), 0o600, "too long"},
		{"readable by others", NewToken(), 0o644, "readable by others"},
		{"readable by the group", NewToken(), 0o640, "readable by others"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "token")
			if err := os.WriteFile(path, []byte(tc.content), tc.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tc.mode); err != nil {
				t.Fatal(err)
			}
			tok, err := LoadToken(dir)
			if err == nil {
				t.Fatalf("loaded %q from a bad file", tok)
			}
			if msg := err.Error(); !strings.Contains(msg, tc.want) || !strings.Contains(msg, path) || !strings.Contains(msg, "delete it") {
				t.Errorf("error %q does not say %q, the path and how to fix it", msg, tc.want)
			}
		})
	}
}

// A token path that is not a regular file is refused without following a
// symlink and without waiting on a FIFO for a writer.
func TestLoadTokenRefusesWhatIsNotAFile(t *testing.T) {
	for name, create := range map[string]func(path string) error{
		"a symlink to a good token": func(path string) error {
			good := filepath.Join(t.TempDir(), "good")
			if err := os.WriteFile(good, []byte(NewToken()), 0o600); err != nil {
				return err
			}
			return os.Symlink(good, path)
		},
		"a dangling symlink": func(path string) error { return os.Symlink(filepath.Join(t.TempDir(), "gone"), path) },
		"a directory":        func(path string) error { return os.Mkdir(path, 0o700) },
		"a FIFO":             func(path string) error { return syscall.Mkfifo(path, 0o600) },
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "token")
			if err := create(path); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { _, err := LoadToken(dir); done <- err }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("loaded a token from it")
				}
				if msg := err.Error(); !strings.Contains(msg, "is not a regular file") || !strings.Contains(msg, path) || !strings.Contains(msg, "delete it") {
					t.Errorf("error %q does not say it is not a regular file, the path and how to fix it", msg)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("LoadToken hung")
			}
		})
	}
}

func newTestServer() *Server {
	return New(config.Config{Listen: "127.0.0.1:7631"}, Deps{}, NewToken())
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

// wantCSP lets the page run its own script and nothing else, load no plugin,
// set no base, post no form, and be framed by nobody. Styles are not
// restricted: the ink bars set their width inline.
const wantCSP = "script-src 'self'; object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

func TestGuardSetsTheSecurityHeaders(t *testing.T) {
	h := guarded()
	for _, tc := range []struct {
		name, method, host, site string
		want                     int
	}{
		{"a 200", "GET", "localhost:7631", "", 200},
		{"a 403 for the host", "GET", "evil.example:7631", "", 403},
		{"a 403 for the token", "POST", "localhost:7631", "", 403},
		{"a 403 for another site", "GET", "localhost:7631", "cross-site", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/x", nil)
			req.Host = tc.host
			if tc.site != "" {
				req.Header.Set("Sec-Fetch-Site", tc.site)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d", rec.Code, tc.want)
			}
			if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
				t.Errorf("X-Frame-Options %q, want DENY", got)
			}
			if got := rec.Header().Get("Content-Security-Policy"); got != wantCSP {
				t.Errorf("Content-Security-Policy %q, want %q", got, wantCSP)
			}
			if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options %q, want nosniff", got)
			}
		})
	}
}

// Another site may link to a tab, but read nothing: a cross-site or
// same-site request is refused unless it is a top-level navigation to one of
// the three pages. A request without Sec-Fetch headers passes; the token
// still guards every write.
func TestGuardRefusesCrossSiteReads(t *testing.T) {
	nav := func(site string) map[string]string {
		return map[string]string{"Sec-Fetch-Site": site, "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}
	}
	fetch := func(site string) map[string]string {
		return map[string]string{"Sec-Fetch-Site": site, "Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty"}
	}
	h := guarded()
	for _, tc := range []struct {
		name, method, path string
		hdr                map[string]string
		want               int
	}{
		{"cross-site event stream", "GET", "/events", fetch("cross-site"), 403},
		{"cross-site log", "GET", "/printers/log", fetch("cross-site"), 403},
		{"cross-site add panel", "GET", "/printers/new?uri=ipp://x.local/", fetch("cross-site"), 403},
		{"cross-site script", "GET", "/static/skra.js", map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "no-cors", "Sec-Fetch-Dest": "script"}, 403},
		{"same-site event stream", "GET", "/events", fetch("same-site"), 403},
		{"cross-site navigation to the overview", "GET", "/", nav("cross-site"), 200},
		{"cross-site navigation to printers", "GET", "/printers", nav("cross-site"), 200},
		{"same-site navigation to history", "GET", "/history", nav("same-site"), 200},
		{"cross-site navigation to the log", "GET", "/printers/log", nav("cross-site"), 403},
		{"cross-site navigation to events", "GET", "/events", nav("cross-site"), 403},
		{"cross-site frame of the overview", "GET", "/", map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "iframe"}, 403},
		{"cross-site form post to a page", "POST", "/", nav("cross-site"), 403},
		{"same-origin event stream", "GET", "/events", fetch("same-origin"), 200},
		{"typed into the address bar", "GET", "/printers/log", nav("none"), 200},
		{"no Sec-Fetch headers", "GET", "/events", nil, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := do(h, tc.method, tc.path, "localhost:7631", tc.hdr); got != tc.want {
				t.Errorf("status %d, want %d", got, tc.want)
			}
		})
	}
}
