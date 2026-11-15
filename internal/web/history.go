// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import "net/http"

// historyPage draws the history tab: the jobs CUPS has finished, with the
// reprint button of the overview's last prints. The hub's completed-jobs read
// keeps it current; the page does no polling of its own.
func (s *Server) historyPage(w http.ResponseWriter, r *http.Request) { s.render(w, r, "history") }
