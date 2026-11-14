// SPDX-License-Identifier: AGPL-3.0-or-later

package activation

import (
	"net"
	"os"
	"strconv"
	"testing"
)

func TestListenerFallback(t *testing.T) {
	t.Setenv("LISTEN_FDS", "")
	t.Setenv("LISTEN_PID", "")
	ln, activated, err := Listener("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listener: %v", err)
	}
	defer ln.Close()
	if activated {
		t.Error("activated = true without LISTEN_FDS")
	}
	if _, ok := ln.Addr().(*net.TCPAddr); !ok {
		t.Errorf("Addr = %T, want *net.TCPAddr", ln.Addr())
	}
}

func TestListenerIgnoresForeignPID(t *testing.T) {
	t.Setenv("LISTEN_FDS", "1")
	t.Setenv("LISTEN_PID", strconv.Itoa(os.Getpid()+1))
	ln, activated, err := Listener("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listener: %v", err)
	}
	defer ln.Close()
	if activated {
		t.Error("activated = true for another process's LISTEN_PID")
	}
}

func TestListenerSocketActivated(t *testing.T) {
	if os.Getenv("SKRA_ACTIVATION_CHILD") == "1" {
		ln, activated, err := Listener("127.0.0.1:0")
		if err != nil || !activated {
			os.Exit(3)
		}
		ln.Close()
		os.Exit(0)
	}
	// Hand a bound socket to a child as fd 3, the way systemd does.
	src, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	f, err := src.(*net.TCPListener).File()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	runChild(t, f)
}
