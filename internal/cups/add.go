// SPDX-License-Identifier: AGPL-3.0-or-later

package cups

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/nemesis398/skra/internal/ipp"
)

// Device is a printer address CUPS found.
type Device struct{ URI, Info, MakeModel, ID, Class string }

// PPD is a driver CUPS has.
type PPD struct{ Name, MakeModel string }

// AddSpec is a new queue. An empty PPD makes a driverless queue (CUPS's
// "everywhere" driver); Default also makes it the default queue.
type AddSpec struct {
	Name, DeviceURI, Info, Location, PPD string
	Shared, Default                      bool
}

// driverless is the ppd-name that has cupsd ask the printer what it can do.
const driverless = "everywhere"

// SetDefaultError is what Add returns when the queue was made but making it
// the default failed, so the caller can tell that the queue exists.
type SetDefaultError struct{ Err error }

func (e *SetDefaultError) Error() string {
	return "the queue was added, but making it the default failed: " + e.Err.Error()
}

func (e *SetDefaultError) Unwrap() error { return e.Err }

// Devices asks CUPS to look for printers on the network and the USB bus. CUPS
// asks every backend and answers when the timeout is over, so the call takes
// about that long. A device whose URI has no "://" is a backend's placeholder
// ("hp", "socket", "lpd") and is left out.
func (c *Client) Devices(ctx context.Context, timeout time.Duration) ([]Device, error) {
	req := ipp.NewRequest(ipp.OpCUPSGetDevices)
	if secs := int(timeout.Round(time.Second) / time.Second); secs > 0 {
		req.Op(ipp.Int("timeout", secs))
	}
	req.Op(ipp.Keyword("requested-attributes", "device-uri", "device-info", "device-make-and-model", "device-id", "device-class"))
	res, err := c.Do(ctx, "/admin/", req, nil)
	if err != nil {
		return nil, err
	}
	var out []Device
	for _, g := range res.Of(ipp.GroupPrinter) {
		d := Device{URI: g.String("device-uri"), Info: g.String("device-info"), MakeModel: g.String("device-make-and-model"),
			ID: g.String("device-id"), Class: g.String("device-class")}
		if strings.Contains(d.URI, "://") {
			out = append(out, d)
		}
	}
	return out, nil
}

// PPDs lists the drivers CUPS has for a device, found by its IEEE 1284 id; or,
// without one, the drivers of a maker; or, without either, all of them, which
// are thousands. CUPS answers "not found" when nothing matches, and that is an
// empty list here.
func (c *Client) PPDs(ctx context.Context, deviceID, make string) ([]PPD, error) {
	req := ipp.NewRequest(ipp.OpCUPSGetPPDs)
	switch {
	case deviceID != "":
		req.Op(ipp.Text("ppd-device-id", deviceID))
	case make != "":
		req.Op(ipp.Text("ppd-make", make))
	}
	res, err := c.Do(ctx, "/", req.Op(ipp.Keyword("requested-attributes", "ppd-name", "ppd-make-and-model")), nil)
	var se *ipp.StatusError
	if errors.As(err, &se) && se.Status == ipp.StatusNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []PPD
	for _, g := range res.Of(ipp.GroupPrinter) {
		if name := g.String("ppd-name"); name != "" {
			out = append(out, PPD{Name: name, MakeModel: g.String("ppd-make-and-model")})
		}
	}
	return out, nil
}

// Add creates a queue and, when asked, makes it the default. A driverless
// queue takes a few seconds, because cupsd asks the printer first. When only
// the second step fails, the error is a *SetDefaultError.
func (c *Client) Add(ctx context.Context, spec AddSpec) error {
	if spec.DeviceURI == "" {
		return errors.New("no printer address given")
	}
	ppd := spec.PPD
	if ppd == "" {
		ppd = driverless
	}
	req, err := queueRequest(ipp.OpCUPSAddModifyPrinter, spec.Name)
	if err != nil {
		return err
	}
	req.Op(ipp.Name("ppd-name", ppd)).Printer(
		ipp.URI("device-uri", spec.DeviceURI),
		ipp.Text("printer-info", spec.Info),
		ipp.Text("printer-location", spec.Location),
		ipp.Bool("printer-is-shared", spec.Shared),
		ipp.Bool("printer-is-accepting-jobs", true),
		ipp.Enum("printer-state", 3))
	if _, err := c.Do(ctx, "/admin/", req, nil); err != nil {
		return err
	}
	if spec.Default {
		if err := c.SetDefault(ctx, spec.Name); err != nil {
			return &SetDefaultError{err}
		}
	}
	return nil
}
