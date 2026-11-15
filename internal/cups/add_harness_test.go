// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package cups_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nemesis398/skra/internal/cups"
	"github.com/nemesis398/skra/internal/testenv"
)

const added = "Virtual-MFC-3"

// TestAddDriverless adds a queue for the harness printer with cupsd's
// driverless driver, reads it back with what the printer told cupsd, and
// removes it. The queue is deleted at the end whatever happens.
func TestAddDriverless(t *testing.T) {
	c := testenv.Client(t)
	ctx := ctxFor(t)
	t.Cleanup(func() { testenv.Command("lpadmin", "-x", added).Run() }) // gone already is fine

	err := c.Add(ctx, cups.AddSpec{Name: added, DeviceURI: "ipp://localhost:8631/ipp/print", Info: "Third virtual MFC", Location: "Harness"})
	if err != nil {
		t.Fatal(err)
	}
	p := printer(t, c, added)
	if p.DeviceURI != "ipp://localhost:8631/ipp/print" || p.Info != "Third virtual MFC" || p.Location != "Harness" {
		t.Errorf("queue = %+v", p)
	}
	if !p.Accepting || p.State != 3 {
		t.Errorf("accepting %v, state %d; want a queue that takes jobs and is idle", p.Accepting, p.State)
	}
	if !slices.Contains(p.SidesSupported, "two-sided-long-edge") {
		t.Errorf("sides-supported = %v: the queue was not made from what the printer reports", p.SidesSupported)
	}
	if p.Shared {
		t.Error("the queue is shared though that was not asked")
	}

	if err := c.Delete(ctx, added); err != nil {
		t.Fatal(err)
	}
	ps, err := c.Printers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(ps, func(p cups.Printer) bool { return p.Name == added }) {
		t.Error("Delete left the queue")
	}
}

// TestAddAsDefault makes the new queue the default and puts the old default
// back. It holds the default lock because the default is one setting of the
// one CUPS.
func TestAddAsDefault(t *testing.T) {
	testenv.LockDefault(t)
	c := testenv.Client(t)
	ctx := ctxFor(t)
	was, err := c.Default(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if was != "" {
			testenv.Command("lpadmin", "-d", was).Run()
		}
		testenv.Command("lpadmin", "-x", added).Run()
	})
	if err := c.Add(ctx, cups.AddSpec{Name: added, DeviceURI: "ipp://localhost:8631/ipp/print", Default: true}); err != nil {
		t.Fatal(err)
	}
	if now, err := c.Default(ctx); err != nil || now != added {
		t.Errorf("default = %q (%v), want %s", now, err, added)
	}
}

// TestPPDsNotFound asks for the drivers of a maker CUPS has none from.
func TestPPDsNotFound(t *testing.T) {
	c := testenv.Client(t)
	ps, err := c.PPDs(ctxFor(t), "", "Zzyzx Imaginary Works")
	if err != nil {
		t.Fatalf("an unknown maker is an error: %v", err)
	}
	if len(ps) != 0 {
		t.Errorf("ppds = %+v, want none", ps)
	}
}

// TestPPDsByMake lists the drivers of a maker CUPS ships.
func TestPPDsByMake(t *testing.T) {
	c := testenv.Client(t)
	ps, err := c.PPDs(ctxFor(t), "", "Generic")
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) == 0 {
		t.Fatal("CUPS lists no Generic driver")
	}
	for _, p := range ps {
		if p.Name == "" || !strings.Contains(strings.ToLower(p.MakeModel), "generic") {
			t.Errorf("driver %+v does not belong to the maker", p)
		}
	}
}

// TestDevicesShape searches for two seconds. What the backends find in the
// container is not fixed, so only the shape is checked.
func TestDevicesShape(t *testing.T) {
	c := testenv.Client(t)
	start := time.Now()
	devs, err := c.Devices(ctxFor(t), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 20*time.Second {
		t.Errorf("the search took %v", took)
	}
	for _, d := range devs {
		if !strings.Contains(d.URI, "://") {
			t.Errorf("device %+v has no scheme", d)
		}
	}
	t.Logf("%d devices in %v", len(devs), time.Since(start))
}

// TestAddressCredentialsAreNotReported adds a queue whose address carries a
// user and password, and reads it back. cupsd 2.4 keeps the credentials for
// the backend but reports the address without user[:password]@ in
// CUPS-Get-Printers, so they never reach the page. skrá relies on that.
func TestAddressCredentialsAreNotReported(t *testing.T) {
	const name = "skra-test-userinfo"
	c := testenv.Client(t)
	ctx := ctxFor(t)
	t.Cleanup(func() { testenv.Command("lpadmin", "-x", name).Run() })
	err := c.Add(ctx, cups.AddSpec{Name: name, DeviceURI: "ipp://u:secret@printer.invalid/ipp/print", PPD: "drv:///sample.drv/generic.ppd"})
	if err != nil {
		t.Fatal(err)
	}
	if p := printer(t, c, name); p.DeviceURI != "ipp://printer.invalid/ipp/print" {
		t.Errorf("device-uri = %q, want it without the credentials", p.DeviceURI)
	}
}
