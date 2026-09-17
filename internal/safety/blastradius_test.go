package safety

import (
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

func TestCheckLimitFlag(t *testing.T) {
	tests := []struct {
		name     string
		command  string
		selector Selector
		limitSet bool
		wantErr  bool
	}{
		{"limit on a read is fine", CmdFind, SelectorNone, true, false},
		{"limit on a bulk update is an error", CmdUpdate, SelectorBulk, true, true},
		{"limit on a bulk delete is an error", CmdDelete, SelectorBulk, true, true},
		{"limit on a bulk publish is an error", CmdPublish, SelectorBulk, true, true},
		{"limit on a bulk unpublish is an error", CmdUnpublish, SelectorBulk, true, true},
		{"unset limit on a bulk write is fine", CmdDelete, SelectorBulk, false, false},
		{"limit on a scoped update is fine", CmdUpdate, SelectorID, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckLimitFlag(tc.command, tc.selector, tc.limitSet)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("CheckLimitFlag() = %v, want nil", err)
				}
				return
			}
			if apierr.CodeOf(err) != apierr.CodeInvalidArgs {
				t.Fatalf("code = %s, want invalid_args", apierr.CodeOf(err))
			}
			if apierr.ExitCode(err) != apierr.ExitValidation {
				t.Fatalf("exit = %d, want 5", apierr.ExitCode(err))
			}
			e, _ := apierr.As(err)
			const want = "--limit is page size and has no meaning on a bulk write; use --max-docs N to cap the blast radius, or --all"
			if e.Hint != want {
				t.Fatalf("hint = %q, want %q", e.Hint, want)
			}
		})
	}
}

func TestResolveMaxDocs(t *testing.T) {
	tests := []struct {
		name      string
		flag      int
		flagSet   bool
		configVal int
		all       bool
		want      int
	}{
		{"built-in default", 0, false, 0, false, 100},
		{"config default", 0, false, 250, false, 250},
		{"flag wins over config", 2, true, 250, false, 2},
		{"--all lifts the cap", 2, true, 250, true, 0},
		{"--all with nothing else", 0, false, 0, true, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveMaxDocs(tc.flag, tc.flagSet, tc.configVal, tc.all); got != tc.want {
				t.Fatalf("ResolveMaxDocs() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestPlanCheck(t *testing.T) {
	tests := []struct {
		name     string
		plan     Plan
		wantErr  bool
		wantHint string
	}{
		{
			name: "under the cap",
			plan: Plan{Command: CmdDelete, Collection: "pages", Matched: 50, MaxDocs: 100},
		},
		{
			name: "exactly at the cap",
			plan: Plan{Command: CmdDelete, Collection: "pages", Matched: 100, MaxDocs: 100},
		},
		{
			name:     "over the cap",
			plan:     Plan{Command: CmdDelete, Collection: "crm-contacts", Matched: 1200, MaxDocs: 100},
			wantErr:  true,
			wantHint: "N=1200 exceeds --max-docs 100; pass --all to delete everything matching, or narrow --where",
		},
		{
			name:     "over the cap on update",
			plan:     Plan{Command: CmdUpdate, Collection: "pages", Matched: 11, MaxDocs: 2},
			wantErr:  true,
			wantHint: "N=11 exceeds --max-docs 2; pass --all to update everything matching, or narrow --where",
		},
		{
			name: "--all lifts the cap",
			plan: Plan{Command: CmdDelete, Collection: "pages", Matched: 1200, MaxDocs: 0, All: true},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.plan.Check()
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("Check() = %v, want nil", err)
				}
				return
			}
			if apierr.CodeOf(err) != apierr.CodeBulkLimitExceeded {
				t.Fatalf("code = %s, want bulk_limit_exceeded", apierr.CodeOf(err))
			}
			if apierr.ExitCode(err) != apierr.ExitValidation {
				t.Fatalf("exit = %d, want 5", apierr.ExitCode(err))
			}
			e, _ := apierr.As(err)
			if e.Hint != tc.wantHint {
				t.Fatalf("hint = %q, want %q", e.Hint, tc.wantHint)
			}
			if !strings.Contains(e.Message, "exceeds --max-docs") {
				t.Fatalf("message = %q", e.Message)
			}
		})
	}
}

func TestChunk(t *testing.T) {
	ids := make([]any, 0, 250)
	for i := 1; i <= 250; i++ {
		ids = append(ids, i)
	}
	tests := []struct {
		name      string
		ids       []any
		size      int
		wantSizes []int
	}{
		{"empty", nil, 100, nil},
		{"one short batch", ids[:7], 100, []int{7}},
		{"exact multiple", ids[:200], 100, []int{100, 100}},
		{"trailing partial", ids, 100, []int{100, 100, 50}},
		{"non-positive size falls back to ChunkSize", ids[:150], 0, []int{100, 50}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Chunk(tc.ids, tc.size)
			if len(got) != len(tc.wantSizes) {
				t.Fatalf("got %d batches, want %d", len(got), len(tc.wantSizes))
			}
			for i, want := range tc.wantSizes {
				if len(got[i]) != want {
					t.Fatalf("batch %d has %d ids, want %d", i, len(got[i]), want)
				}
			}
		})
	}
}

