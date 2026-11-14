// SPDX-License-Identifier: AGPL-3.0-or-later

// Package activation hands out the listening socket, from systemd socket
// activation when there is one and from a plain TCP listen otherwise.
package activation

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"syscall"
)

// firstFD is SD_LISTEN_FDS_START: systemd passes sockets from fd 3 on.
const firstFD = 3

// Listener returns the socket systemd passed in fd 3 when LISTEN_PID is this
// process and LISTEN_FDS is at least 1, and true. Otherwise it listens on addr
// over TCP and returns false.
func Listener(addr string) (net.Listener, bool, error) {
	if passedFDs() >= 1 {
		syscall.CloseOnExec(firstFD)
		f := os.NewFile(firstFD, "skra.socket")
		defer f.Close()
		ln, err := net.FileListener(f)
		if err != nil {
			return nil, false, fmt.Errorf("socket activation: %w", err)
		}
		return ln, true, nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, false, fmt.Errorf("listen %s: %w", addr, err)
	}
	return ln, false, nil
}

// passedFDs is the number of sockets systemd passed to this process, or 0
// when the variables are unset, malformed, or meant for another process.
func passedFDs() int {
	pid, err := strconv.Atoi(os.Getenv("LISTEN_PID"))
	if err != nil || pid != os.Getpid() {
		return 0
	}
	n, err := strconv.Atoi(os.Getenv("LISTEN_FDS"))
	if err != nil || n < 0 {
		return 0
	}
	return n
}
