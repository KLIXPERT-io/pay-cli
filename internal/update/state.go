// Package update implements §15: `pay update-self`, the three-state
// verification outcome, managed-install detection and the detached apply.
//
// Nothing here reads the environment or the clock directly: the release API
// base URL, the HTTP client, the clock and every path are fields on Updater, so
// the whole package is testable against an httptest.Server and a t.TempDir().
package update

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/fsatomic"
)

const (
	// StateFileName lives in the state directory (§4.1).
	StateFileName = "update-state.json"
	// StatePerm is the state file's mode.
	StatePerm fs.FileMode = 0o600
	// CheckInterval is §15's 24 h throttle between background checks.
	CheckInterval = 24 * time.Hour
	// StateVersion is the state file's schema version.
	StateVersion = 1
)

// State is update-state.json.
type State struct {
	Version int `json:"version"`
	// LastCheck is when the release API was last consulted; it drives the 24 h
	// throttle.
	LastCheck time.Time `json:"last_check,omitempty"`
	// CurrentVersion is the binary that performed the check.
	CurrentVersion string `json:"current_version,omitempty"`
	// PendingVersion is a newer release that has been seen but not applied.
	// The implicit apply path consumes it (§15.1).
	PendingVersion string `json:"pending_version,omitempty"`
	// Verification is the last known outcome for PendingVersion.
	Verification Outcome `json:"verification,omitempty"`
	// Notified records that the user has already been told about
	// PendingVersion, so the notice is printed once per release.
	Notified bool `json:"notified,omitempty"`
	// LastApply is when an apply last completed.
	LastApply time.Time `json:"last_apply,omitempty"`
	// LastError is the last apply or check failure, for `pay doctor`.
	LastError string `json:"last_error,omitempty"`
}

// LoadState reads the state file. A missing file is the zero state. A corrupt
// one is ALSO the zero state and not an error: update bookkeeping must never be
// the reason a command fails, and the next write repairs it.
func LoadState(path string) (State, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return State{Version: StateVersion}, nil
		}
		return State{Version: StateVersion}, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		// A corrupt state file must never break `pay`: start from a fresh one.
		return State{Version: StateVersion}, nil //nolint:nilerr // corrupt state degrades to default
	}
	if s.Version == 0 {
		s.Version = StateVersion
	}
	return s, nil
}

// SaveState writes the state file atomically with mode 0600.
func SaveState(path string, s State) error {
	if s.Version == 0 {
		s.Version = StateVersion
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return fsatomic.Write(path, append(b, '\n'), StatePerm)
}

// CheckDue reports whether the 24 h throttle has expired.
func (s State) CheckDue(now time.Time) bool {
	return s.LastCheck.IsZero() || now.Sub(s.LastCheck) >= CheckInterval
}

// HasPending reports whether a newer version than current is recorded and is
// not known to have failed verification. A failed verification is never
// applied, implicitly or otherwise (§15.3).
func (s State) HasPending(current string) bool {
	if s.PendingVersion == "" || s.Verification == OutcomeFailed {
		return false
	}
	return Newer(current, s.PendingVersion)
}

// ShouldAutoApply is the §15.1 implicit-apply predicate, evaluated at the top
// of main(). When it is true the caller spawns the DETACHED child and returns
// immediately; the swapped binary takes effect on the next invocation.
func ShouldAutoApply(s State, autoEnabled bool, current string, disabled bool) bool {
	return autoEnabled && !disabled && s.HasPending(current)
}
