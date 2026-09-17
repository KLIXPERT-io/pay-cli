package update

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func at(h int) time.Time { return time.Date(2026, 9, 16, h, 0, 0, 0, time.UTC) }

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", StateFileName)

	// A missing file is the zero state, not an error.
	got, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState() = %v", err)
	}
	if got.PendingVersion != "" || got.Version != StateVersion {
		t.Fatalf("zero state = %+v", got)
	}

	want := State{
		LastCheck:      at(12),
		CurrentVersion: "v0.1.0",
		PendingVersion: "v1.2.3",
		Verification:   OutcomeVerified,
	}
	if err := SaveState(path, want); err != nil {
		t.Fatalf("SaveState() = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != StatePerm {
		t.Fatalf("mode = %v, want %v", info.Mode().Perm(), StatePerm)
	}

	got, err = LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.PendingVersion != "v1.2.3" || got.Verification != OutcomeVerified || !got.LastCheck.Equal(at(12)) {
		t.Fatalf("round trip = %+v", got)
	}
}

func TestLoadStateCorruptIsZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), StateFileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState() = %v, want nil: update bookkeeping must never fail a command", err)
	}
	if got.PendingVersion != "" {
		t.Fatalf("state = %+v, want zero", got)
	}
}

func TestCheckDue(t *testing.T) {
	tests := []struct {
		name string
		last time.Time
		now  time.Time
		want bool
	}{
		{"never checked", time.Time{}, at(12), true},
		{"just checked", at(12), at(13), false},
		{"exactly 24h", at(12), at(12).Add(CheckInterval), true},
		{"over 24h", at(12), at(12).Add(48 * time.Hour), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := (State{LastCheck: tc.last}).CheckDue(tc.now); got != tc.want {
				t.Fatalf("CheckDue() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHasPendingAndShouldAutoApply(t *testing.T) {
	tests := []struct {
		name        string
		state       State
		current     string
		auto        bool
		disabled    bool
		wantPending bool
		wantApply   bool
	}{
		{
			name:    "newer and verified",
			state:   State{PendingVersion: "v1.2.3", Verification: OutcomeVerified},
			current: "v1.0.0", auto: true,
			wantPending: true, wantApply: true,
		},
		{
			name:    "unverifiable still applies",
			state:   State{PendingVersion: "v1.2.3", Verification: OutcomeUnverifiable},
			current: "v1.0.0", auto: true,
			wantPending: true, wantApply: true,
		},
		{
			name:    "a failed verification is never applied",
			state:   State{PendingVersion: "v1.2.3", Verification: OutcomeFailed},
			current: "v1.0.0", auto: true,
		},
		{
			name:    "not newer",
			state:   State{PendingVersion: "v1.0.0"},
			current: "v1.0.0", auto: true,
		},
		{
			name:    "nothing pending",
			state:   State{},
			current: "v1.0.0", auto: true,
		},
		{
			name:    "auto is off by default",
			state:   State{PendingVersion: "v1.2.3"},
			current: "v1.0.0", auto: false,
			wantPending: true,
		},
		{
			name:    "PAY_NO_UPDATE disables it",
			state:   State{PendingVersion: "v1.2.3"},
			current: "v1.0.0", auto: true, disabled: true,
			wantPending: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.state.HasPending(tc.current); got != tc.wantPending {
				t.Errorf("HasPending() = %v, want %v", got, tc.wantPending)
			}
			if got := ShouldAutoApply(tc.state, tc.auto, tc.current, tc.disabled); got != tc.wantApply {
				t.Errorf("ShouldAutoApply() = %v, want %v", got, tc.wantApply)
			}
		})
	}
}
