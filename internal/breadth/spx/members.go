package spx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/osauer/ibkr/internal/xdgcache"
)

// MembersFilename is the canonical filename for the runtime-refreshed
// members cache. Lives alongside the rest of ibkr's per-feature cache
// subdirs under $XDG_CACHE_HOME/ibkr/spx-members/.
const MembersFilename = "sp500-members.json"

// MembersDefaultPath resolves the daemon's canonical members-cache
// path: $XDG_CACHE_HOME/ibkr/spx-members/sp500-members.json (XDG
// fallback to $HOME/.cache/...). Centralised so both the daemon
// (writer) and any read-side surface (status renderer, the future
// SPA) land on the same path without duplicating the resolution
// logic.
func MembersDefaultPath() (string, error) {
	dir, err := xdgcache.CacheDir("spx-members")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, MembersFilename), nil
}

// MemberList returns the S&P-500 membership the engine uses for
// today's compute. Default: the checked-in list at members_data.go
// (`make refresh-spx-members` regenerates it on every release). When
// LoadExternal succeeds the cached file at
// `~/.cache/ibkr/spx-members/sp500-members.json` takes precedence —
// the daemon's runtime refresher (refresh.go) writes that file as
// reconstitutions land, so a non-maintainer user trading off a
// six-month-old release still sees current membership.
//
// On any external-load failure (missing file, corrupt JSON, sanity
// bounds violated, version mismatch) the embedded list is returned —
// breadth never goes silent because a refresh hasn't landed yet.
//
// asOf is the date the returned list was generated. For the embedded
// path that's the release-time refresh; for the external path it's
// the timestamp the daemon's runtime refresher stamped.
func MemberList() (members []string, asOf time.Time) {
	out := slices.Clone(sp500Members)
	return out, sp500AsOf
}

// LoadExternal reads the cached members file at path and returns
// (members, asOf, true) when it passes every gate, or (nil, zero,
// false) otherwise. Gates (any failure → cached file is treated as
// absent):
//
//   - File missing or unreadable.
//   - Corrupt JSON.
//   - Version mismatch (future schema bump triggers cold rebuild).
//   - Sanity bounds: MinMembers ≤ count ≤ MaxMembers. A 600-name list
//     or a 200-name list means the parser tripped and we'd rather
//     keep computing against the embedded baseline than publish
//     nonsense.
//
// The function returns no error — every gate-fail collapses to "use
// embedded". The daemon's refresh path logs WHY a file was rejected
// before falling through; this helper is intentionally silent so it
// stays usable from non-daemon contexts (CLI, tests).
func LoadExternal(path string) (members []string, asOf time.Time, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, time.Time{}, false
	}
	var env membersFile
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, time.Time{}, false
	}
	if env.Version != currentMembersFileVersion {
		return nil, time.Time{}, false
	}
	if n := len(env.Members); n < MinMembers || n > MaxMembers {
		return nil, time.Time{}, false
	}
	// Defensive sort+clone: the file is supposed to be sorted (the
	// parser sorts before writing) but we don't want a hand-edited
	// file to surface as un-ordered downstream.
	out := slices.Clone(env.Members)
	sort.Strings(out)
	return out, env.AsOf, true
}

// SaveExternal writes members + asOf atomically to path. Used by the
// daemon's runtime refresher after a successful fetch+parse. The file
// is pretty-printed JSON so a human debugging the cache can `cat` it
// and read the membership directly.
func SaveExternal(path string, members []string, asOf time.Time) error {
	env := membersFile{
		Version: currentMembersFileVersion,
		AsOf:    asOf,
		Source:  "wikipedia",
		URL:     WikipediaURL,
		Count:   len(members),
		Members: members,
	}
	data, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal members file: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		if tmp != nil {
			_ = tmp.Close()
		}
		_ = os.Remove(tmpPath)
	}()
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	tmp = nil
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename %s: %w", path, err)
	}
	return nil
}

// MembersFileExists reports whether path exists. Used by status
// rendering to decide between the "cache:DATE" and "embedded:DATE"
// source token without re-loading the file.
func MembersFileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil || !errors.Is(err, fs.ErrNotExist)
}

// membersFile is the on-disk envelope. Mirrors the gamma-zero store's
// shape (version + descriptive fields + payload) but kept distinct
// because there's no benefit to sharing the type (the consumers are
// disjoint and the gate semantics differ).
type membersFile struct {
	Version int       `json:"version"`
	AsOf    time.Time `json:"as_of"`
	Source  string    `json:"source"`
	URL     string    `json:"url"`
	Count   int       `json:"count"`
	Members []string  `json:"members"`
}

// currentMembersFileVersion is bumped on any incompatible shape
// change. v1 is the initial layout; a future v2 (e.g. adding
// per-symbol inclusion dates for the pending-50d implementation)
// would bump this and let LoadExternal cold-rebuild from the embedded
// list while the next refresh writes the new shape.
const currentMembersFileVersion = 1
