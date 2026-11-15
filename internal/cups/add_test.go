// SPDX-License-Identifier: AGPL-3.0-or-later

package cups

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nemesis398/skra/internal/ipp"
	"github.com/nemesis398/skra/internal/ipp/ipptest"
)

// script is a fake CUPS that answers each request with the next reply of the
// list (the last one repeats) and keeps every request it got.
type script struct {
	mu   sync.Mutex
	reqs []seen
}

func (s *script) all() []seen {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]seen(nil), s.reqs...)
}

func scripted(t *testing.T, replies ...*ipptest.Wire) (*Client, *script) {
	t.Helper()
	sc := &script{}
	sock := fake(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req, _ := ipp.Decode(body)
		sc.mu.Lock()
		i := min(len(sc.reqs), len(replies)-1)
		sc.reqs = append(sc.reqs, seen{path: r.URL.EscapedPath(), op: uint16(body[2])<<8 | uint16(body[3]), req: req})
		sc.mu.Unlock()
		reply(w, replies[i])
	})
	return New(sock, "mika"), sc
}

func TestDevicesAskWithATimeoutAndDropURIsWithoutAScheme(t *testing.T) {
	c, sc := scripted(t, ipptest.NewWire(0).
		Group(ipp.GroupPrinter).
		Str(ipp.TagText, "device-info", "Brother DCP-L2530DW").
		Str(ipp.TagText, "device-make-and-model", "Brother DCP-L2530DW series").
		Str(ipp.TagURI, "device-uri", "dnssd://Brother%20DCP-L2530DW._ipp._tcp.local./?uuid=1").
		Str(ipp.TagText, "device-id", "MFG:Brother;MDL:DCP-L2530DW;").
		Str(ipp.TagKeyword, "device-class", "network").
		Group(ipp.GroupPrinter).
		Str(ipp.TagText, "device-info", "HP Printer (HPLIP)").
		Str(ipp.TagURI, "device-uri", "hp").
		Str(ipp.TagKeyword, "device-class", "direct").
		Group(ipp.GroupPrinter).
		Str(ipp.TagText, "device-info", "AppSocket/HP JetDirect").
		Str(ipp.TagURI, "device-uri", "socket").
		Group(ipp.GroupPrinter).
		Str(ipp.TagText, "device-info", "USB Printer").
		Str(ipp.TagURI, "device-uri", "usb://Brother/HL-L2350DW?serial=1").
		Str(ipp.TagKeyword, "device-class", "direct"))
	devs, err := c.Devices(context.Background(), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	want := []Device{
		{URI: "dnssd://Brother%20DCP-L2530DW._ipp._tcp.local./?uuid=1", Info: "Brother DCP-L2530DW", MakeModel: "Brother DCP-L2530DW series",
			ID: "MFG:Brother;MDL:DCP-L2530DW;", Class: "network"},
		{URI: "usb://Brother/HL-L2350DW?serial=1", Info: "USB Printer", Class: "direct"},
	}
	if !slices.Equal(devs, want) {
		t.Errorf("devices = %+v\nwant      %+v", devs, want)
	}
	reqs := sc.all()
	if len(reqs) != 1 || reqs[0].op != ipp.OpCUPSGetDevices {
		t.Fatalf("requests = %+v", reqs)
	}
	op := reqs[0].group(ipp.GroupOperation)
	if n, ok := op.Int("timeout"); !ok || n != 10 {
		t.Errorf("timeout = %d (%v), want 10", n, ok)
	}
	if reqs[0].path != "/admin/" {
		t.Errorf("path = %q", reqs[0].path)
	}
}

func TestDevicesWithNothingFoundIsEmpty(t *testing.T) {
	c, _ := scripted(t, ipptest.NewWire(0))
	devs, err := c.Devices(context.Background(), 2*time.Second)
	if err != nil || len(devs) != 0 {
		t.Errorf("devices = %+v, err %v", devs, err)
	}
}

func TestDevicesReturnTheCUPSError(t *testing.T) {
	c, _ := scripted(t, ipptest.NewWire(0x0403).Str(ipp.TagText, "status-message", "Forbidden"))
	if _, err := c.Devices(context.Background(), time.Second); err == nil || err.Error() != "Forbidden" {
		t.Errorf("err = %v", err)
	}
}

func TestPPDsFilterByDeviceIDElseByMake(t *testing.T) {
	c, sc := scripted(t, ipptest.NewWire(0).
		Group(ipp.GroupPrinter).
		Str(ipp.TagName, "ppd-name", "drv:///brother.drv/dcpl2530dw.ppd").
		Str(ipp.TagText, "ppd-make-and-model", "Brother DCP-L2530DW, using brlaser v6").
		Group(ipp.GroupPrinter).
		Str(ipp.TagName, "ppd-name", "everywhere"))
	ps, err := c.PPDs(context.Background(), "MFG:Brother;MDL:DCP-L2530DW;", "Brother")
	if err != nil {
		t.Fatal(err)
	}
	want := []PPD{
		{Name: "drv:///brother.drv/dcpl2530dw.ppd", MakeModel: "Brother DCP-L2530DW, using brlaser v6"},
		{Name: "everywhere"},
	}
	if !slices.Equal(ps, want) {
		t.Errorf("ppds = %+v", ps)
	}
	if _, err := c.PPDs(context.Background(), "", "Brother"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PPDs(context.Background(), "", ""); err != nil {
		t.Fatal(err)
	}
	reqs := sc.all()
	if len(reqs) != 3 {
		t.Fatalf("%d requests", len(reqs))
	}
	for i, wantAttr := range []struct{ id, make string }{{"MFG:Brother;MDL:DCP-L2530DW;", ""}, {"", "Brother"}, {"", ""}} {
		if reqs[i].op != ipp.OpCUPSGetPPDs || reqs[i].path != "/" {
			t.Errorf("request %d: op %#x on %q", i, reqs[i].op, reqs[i].path)
		}
		op := reqs[i].group(ipp.GroupOperation)
		if op.String("ppd-device-id") != wantAttr.id || op.String("ppd-make") != wantAttr.make {
			t.Errorf("request %d: ppd-device-id %q, ppd-make %q, want %q and %q", i, op.String("ppd-device-id"), op.String("ppd-make"), wantAttr.id, wantAttr.make)
		}
		if !slices.Equal(op.Strings("requested-attributes"), []string{"ppd-name", "ppd-make-and-model"}) {
			t.Errorf("request %d: requested-attributes = %v", i, op.Strings("requested-attributes"))
		}
	}
}

func TestPPDsNotFoundIsAnEmptyList(t *testing.T) {
	c, _ := scripted(t, ipptest.NewWire(ipp.StatusNotFound).Str(ipp.TagText, "status-message", "No destinations added."))
	ps, err := c.PPDs(context.Background(), "", "Nobody")
	if err != nil || len(ps) != 0 {
		t.Errorf("ppds = %+v, err %v; want an empty list", ps, err)
	}
}

func TestPPDsOtherErrorsComeBack(t *testing.T) {
	c, _ := scripted(t, ipptest.NewWire(0x0400).Str(ipp.TagText, "status-message", "Bad request"))
	if _, err := c.PPDs(context.Background(), "", "Brother"); err == nil || err.Error() != "Bad request" {
		t.Errorf("err = %v", err)
	}
}

func TestAddSendsTheQueueAndTheDriverOptions(t *testing.T) {
	c, sc := scripted(t, ipptest.NewWire(0))
	err := c.Add(context.Background(), AddSpec{Name: "Büro_Laser", DeviceURI: "ipp://printer.local/ipp/print", Info: "Brother im Büro",
		Location: "Study", PPD: "drv:///brother.drv/dcpl2530dw.ppd", Shared: true})
	if err != nil {
		t.Fatal(err)
	}
	reqs := sc.all()
	if len(reqs) != 1 {
		t.Fatalf("%d requests", len(reqs))
	}
	r := reqs[0]
	if r.op != ipp.OpCUPSAddModifyPrinter || r.path != "/admin/" {
		t.Errorf("op %#x on %q", r.op, r.path)
	}
	op := r.group(ipp.GroupOperation)
	if op.String("printer-uri") != "ipp://localhost/printers/B%C3%BCro_Laser" {
		t.Errorf("printer-uri = %q", op.String("printer-uri"))
	}
	if op.String("ppd-name") != "drv:///brother.drv/dcpl2530dw.ppd" || op.String("requesting-user-name") != "mika" {
		t.Errorf("operation group = %+v", op.Attrs)
	}
	for _, a := range op.Attrs {
		if a.Name == "ppd-name" && a.Tag != ipp.TagName {
			t.Errorf("ppd-name tag = %#x, want name", a.Tag)
		}
	}
	pg := r.group(ipp.GroupPrinter)
	for name, want := range map[string]struct {
		tag ipp.Tag
		val string
	}{
		"device-uri":       {ipp.TagURI, "ipp://printer.local/ipp/print"},
		"printer-info":     {ipp.TagText, "Brother im Büro"},
		"printer-location": {ipp.TagText, "Study"},
	} {
		i := slices.IndexFunc(pg.Attrs, func(a ipp.Attr) bool { return a.Name == name })
		if i < 0 || pg.Attrs[i].Tag != want.tag || pg.String(name) != want.val {
			t.Errorf("%s = %+v, want tag %#x %q", name, pg.Attrs, want.tag, want.val)
		}
	}
	for name, want := range map[string]bool{"printer-is-shared": true, "printer-is-accepting-jobs": true} {
		if got, ok := pg.Bool(name); !ok || got != want {
			t.Errorf("%s = %v (%v), want %v", name, got, ok, want)
		}
	}
	if n, ok := pg.Int("printer-state"); !ok || n != 3 {
		t.Errorf("printer-state = %d (%v), want 3", n, ok)
	}
	if i := slices.IndexFunc(pg.Attrs, func(a ipp.Attr) bool { return a.Name == "printer-state" }); i < 0 || pg.Attrs[i].Tag != ipp.TagEnum {
		t.Error("printer-state is not an enum")
	}
}

func TestAddWithoutAPPDIsDriverless(t *testing.T) {
	c, sc := scripted(t, ipptest.NewWire(0))
	if err := c.Add(context.Background(), AddSpec{Name: "Hall", DeviceURI: "ipp://hall.local/ipp/print"}); err != nil {
		t.Fatal(err)
	}
	r := sc.all()[0]
	if got := r.group(ipp.GroupOperation).String("ppd-name"); got != "everywhere" {
		t.Errorf("ppd-name = %q, want everywhere", got)
	}
	if shared, ok := r.group(ipp.GroupPrinter).Bool("printer-is-shared"); !ok || shared {
		t.Errorf("printer-is-shared = %v (%v), want false and present", shared, ok)
	}
}

func TestAddMakesTheDefaultOnlyWhenAsked(t *testing.T) {
	c, sc := scripted(t, ipptest.NewWire(0))
	if err := c.Add(context.Background(), AddSpec{Name: "Hall", DeviceURI: "ipp://hall.local/ipp/print"}); err != nil {
		t.Fatal(err)
	}
	if n := len(sc.all()); n != 1 {
		t.Fatalf("%d requests without Default, want 1", n)
	}
	if err := c.Add(context.Background(), AddSpec{Name: "Hall2", DeviceURI: "ipp://hall.local/ipp/print", Default: true}); err != nil {
		t.Fatal(err)
	}
	reqs := sc.all()
	if len(reqs) != 3 || reqs[1].op != ipp.OpCUPSAddModifyPrinter || reqs[2].op != ipp.OpCUPSSetDefault {
		t.Fatalf("requests = %+v", reqs)
	}
	if u := reqs[2].group(ipp.GroupOperation).String("printer-uri"); u != "ipp://localhost/printers/Hall2" {
		t.Errorf("SetDefault printer-uri = %q", u)
	}
}

func TestAddDoesNotMakeTheDefaultWhenTheAddFailed(t *testing.T) {
	c, sc := scripted(t, ipptest.NewWire(0x0400).Str(ipp.TagText, "status-message", "The printer did not answer."))
	err := c.Add(context.Background(), AddSpec{Name: "Hall", DeviceURI: "ipp://hall.local/ipp/print", Default: true})
	var late *SetDefaultError
	if err == nil || err.Error() != "The printer did not answer." || errors.As(err, &late) {
		t.Errorf("err = %v", err)
	}
	if n := len(sc.all()); n != 1 {
		t.Errorf("%d requests, want 1", n)
	}
}

func TestAddSaysTheQueueExistsWhenOnlyTheDefaultFailed(t *testing.T) {
	c, _ := scripted(t, ipptest.NewWire(0), ipptest.NewWire(0x0403).Str(ipp.TagText, "status-message", "Not allowed."))
	err := c.Add(context.Background(), AddSpec{Name: "Hall", DeviceURI: "ipp://hall.local/ipp/print", Default: true})
	var late *SetDefaultError
	if !errors.As(err, &late) {
		t.Fatalf("err = %v, want a *SetDefaultError", err)
	}
	if late.Error() != "the queue was added, but making it the default failed: Not allowed." {
		t.Errorf("message = %q", late.Error())
	}
	var se *ipp.StatusError
	if !errors.As(err, &se) || se.Status != 0x0403 {
		t.Errorf("the IPP status is not reachable through the error: %v", err)
	}
}

func TestAddNeedsAQueueAndAnAddress(t *testing.T) {
	c, sc := scripted(t, ipptest.NewWire(0))
	if err := c.Add(context.Background(), AddSpec{DeviceURI: "ipp://hall.local/ipp/print"}); err == nil {
		t.Error("Add without a name succeeded")
	}
	if err := c.Add(context.Background(), AddSpec{Name: "Hall"}); err == nil {
		t.Error("Add without an address succeeded")
	}
	if n := len(sc.all()); n != 0 {
		t.Errorf("%d requests sent", n)
	}
}
