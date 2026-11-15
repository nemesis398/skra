// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// tokenBytes is how many random bytes a token holds.
const tokenBytes = 32

// NewToken returns a fresh token: 32 random bytes as unpadded base64url.
func NewToken() string {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		panic("skra: no randomness: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// LoadToken returns the token kept in dir/token, and makes it when there is
// none. The token outlives the process, so a page that was open while the
// service exited when idle can still act once the socket starts it again.
//
// The file is made with O_EXCL and mode 0600. A file that is there but is not
// a regular file, is readable by the group or by others, or does not hold a
// token of 32 bytes or more is refused, so the service does not start.
// Deleting the file makes a new token at the next start.
func LoadToken(dir string) (string, error) {
	path := filepath.Join(dir, "token")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("token: create %s: %w", dir, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return readToken(path)
	}
	if err != nil {
		return "", fmt.Errorf("token: create %s: %w", path, err)
	}
	tok := NewToken()
	_, werr := f.WriteString(tok)
	if err := errors.Join(werr, f.Sync(), f.Close()); err != nil {
		os.Remove(path) // a half-written file would refuse the next start
		return "", fmt.Errorf("token: write %s: %w", path, err)
	}
	return tok, nil
}

// readToken reads and checks a token file that exists.
func readToken(path string) (string, error) {
	bad := func(what string) (string, error) {
		return "", fmt.Errorf("token: %s %s; delete it and skrá makes a new token at the next start", path, what)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("token: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return bad("is not a regular file")
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return bad(fmt.Sprintf("is readable by others (mode %04o, want 0600)", perm))
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("token: %w", err)
	}
	tok := string(raw)
	if tok == "" {
		return bad("is empty")
	}
	b, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return bad("is not a token")
	}
	if len(b) < tokenBytes {
		return bad(fmt.Sprintf("holds a token that is too short (%d bytes, want %d)", len(b), tokenBytes))
	}
	return tok, nil
}
