// SPDX-License-Identifier: AGPL-3.0-or-later

package printer

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nemesis398/skra/internal/ipp"
)

// wire builds an IPP response by hand, because package ipp only encodes requests.
type wire struct{ b []byte }

func newWire(status uint16) *wire {
	w := &wire{b: []byte{2, 0, byte(status >> 8), byte(status), 0, 0, 0, 1}}
	return w.group(ipp.GroupOperation).
		attr(ipp.TagCharset, "attributes-charset", []byte("utf-8")).
		attr(ipp.TagLanguage, "attributes-natural-language", []byte("en"))
}

func (w *wire) group(g ipp.Group) *wire { w.b = append(w.b, byte(g)); return w }

func (w *wire) attr(tag ipp.Tag, name string, v []byte) *wire {
	w.b = append(w.b, byte(tag))
	w.b = binary.BigEndian.AppendUint16(w.b, uint16(len(name)))
	w.b = append(w.b, name...)
	w.b = binary.BigEndian.AppendUint16(w.b, uint16(len(v)))
	w.b = append(w.b, v...)
	return w
}

func (w *wire) str(tag ipp.Tag, name string, vs ...string) *wire {
	for i, v := range vs {
		n := name
		if i > 0 {
			n = ""
		}
		w.attr(tag, n, []byte(v))
	}
	return w
}

func (w *wire) num(tag ipp.Tag, name string, vs ...int) *wire {
	for i, v := range vs {
		n := name
		if i > 0 {
			n = ""
		}
		w.attr(tag, n, binary.BigEndian.AppendUint32(nil, uint32(int32(v))))
	}
	return w
}

func (w *wire) bytes() []byte { return append(append([]byte(nil), w.b...), byte(ipp.GroupEnd)) }

// inkAnswer is what a four-cartridge printer sends.
func inkAnswer() []byte {
	return newWire(0).group(ipp.GroupPrinter).
		str(ipp.TagName, "marker-names", "Black", "Cyan", "Magenta", "Yellow").
		num(ipp.TagInteger, "marker-levels", 68, 48, 6, 12).
		str(ipp.TagName, "marker-colors", "#000000", "#00FFFF", "#FF00FF", "#FFFF00").
		str(ipp.TagKeyword, "marker-types", "ink-cartridge", "ink-cartridge", "ink-cartridge", "ink-cartridge").
		str(ipp.TagKeyword, "printer-state-reasons", "marker-supply-low-report", "media-empty-warning").
		str(ipp.TagText, "printer-state-message", "Sleeping").bytes()
}

func ippServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}

func answer(b []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/ipp")
		w.Write(b)
	}
}

func TestQueryReadsMarkers(t *testing.T) {
	var got *ipp.Response
	var method, ctype, path string
	s := ippServer(t, func(w http.ResponseWriter, r *http.Request) {
		method, ctype, path = r.Method, r.Header.Get("Content-Type"), r.URL.Path
		body, _ := io.ReadAll(r.Body)
		got, _ = ipp.Decode(body)
		answer(inkAnswer())(w, r)
	})

	live := Query(context.Background(), "ipp://"+s.Listener.Addr().String()+"/ipp/print", nil)

	if !live.Reachable {
		t.Fatal("a printer that answered is not reachable")
	}
	var names, colors []string
	var levels []int
	for _, m := range live.Markers {
		names, colors, levels = append(names, m.Name), append(colors, m.Color), append(levels, m.Level)
	}
	if !slices.Equal(names, []string{"Black", "Cyan", "Magenta", "Yellow"}) ||
		!slices.Equal(colors, []string{"#000000", "#00FFFF", "#FF00FF", "#FFFF00"}) ||
		!slices.Equal(levels, []int{68, 48, 6, 12}) {
		t.Errorf("markers = %+v", live.Markers)
	}
	if live.Markers[0].Type != "ink-cartridge" {
		t.Errorf("type = %q", live.Markers[0].Type)
	}
	if !slices.Equal(live.Reasons, []string{"marker-supply-low-report", "media-empty-warning"}) || live.Message != "Sleeping" {
		t.Errorf("reasons %v, message %q", live.Reasons, live.Message)
	}
	if live.At.IsZero() {
		t.Error("At is not set")
	}

	if method != http.MethodPost || ctype != "application/ipp" || path != "/ipp/print" {
		t.Errorf("sent %s %s to %s", method, ctype, path)
	}
	if got == nil || got.Status != ipp.OpGetPrinterAttributes {
		t.Fatalf("request = %+v, want Get-Printer-Attributes", got)
	}
	op := got.Of(ipp.GroupOperation)[0]
	if op.String("printer-uri") != "ipp://"+s.Listener.Addr().String()+"/ipp/print" {
		t.Errorf("printer-uri = %q", op.String("printer-uri"))
	}
	want := []string{"marker-names", "marker-levels", "marker-colors", "marker-types", "printer-state-reasons", "printer-state-message"}
	if g := op.Strings("requested-attributes"); !slices.Equal(g, want) {
		t.Errorf("requested-attributes = %v, want %v", g, want)
	}
}

