// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"context"
	"net/http"
	"regexp"
	"slices"

	"github.com/nemesis398/skra/internal/cups"
)

// logLines is how much of the journal the log section shows.
const logLines = 200

const managedText = "cupsd.conf is managed by the system configuration."

// logLevels are the levels the switch offers, in the order it shows them.
var logLevels = []string{"warn", "info", "debug"}

// logSaid is the footer message after a level was set.
var logSaid = map[string]string{
	"warn":  "Logging is at warn.",
	"info":  "Logging is at info.",
	"debug": "Debug logging is on.",
}

func (s *Server) logRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /printers/log", s.logFragment)
	mux.HandleFunc("POST /printers/log/level", s.setLogLevel)
}

// readLog reads the journal through Deps.ReadLog, or cups.ReadLog without one.
func (s *Server) readLog(ctx context.Context, n int) ([]string, error) {
	if s.deps.ReadLog != nil {
		return s.deps.ReadLog(ctx, n)
	}
	return cups.ReadLog(ctx, n)
}

// logLine is a line of the journal split at the time stamp journalctl prints
// first, so the column can be styled. TS keeps its trailing space, so the two
// parts read as the line when they are copied.
type logLine struct{ TS, Rest string }

var stamp = regexp.MustCompile(`^(\d{4}-\d\d-\d\dT\S+ )(.*)$`)

func splitStamp(line string) logLine {
	if m := stamp.FindStringSubmatch(line); m != nil {
		return logLine{TS: m[1], Rest: m[2]}
	}
	return logLine{Rest: line}
}

type logView struct {
	Levels []choice
	Locked bool   // the switch is off: managed, or the level could not be read
	Hint   string // beside the switch
	Lines  []logLine
	Error  string // why there are no lines
	Count  int
}

// logFragment renders the log section: the level switch, the refresh button
// and the last lines of the journal. It answers 200 whatever the journal or
// CUPS say, because the section says it itself.
func (s *Server) logFragment(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), actionTimeout)
	defer cancel()
	v := logView{Count: logLines}

	level, err := s.deps.CUPS.LogLevel(ctx)
	switch {
	case err != nil:
		v.Locked, v.Hint = true, err.Error()
	case s.cfg.CupsdConfManaged:
		v.Locked, v.Hint = true, managedText
	case slices.Contains(logLevels, level):
		v.Hint = "CUPS restarts its scheduler for a moment when this changes."
	default:
		v.Hint = "cupsd.conf sets " + level + ". Pick one of these to change it."
	}
	for _, l := range logLevels {
		v.Levels = append(v.Levels, choice{Value: l, Label: l, On: err == nil && l == level})
	}

	lines, err := s.readLog(ctx, logLines)
	switch {
	case err != nil:
		v.Error = err.Error()
	default:
		for _, l := range lines {
			v.Lines = append(v.Lines, splitStamp(l))
		}
	}

	var buf bytes.Buffer
	if err := s.tpl.ExecuteTemplate(&buf, "log", v); err != nil {
		reply(w, http.StatusInternalServerError, "Could not render the log.")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	buf.WriteTo(w)
}

// setLogLevel changes the LogLevel of cupsd.conf through CUPS and answers when
// CUPS is back. The scheduler restarts for it, so it can take several seconds.
func (s *Server) setLogLevel(w http.ResponseWriter, r *http.Request) {
	if s.cfg.CupsdConfManaged {
		reply(w, http.StatusForbidden, managedText)
		return
	}
	level := r.PostFormValue("level")
	if !slices.Contains(logLevels, level) {
		reply(w, http.StatusBadRequest, "Choose warn, info or debug.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), actionTimeout)
	defer cancel()
	old, err := s.deps.CUPS.LogLevel(ctx)
	if err != nil {
		old = "?"
	} else {
		err = s.deps.CUPS.SetLogLevel(ctx, level)
	}
	s.record(r, "cups.loglevel", "cupsd.conf", old+" → "+level, err)
	s.hub.Refresh()
	if err != nil {
		reply(w, http.StatusBadGateway, err.Error())
		return
	}
	reply(w, http.StatusOK, logSaid[level])
}
