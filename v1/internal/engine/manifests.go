package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// DirManifests reads release manifests from a directory laid out as
// `<root>/<app>/<version>.json`.
//
// A directory rather than a database table, because the manifest describes the
// artifact and belongs with it: it is written by the build that produced the
// version, and the orchestrator only reads it. Section 8.2's migration floor
// is only trustworthy if the thing that knows about the migrations is what
// wrote it down.
type DirManifests struct {
	Root string
}

// ErrNoManifest is returned when an app+version has no manifest on disk.
//
// A distinct error rather than a generic one, because section 8.3 turns on
// telling "this version is safe to roll back to" apart from "I do not know
// whether this version is safe to roll back to". The second is not permission
// to proceed, and callers have to be able to tell which they got.
var ErrNoManifest = errors.New("engine: no release manifest")

func (d *DirManifests) Manifest(ctx context.Context, app, version string) (*Manifest, error) {
	if d.Root == "" {
		return nil, fmt.Errorf("%w: no manifest directory is configured", ErrNoManifest)
	}
	// The version is attacker-influenced in the sense that it comes from a
	// request body, so it must not be able to climb out of the directory.
	if strings.ContainsAny(app, `/\`) || strings.ContainsAny(version, `/\`) ||
		strings.Contains(app, "..") || strings.Contains(version, "..") {
		return nil, fmt.Errorf("engine: refusing to read a manifest for %q/%q: the name contains a path separator", app, version)
	}

	path := filepath.Join(d.Root, app, version+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w for %s %s at %s", ErrNoManifest, app, version, path)
		}
		return nil, err
	}

	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("engine: parsing %s: %w", path, err)
	}
	if m.Version == "" {
		m.Version = version
	}
	if m.Version != version {
		// A manifest that names a different version is either a bad copy or a
		// mislabelled artifact, and either way the migration floor in it does
		// not describe the thing about to be deployed.
		return nil, fmt.Errorf("engine: %s declares version %q but is filed under %q",
			path, m.Version, version)
	}
	return &m, nil
}
