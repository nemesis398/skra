// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
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
	// Locked disables the level buttons, because cupsd.conf is managed or
	// CUPS did not say which level it sets. Hint says which of the two.
	Locked bool
	Hint   string // beside the switch
	// User is the configured user. AskPassword is set when the switch is not
	// locked and User is not root: CUPS then takes a change of cupsd.conf only
	// with the user's password, so picking a level opens a row that asks for
	// it.
	User        string
	AskPassword bool
	Lines       []logLine
	Error       string // why there are no lines
	Count       int
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
	case s.cfg.CupsdConfManaged:
		v.Locked, v.Hint = true, managedText
	case err != nil:
		v.Locked, v.Hint = true, err.Error()
	case slices.Contains(logLevels, level):
		v.Hint = "CUPS restarts its scheduler for a moment when this changes."
	default:
		v.Hint = "cupsd.conf sets " + level + ". Pick one of these to change it."
	}
	for _, l := range logLevels {
		v.Levels = append(v.Levels, choice{Value: l, Label: l, On: err == nil && l == level})
	}
	v.User, v.AskPassword = s.cfg.User, !v.Locked && s.cfg.User != "root"

	lines, err := s.readLog(ctx, logLines)
	switch {
	case err != nil:
		v.Error = err.Error()
	default:
		for _, l := range lines {
			v.Lines = append(v.Lines, splitStamp(l))
		}
	}

	s.fragment(w, "log", v, "Could not render the log.")
}

// setLogLevel changes the LogLevel of cupsd.conf through CUPS and answers when
// CUPS is back. The scheduler restarts for it, so it can take several seconds.
//
// The form field password goes to CUPS with the one PUT and nowhere else: the
// history records the levels, the answers are fixed texts or CUPS's errors,
// which never carry it, and nothing is logged.
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
	password := r.PostFormValue("password")
	if err := cups.CheckPassword(s.cfg.User, password); err != nil {
		reply(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), actionTimeout)
	defer cancel()
	old, err := s.deps.CUPS.LogLevel(ctx)
	if err != nil {
		old = "?"
	} else {
		err = s.deps.CUPS.SetLogLevel(ctx, level, password)
	}
	// The change is made when CUPS only waits for a job before it uses it.
	waits := errors.Is(err, cups.ErrReloadWaits)
	if waits {
		err = nil
	}
	s.record(r, "cups.loglevel", "cupsd.conf", old+" → "+level, err)
	s.hub.Refresh()
	switch {
	case err != nil:
		reply(w, http.StatusBadGateway, err.Error())
	case waits:
		reply(w, http.StatusOK, cups.ErrReloadWaits.Error())
	default:
		reply(w, http.StatusOK, logSaid[level])
	}
}