func TestQueryUnevenMarkerLists(t *testing.T) {
	// A Brother MFC sends levels and types but no names.
	b := newWire(0).group(ipp.GroupPrinter).
		num(ipp.TagInteger, "marker-levels", 50, 40).
		str(ipp.TagKeyword, "marker-types", "toner", "toner", "opc").bytes()
	s := ippServer(t, answer(b))
	live := Query(context.Background(), "ipp://"+s.Listener.Addr().String()+"/", nil)
	if !live.Reachable || len(live.Markers) != 3 || live.Markers[2].Level != -1 || live.Markers[0].Name != "" {
		t.Errorf("live = %+v", live)
	}
}

func TestQueryUnreachableIsAway(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close() // nothing listens here now

	start := time.Now()
	live := Query(context.Background(), "ipp://"+addr+"/ipp/print", nil)
	if live.Reachable {
		t.Error("a closed port is reachable")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %v", d)
	}
}

func TestQueryGivesUpOnAPrinterThatNeverAnswers(t *testing.T) {
	old := timeout
	timeout = 100 * time.Millisecond
	defer func() { timeout = old }()
	release := make(chan struct{})
	s := ippServer(t, func(http.ResponseWriter, *http.Request) { <-release })
	defer close(release)

	start := time.Now()
	live := Query(context.Background(), "ipp://"+s.Listener.Addr().String()+"/", nil)
	if live.Reachable {
		t.Error("a hung printer is reachable")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("took %v, want about the timeout", d)
	}
}

func TestQueryTimeoutIsTwoSeconds(t *testing.T) {
	if timeout != 2*time.Second {
		t.Errorf("timeout = %v, want 2 s", timeout)
	}
}

func TestQueryFailuresAreAway(t *testing.T) {
	refused := newWire(0x0406).bytes() // client-error-not-found
	for name, h := range map[string]http.HandlerFunc{
		"ipp error":   answer(refused),
		"html":        answer([]byte("<html>nope</html>")),
		"truncated":   answer(inkAnswer()[:12]),
		"empty":       answer(nil),
		"http error":  func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", 500) },
		"http 404":    http.NotFound,
		"no response": func(w http.ResponseWriter, _ *http.Request) { panic(http.ErrAbortHandler) },
	} {
		t.Run(name, func(t *testing.T) {
			s := ippServer(t, h)
			live := Query(context.Background(), "ipp://"+s.Listener.Addr().String()+"/", nil)
			if live.Reachable {
				t.Errorf("reachable: %+v", live)
			}
		})
	}
}

func TestQueryBadURIsAreAway(t *testing.T) {
	for _, uri := range []string{"", "ipp://", "://x", "usb://Brother/MFC", "ipp://host:notaport/", "dnssd://x"} {
		if Query(context.Background(), uri, nil).Reachable {
			t.Errorf("%q reachable", uri)
		}
	}
}

func TestQueryIPPSTrustsASelfSignedCertificate(t *testing.T) {
	s := httptest.NewTLSServer(answer(inkAnswer()))
	defer s.Close()
	live := Query(context.Background(), "ipps://"+s.Listener.Addr().String()+"/ipp/print", nil)
	if !live.Reachable || len(live.Markers) != 4 {
		t.Errorf("live = %+v", live)
	}
}

func TestQueryIPPDoesNotSpeakTLS(t *testing.T) {
	s := ippServer(t, answer(inkAnswer()))
	if Query(context.Background(), "ipps://"+s.Listener.Addr().String()+"/", nil).Reachable {
		t.Error("ipps against a plain server answered")
	}
}

func TestClientsIgnoreTheProxyEnvironment(t *testing.T) {
	for name, c := range map[string]*http.Client{"plain": plainClient, "tls": tlsClient} {
		tr, ok := c.Transport.(*http.Transport)
		if !ok || tr.Proxy != nil || c == http.DefaultClient {
			t.Errorf("%s client: want a dedicated transport without a proxy", name)
		}
	}
	if tr := tlsClient.Transport.(*http.Transport); !tr.TLSClientConfig.InsecureSkipVerify {
		t.Error("the tls client verifies certificates; printers carry self-signed ones")
	}
	if tr := plainClient.Transport.(*http.Transport); tr.TLSClientConfig != nil && tr.TLSClientConfig.InsecureSkipVerify {
		t.Error("the plain client skips verification")
	}
}

