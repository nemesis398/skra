// SPDX-License-Identifier: AGPL-3.0-or-later

package escl

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/nemesis398/skra/internal/config"
	"github.com/nemesis398/skra/internal/printer"
)

// The avahi browser is what Find gets in skra.
var _ Browser = printer.Avahi{}

// fakeBrowser answers Browse from a table by registration type.
type fakeBrowser struct {
	found map[string][]Found
	err   map[string]error

	mu    sync.Mutex
	asked []string
}

func (b *fakeBrowser) Browse(ctx context.Context, regtype string) ([]Found, error) {
	b.mu.Lock()
	b.asked = append(b.asked, regtype)
	b.mu.Unlock()
	return b.found[regtype], b.err[regtype]
}

const brotherUUID = "e3248000-80ce-11db-8000-94ddf8f11cb0"

func brother(port uint16) Found {
	return Found{
		Name: "Brother MFC-J6975DW",
		Host: "192.168.178.31",
		Port: port,
		TXT:  map[string]string{"txtvers": "1", "rs": "eSCL", "UUID": brotherUUID, "ty": "Brother MFC-J6975DW", "is": "platen,adf"},
	}
}

func TestFindMergesAndDedupes(t *testing.T) {
	b := &fakeBrowser{found: map[string][]Found{
		"_uscan._tcp": {
			brother(80),
			// Another scanner whose TXT names no resource path and no model.
			{Name: "Old Scanner", Host: "192.168.178.40", Port: 8080, TXT: map[string]string{"uuid": "1111"}},
		},
		"_uscans._tcp": {brother(443)},
	}}
	cfg := []config.Scanner{{Name: "skrá fake scanner", URL: "http://127.0.0.1:8632/eSCL"}}
	got := Find(context.Background(), cfg, b)
	want := []Scanner{
		{Name: "skrá fake scanner", URL: "http://127.0.0.1:8632/eSCL", Declared: true},
		{Name: "Brother MFC-J6975DW", URL: "https://192.168.178.31:443/eSCL", UUID: brotherUUID},
		{Name: "Old Scanner", URL: "http://192.168.178.40:8080/eSCL", UUID: "1111"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("Find =\n%+v\nwant\n%+v", got, want)
	}
	slices.Sort(b.asked)
	if !slices.Equal(b.asked, []string{"_uscan._tcp", "_uscans._tcp"}) {
		t.Errorf("browsed %v", b.asked)
	}
}

// The https announcement wins whichever registration type answers first.
func TestFindPrefersHTTPS(t *testing.T) {
	plain := brother(80)
	plain.TXT = map[string]string{"rs": "/eSCL/", "UUID": brotherUUID}
	b := &fakeBrowser{found: map[string][]Found{
		"_uscans._tcp": {brother(443)},
		"_uscan._tcp":  {plain},
	}}
	got := Find(context.Background(), nil, b)
	if len(got) != 1 || got[0].URL != "https://192.168.178.31:443/eSCL" {
		t.Errorf("Find = %+v", got)
	}
}

// A scanner without a UUID is told apart by its service name.
func TestFindWithoutUUID(t *testing.T) {
	b := &fakeBrowser{found: map[string][]Found{
		"_uscan._tcp":  {{Name: "Canon", Host: "192.168.178.50", Port: 80, TXT: map[string]string{"rs": "eSCL"}}, {Name: "HP", Host: "192.168.178.51", Port: 8080}},
		"_uscans._tcp": {{Name: "Canon", Host: "192.168.178.50", Port: 443, TXT: map[string]string{"rs": "eSCL"}}},
	}}
	got := Find(context.Background(), nil, b)
	want := []Scanner{
		{Name: "Canon", URL: "https://192.168.178.50:443/eSCL"},
		{Name: "HP", URL: "http://192.168.178.51:8080/eSCL"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("Find = %+v, want %+v", got, want)
	}
}

func TestFindKeepsConfiguredOnBrowseError(t *testing.T) {
	b := &fakeBrowser{err: map[string]error{
		"_uscan._tcp":  errors.New("no system bus"),
		"_uscans._tcp": errors.New("no system bus"),
	}}
	cfg := []config.Scanner{{Name: "Brother", URL: "http://192.168.178.31/eSCL"}}
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	got := Find(context.Background(), cfg, b)
	if !slices.Equal(got, []Scanner{{Name: "Brother", URL: "http://192.168.178.31/eSCL", Declared: true}}) {
		t.Errorf("Find = %+v", got)
	}
	// Each failed type is in the log, so a scanner that is missing can be
	// traced to avahi.
	lines := strings.Split(strings.TrimSpace(logged.String()), "\n")
	for _, regtype := range []string{"_uscan._tcp", "_uscans._tcp"} {
		n := 0
		for _, l := range lines {
			if strings.Contains(l, "browsing "+regtype+": no system bus") {
				n++
			}
		}
		if n != 1 {
			t.Errorf("the log has %d lines for %s, want 1:\n%s", n, regtype, logged.String())
		}
	}
	// One type failing does not hide what the other found.
	b = &fakeBrowser{
		found: map[string][]Found{"_uscans._tcp": {brother(443)}},
		err:   map[string]error{"_uscan._tcp": errors.New("avahi failed")},
	}
	if got := Find(context.Background(), nil, b); len(got) != 1 || got[0].Name != "Brother MFC-J6975DW" {
		t.Errorf("Find = %+v", got)
	}
	if got := Find(context.Background(), nil, nil); len(got) != 0 {
		t.Errorf("Find without a browser = %+v", got)
	}
}

// A configured scanner that is also announced is listed once, as configured.
func TestFindConfiguredWinsOverAnnounced(t *testing.T) {
	// Its https announcement does not come back beside it either.
	b := &fakeBrowser{found: map[string][]Found{"_uscan._tcp": {brother(80)}, "_uscans._tcp": {brother(443)}}}
	cfg := []config.Scanner{{Name: "Office", URL: "http://192.168.178.31:80/eSCL"}}
	got := Find(context.Background(), cfg, b)
	if !slices.Equal(got, []Scanner{{Name: "Office", URL: "http://192.168.178.31:80/eSCL", Declared: true}}) {
		t.Errorf("Find = %+v", got)
	}
}

func TestFindKeepsLocalScanners(t *testing.T) {
	b := &fakeBrowser{found: map[string][]Found{"_uscan._tcp": {
		// ipp-usb shares a USB scanner, the Epson at school, on this machine.
		{Name: "EPSON ET-4850 Series", Host: "127.0.0.1", Port: 60000, TXT: map[string]string{"rs": "eSCL", "ty": "EPSON ET-4850 Series", "uuid": "cfe92100-67c4-11d4-a45f-e0bb9e2a9fbf"}},
		// The IPP port of a host is never a scanner skra may speak to.
		{Name: "Own CUPS", Host: "127.0.0.1", Port: 631, TXT: map[string]string{"rs": "eSCL"}},
		{Name: "Other CUPS", Host: "192.168.178.9", Port: 631, TXT: map[string]string{"rs": "eSCL"}},
		{Name: "IPv6", Host: "fe80::1%wlp4s0", Port: 80, TXT: map[string]string{"rs": "eSCL"}},
	}}}
	got := Find(context.Background(), nil, b)
	want := []Scanner{
		{Name: "EPSON ET-4850 Series", URL: "http://127.0.0.1:60000/eSCL", UUID: "cfe92100-67c4-11d4-a45f-e0bb9e2a9fbf"},
		{Name: "IPv6", URL: "http://[fe80::1%25wlp4s0]:80/eSCL"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("Find =\n%+v\nwant\n%+v", got, want)
	}
	for _, s := range got {
		if _, err := New(s.URL); err != nil {
			t.Errorf("New(%q): %v", s.URL, err)
		}
	}
}
