package safety

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/output"
)

func TestNewDryRun(t *testing.T) {
	tests := []struct {
		name          string
		req           DryRunRequest
		total         int
		ids           []any
		noRedact      bool
		wantAffect    int
		wantSample    int
		wantTruncated bool
		wantURL       string
		wantBody      string
	}{
		{
			name:          "bulk delete samples five ids",
			req:           DryRunRequest{Method: "DELETE", URL: "http://localhost:3900/api/crm-contacts?where=%7B%7D"},
			total:         12,
			ids:           []any{224, 223, 222, 221, 220, 219, 218},
			wantAffect:    12,
			wantSample:    5,
			wantTruncated: true,
			wantURL:       "http://localhost:3900/api/crm-contacts?where=%7B%7D",
			wantBody:      "null",
		},
		{
			name:       "single doc is not truncated",
			req:        DryRunRequest{Method: "PATCH", URL: "http://localhost:3900/api/pages/16"},
			total:      1,
			ids:        []any{16},
			wantAffect: 1,
			wantSample: 1,
		},
		{
			name:       "negative total falls back to the id count",
			req:        DryRunRequest{Method: "PATCH", URL: "http://x/api/pages"},
			total:      -1,
			ids:        []any{1, 2},
			wantAffect: 2,
			wantSample: 2,
		},
		{
			name: "userinfo is stripped from the url",
			req: DryRunRequest{
				Method: "DELETE",
				URL:    "https://user:hunter2@example.com/api/pages?where=%7B%7D",
			},
			total:      0,
			wantAffect: 0,
			wantURL:    "https://example.com/api/pages?where=%7B%7D",
		},
		{
			name: "a body carrying a credential is redacted",
			req: DryRunRequest{
				Method: "POST",
				URL:    "http://localhost:3900/api/users",
				Body:   json.RawMessage(`{"email":"a@b.c","password":"hunter2"}`),
			},
			total:      1,
			ids:        []any{1},
			wantAffect: 1,
			wantSample: 1,
			wantBody:   `{"email":"a@b.c","password":"<redacted>"}`,
		},
		{
			name: "--no-redact keeps the body byte-faithful",
			req: DryRunRequest{
				Method: "POST",
				URL:    "http://localhost:3900/api/users",
				Body:   json.RawMessage(`{"password":"hunter2"}`),
			},
			total:      1,
			ids:        []any{1},
			noRedact:   true,
			wantAffect: 1,
			wantSample: 1,
			wantBody:   `{"password":"hunter2"}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NewDryRun(tc.req, tc.total, tc.ids, tc.noRedact)
			if got.WouldAffect != tc.wantAffect {
				t.Errorf("would_affect = %d, want %d", got.WouldAffect, tc.wantAffect)
			}
			if len(got.SampleIDs) != tc.wantSample {
				t.Errorf("sample_ids = %v, want %d entries", got.SampleIDs, tc.wantSample)
			}
			if got.Truncated != tc.wantTruncated {
				t.Errorf("truncated = %v, want %v", got.Truncated, tc.wantTruncated)
			}
			if tc.wantURL != "" && got.Request.URL != tc.wantURL {
				t.Errorf("url = %q, want %q", got.Request.URL, tc.wantURL)
			}
			if tc.wantBody != "" {
				body, err := got.Request.Body.MarshalJSON()
				if err != nil {
					t.Fatalf("MarshalJSON: %v", err)
				}
				if string(body) != tc.wantBody {
					t.Errorf("body = %s, want %s", body, tc.wantBody)
				}
			}
		})
	}
}

func TestDryRunEnvelope(t *testing.T) {
	res := NewDryRun(DryRunRequest{Method: "DELETE", URL: "http://localhost:3900/api/pages"}, 2, []any{1, 2}, false)
	env := res.Envelope("delete", output.Meta{DryRun: false})
	if !env.OK {
		t.Fatal("dry run envelope must be ok")
	}
	if env.ExitCode() != 0 {
		t.Fatalf("exit = %d, want 0", env.ExitCode())
	}
	if env.DataKind != output.KindOpResult {
		t.Fatalf("data_kind = %q, want op_result", env.DataKind)
	}
	if !env.Meta.DryRun {
		t.Fatal("meta.dry_run must be forced true")
	}
	b, err := env.MarshalIndentTo()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"would_affect": 2`, `"sample_ids"`, `"truncated": false`, `"dry_run": true`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("envelope missing %q:\n%s", want, b)
		}
	}
}

func TestDryRunTruncationReflectsTheSample(t *testing.T) {
	req := DryRunRequest{Method: "POST", URL: "http://x/api/pages"}
	cases := []struct {
		name  string
		total int
		ids   []any
		want  bool
	}{
		{name: "create resolves no ids", total: 1, ids: nil, want: false},
		{name: "one id shown in full", total: 1, ids: []any{1}, want: false},
		{name: "five ids shown in full", total: 5, ids: []any{1, 2, 3, 4, 5}, want: false},
		{name: "six ids are truncated to five", total: 6, ids: []any{1, 2, 3, 4, 5, 6}, want: true},
		{name: "count without ids is truncated", total: 1200, ids: nil, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NewDryRun(req, tc.total, tc.ids, false)
			if got.Truncated != tc.want {
				t.Errorf("truncated = %v, want %v (sample %v)", got.Truncated, tc.want, got.SampleIDs)
			}
		})
	}
}
