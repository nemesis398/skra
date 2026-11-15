// SPDX-License-Identifier: AGPL-3.0-or-later

package cups

import (
	"net/url"
	"time"
)

// Printer is one CUPS queue with the attributes skra shows and edits.
type Printer struct {
	Name, Info, Location, MakeModel, DeviceURI, Message, ErrorPolicy, SidesDefault, ColorDefault, MediaDefault string
	// State is the IPP printer-state: 3 idle, 4 processing, 5 stopped; 0 when CUPS sent none.
	State, QualityDefault, QueuedJobs                                                int
	Reasons, SidesSupported, ColorSupported, MediaSupported, ErrorPolicies, Commands []string
	QualitySupported                                                                 []int
	Accepting, Shared                                                                bool
	Markers                                                                          []Marker
	// MarkersAt is marker-change-time; zero when CUPS sent none.
	MarkersAt time.Time
}

// Marker is one supply (ink, toner). Level and Low are -1 when CUPS sent no
// value, and also when CUPS sent -1, which IPP uses for "unavailable": the two
// cases share -1 and cannot be told apart. CUPS itself uses -2 for unknown and
// -3 for "some ink left". The overview draws every level below 0 alike, as
// "·" with an empty bar.
type Marker struct {
	Name, Color, Type string
	Level, Low        int
}

// Job is one print job. State is the IPP job-state: 3 pending, 4 held,
// 5 processing, 6 stopped, 7 canceled, 8 aborted, 9 completed.
type Job struct {
	ID, State, Sheets            int
	Name, User, Printer, Message string
	Reasons                      []string
	Created, Completed           time.Time
}

// PrinterURI is the printer-uri CUPS expects for the queue name.
func PrinterURI(name string) string {
	return "ipp://localhost/printers/" + url.PathEscape(name)
}