var uniques atomic.Int32

// unique makes a dnssd URI no other test (or run of the same test, under
// -count) has used, because the resolution cache is shared by the package.
func unique(format string) string {
	return strings.Replace(format, "%s", strconv.Itoa(int(uniques.Add(1)))+"-"+strconv.FormatInt(time.Now().UnixNano(), 36), 1)
}

type fakeResolver struct {
	uri   string
	err   error
	calls atomic.Int32
}

func (f *fakeResolver) Resolve(context.Context, string) (string, error) {
	f.calls.Add(1)
	return f.uri, f.err
}

func TestQueryResolvesDNSSD(t *testing.T) {
	s := ippServer(t, answer(inkAnswer()))
	r := &fakeResolver{uri: "ipp://" + s.Listener.Addr().String() + "/ipp/print"}
	live := Query(context.Background(), unique("dnssd://Resolves%20Once-%s._ipp._tcp.local/?uuid=1"), r)
	if !live.Reachable || len(live.Markers) != 4 {
		t.Errorf("live = %+v", live)
	}
}

func TestQueryDNSSDWithoutAResolverOrWhenItFailsIsAway(t *testing.T) {
	if Query(context.Background(), "dnssd://No%20Resolver._ipp._tcp.local/", nil).Reachable {
		t.Error("reachable without a resolver")
	}
	r := &fakeResolver{err: errors.New("no such service")}
	if Query(context.Background(), "dnssd://Gone._ipp._tcp.local/", r).Reachable {
		t.Error("reachable although resolving failed")
	}
	if Query(context.Background(), "dnssd://not-a-service/", Avahi{}).Reachable {
		t.Error("reachable from an unparseable dnssd URI")
	}
}

func TestQueryCachesTheResolutionUntilTheQueryFails(t *testing.T) {
	var down atomic.Bool
	s := ippServer(t, func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "away", 503)
			return
		}
		answer(inkAnswer())(w, r)
	})
	r := &fakeResolver{uri: "ipp://" + s.Listener.Addr().String() + "/"}
	uri := unique("dnssd://Cache%20Test-%s._ipp._tcp.local/")
	ctx := context.Background()

	for range 3 {
		if !Query(ctx, uri, r).Reachable {
			t.Fatal("not reachable")
		}
	}
	if n := r.calls.Load(); n != 1 {
		t.Errorf("resolved %d times for three queries, want 1", n)
	}

	down.Store(true)
	if Query(ctx, uri, r).Reachable {
		t.Fatal("reachable while down")
	}
	down.Store(false)
	if !Query(ctx, uri, r).Reachable {
		t.Fatal("not back")
	}
	if n := r.calls.Load(); n != 2 {
		t.Errorf("resolved %d times, want 2: a failed query drops the cache", n)
	}
}

func TestQueryDoesNotCacheAFailedResolution(t *testing.T) {
	r := &fakeResolver{err: errors.New("timeout")}
	uri := unique("dnssd://Never%20Cached-%s._ipp._tcp.local/")
	Query(context.Background(), uri, r)
	Query(context.Background(), uri, r)
	if n := r.calls.Load(); n != 2 {
		t.Errorf("resolved %d times, want 2", n)
	}
}

func TestQueryHonorsTheCallersContext(t *testing.T) {
	release := make(chan struct{})
	s := ippServer(t, func(http.ResponseWriter, *http.Request) { <-release })
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	Query(ctx, "ipp://"+s.Listener.Addr().String()+"/", nil)
	if d := time.Since(start); d > time.Second {
		t.Errorf("took %v after the context ended", d)
	}
}

func TestPrinterURLs(t *testing.T) {
	for in, want := range map[string]string{
		"ipp://printer.local/ipp/print":        "http://printer.local:631/ipp/print",
		"ipps://printer.local/ipp/print":       "https://printer.local:631/ipp/print",
		"ipp://127.0.0.1:8631/ipp/print":       "http://127.0.0.1:8631/ipp/print",
		"ipp://[fe80::1%25eth0]:631/ipp/print": "http://[fe80::1%25eth0]:631/ipp/print",
		"ipp://[2001:db8::5]/ipp/print":        "http://[2001:db8::5]:631/ipp/print",
		"ipp://host/ipp/print?a=b":             "http://host:631/ipp/print?a=b",
		"ipp://host":                           "http://host:631/",
	} {
		got, _, err := httpURL(in)
		if err != nil || got != want {
			t.Errorf("httpURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"usb://x/y", "socket://host:9100", "http://host/", "", "ipp:///x"} {
		if _, _, err := httpURL(in); err == nil {
			t.Errorf("httpURL(%q) accepted", in)
		}
	}
}