func TestChunkCopiesInput(t *testing.T) {
	ids := []any{1, 2, 3}
	batches := Chunk(ids, 2)
	batches[0][0] = 99
	if ids[0] != 1 {
		t.Fatalf("Chunk aliased its input: ids[0] = %v", ids[0])
	}
}

func TestWhereIDsIn(t *testing.T) {
	w := WhereIDsIn([]any{1, 2})
	id, ok := w["id"].(map[string]any)
	if !ok {
		t.Fatalf("where = %#v", w)
	}
	in, ok := id["in"].([]any)
	if !ok || len(in) != 2 || in[0] != 1 || in[1] != 2 {
		t.Fatalf("in = %#v", id["in"])
	}
}

func TestIsBulkWriteVerb(t *testing.T) {
	for _, v := range []string{CmdUpdate, CmdDelete, CmdPublish, CmdUnpublish} {
		if !IsBulkWriteVerb(v) {
			t.Errorf("IsBulkWriteVerb(%q) = false", v)
		}
	}
	for _, v := range []string{CmdFind, CmdCreate, CmdUpload, CmdGet, ""} {
		if IsBulkWriteVerb(v) {
			t.Errorf("IsBulkWriteVerb(%q) = true", v)
		}
	}
}

// TestValidateMaxDocsRejectsNonPositive: an explicit blast-radius cap is never
// discarded in silence. 0 and negatives cannot be honoured (Plan.Check reads
// MaxDocs <= 0 as "no cap"), so they are an error rather than a fallback to
// defaults.max_bulk and an error message quoting a cap the user never typed.
func TestValidateMaxDocsRejectsNonPositive(t *testing.T) {
	tests := []struct {
		name    string
		flag    int
		flagSet bool
		wantErr bool
	}{
		{"--max-docs 0", 0, true, true},
		{"--max-docs -1", -1, true, true},
		{"--max-docs 1", 1, true, false},
		{"flag absent", 0, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateMaxDocs(tc.flag, tc.flagSet)
			if tc.wantErr != (err != nil) {
				t.Fatalf("ValidateMaxDocs(%d, %v) = %v", tc.flag, tc.flagSet, err)
			}
			if err == nil {
				return
			}
			if apierr.CodeOf(err) != apierr.CodeInvalidArgs {
				t.Fatalf("code = %s, want invalid_args", apierr.CodeOf(err))
			}
			e, _ := apierr.As(err)
			if !strings.Contains(e.Message, "positive") {
				t.Fatalf("message = %q", e.Message)
			}
			// An agent that typed -1 meaning "unlimited" must be told which
			// flag actually means that.
			if !strings.Contains(e.Hint, "--all") {
				t.Fatalf("hint = %q", e.Hint)
			}
		})
	}
}

// TestMaxBulkZeroIsRejectedNotReinterpreted is finding 11's config half.
//
// ResolveMaxDocs treats a configured cap of 0 as "unset" and substitutes
// DefaultMaxDocs, so `max_bulk = 0` in a profile silently became 100 — the
// opposite of what was written, on the one setting whose whole job is to be a
// limit. internal/config now refuses the value outright; this pins the
// behaviour ResolveMaxDocs relies on, so the two cannot drift apart.
func TestMaxBulkZeroIsRejectedNotReinterpreted(t *testing.T) {
	if got := ResolveMaxDocs(0, false, 0, false); got != DefaultMaxDocs {
		t.Fatalf("ResolveMaxDocs with configMaxBulk=0 = %d, want the default %d — "+
			"config must reject 0 before it reaches here", got, DefaultMaxDocs)
	}
	// The flag half, for contrast: an explicit non-positive flag is an error,
	// never a reinterpretation.
	for _, v := range []int{0, -1, -5} {
		if err := ValidateMaxDocs(v, true); err == nil {
			t.Errorf("--max-docs %d was accepted", v)
		}
	}
	if err := ValidateMaxDocs(0, false); err != nil {
		t.Errorf("an UNSET --max-docs must not be an error: %v", err)
	}
}
