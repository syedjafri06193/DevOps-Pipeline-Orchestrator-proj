package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/syedjafri06193/orch/internal/auth"
)

// Tokens live in their own file beside the journal.
//
// Not in the journal, because the journal is replayed into memory on every
// start and is the deployment history: putting credentials in a file whose
// whole purpose is to be read back and rendered is how a secret ends up on a
// dashboard. Only hashes are written either way, but the separation means a
// backup of the deployment history is not a backup of the credentials.

type tokenFile struct {
	Tokens []*auth.Token `json:"tokens"`
}

func loadTokens(s *auth.Store, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // no tokens yet is not an error
		}
		return err
	}
	var f tokenFile
	if err := json.Unmarshal(b, &f); err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}
	for _, t := range f.Tokens {
		s.Add(t)
	}
	return nil
}

func saveTokens(s *auth.Store, path string) error {
	b, err := json.MarshalIndent(tokenFile{Tokens: s.List()}, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}

	// Written to a temporary file and renamed, so a crash mid-write leaves the
	// old token file rather than a truncated one. A truncated token file locks
	// every CI job out at once.
	tmp := path + ".tmp"
	// 0600: the file holds hashes rather than secrets, but the set of issued
	// credentials and who holds them is not something to leave world-readable.
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
