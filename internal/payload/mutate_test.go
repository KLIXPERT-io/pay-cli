package payload

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
)

func TestCreateSendsJSONBody(t *testing.T) {
	s := newStub(t, jsonHandler(201, `{"doc":{"id":5,"title":"A"},"message":"Media successfully created."}`))
	c, _ := newTestClient(t, s, nil)
	res, err := c.Create(context.Background(), "pages", map[string]any{"title": "A", "heroImage": 4}, query.Params{Depth: query.IntPtr(0)})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if idToString(res.Doc.ID()) != "5" {
		t.Fatalf("doc = %v", res.Doc)
	}
	seen := s.last()
	if seen.Method != http.MethodPost || seen.Path != "/api/pages" {
		t.Fatalf("%s %s", seen.Method, seen.Path)
	}
	if got := seen.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(seen.Body), &body); err != nil {
		t.Fatalf("body = %s", seen.Body)
	}
	// A relationship id must stay a JSON number: {"heroImage":"4"} is a 400
	// "invalid relationships", {"heroImage":4} is accepted (verified).
	if body["heroImage"] != float64(4) {
		t.Fatalf("heroImage = %#v, want the number 4", body["heroImage"])
	}
}

func TestUpdateIsPatch(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{"doc":{"id":1},"message":"Updated successfully."}`))
	c, _ := newTestClient(t, s, nil)
	if _, err := c.Update(context.Background(), "posts", "1", map[string]any{"title": "B"}, query.Params{}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if s.last().Method != http.MethodPatch || s.last().Path != "/api/posts/1" {
		t.Fatalf("%s %s", s.last().Method, s.last().Path)
	}
}

func TestTrashAndRestore(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{"doc":{"id":1},"message":"Updated successfully."}`))
	c, _ := newTestClient(t, s, nil)

	if _, err := c.Trash(context.Background(), "crm-contacts", "1", "2026-09-16T12:00:00Z", query.Params{}); err != nil {
		t.Fatalf("Trash: %v", err)
	}
	if !strings.Contains(s.last().Body, `"deletedAt":"2026-09-16T12:00:00Z"`) {
		t.Fatalf("soft delete body = %s", s.last().Body)
	}

	if _, err := c.Restore(context.Background(), "crm-contacts", "1", query.Params{}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	// The restore body is {"deletedAt":null} — the exact value bracket
	// notation cannot express, which is why `data` is JSON.
	if s.last().Body != `{"deletedAt":null}` {
		t.Fatalf("restore body = %s", s.last().Body)
	}
	values, _ := url.ParseQuery(s.last().RawQuery)
	if values.Get("trash") != "true" {
		t.Fatalf("restore must send trash=true: %v", values)
	}
}

func TestTrashNeedsAnInstant(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{}`))
	c, _ := newTestClient(t, s, nil)
	if _, err := c.Trash(context.Background(), "x", "1", "", query.Params{}); !apierr.HasCode(err, apierr.CodeInvalidArgs) {
		t.Fatalf("err = %v, want invalid_args", err)
	}
	if s.count() != 0 {
		t.Fatal("no request should have been sent")
	}
}

func TestPlanBulkCountsThenResolves(t *testing.T) {
	s := newStub(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/count") {
			_, _ = io.WriteString(w, `{"totalDocs":3}`)
			return
		}
		_, _ = io.WriteString(w, `{"docs":[{"id":1},{"id":2},{"id":3}],"hasNextPage":false,"totalDocs":3,"limit":200,"page":1}`)
	})
	c, _ := newTestClient(t, s, nil)
	plan, err := c.PlanBulk(context.Background(), "crm-contacts", query.Term("status", query.OpEquals, "lead"), 100, false)
	if err != nil {
		t.Fatalf("PlanBulk: %v", err)
	}
	if plan.Count != 3 || len(plan.IDs) != 3 {
		t.Fatalf("plan = %+v", plan)
	}
	if s.count() != 2 {
		t.Fatalf("made %d requests, want count + id resolution", s.count())
	}
}

func TestPlanBulkEnforcesTheBlastRadius(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{"totalDocs":1200}`))
	c, _ := newTestClient(t, s, nil)
	_, err := c.PlanBulk(context.Background(), "crm-contacts", query.Term("status", query.OpEquals, "lead"), 100, false)
	if !apierr.HasCode(err, apierr.CodeBulkLimitExceeded) {
		t.Fatalf("err = %v, want bulk_limit_exceeded", err)
	}
	e, _ := apierr.As(err)
	if !strings.Contains(e.Hint, "--all") || !strings.Contains(e.Hint, "1200") {
		t.Fatalf("hint = %q", e.Hint)
	}
	if s.count() != 1 {
		t.Fatalf("the ids were resolved anyway: %d requests", s.count())
	}
}

func TestPlanBulkRequiresWhere(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{}`))
	c, _ := newTestClient(t, s, nil)
	_, err := c.PlanBulk(context.Background(), "pages", nil, 100, false)
	if !apierr.HasCode(err, apierr.CodeWhereRequired) {
		t.Fatalf("err = %v, want where_required", err)
	}
	if s.count() != 0 {
		t.Fatal("no request should have been sent")
	}
}

func TestBulkWritesAreIDScopedAndNeverSendLimit(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{"docs":[{"id":1}],"errors":[],"message":"Updated 1 Crm Contact successfully."}`))
	c, _ := newTestClient(t, s, nil)

	ids := make([]any, 0, 250)
	for i := 1; i <= 250; i++ {
		ids = append(ids, i)
	}
	if _, err := c.UpdateByIDs(context.Background(), "crm-contacts", ids, map[string]any{"status": "lead"},
		query.Params{Limit: query.IntPtr(2)}); err != nil {
		t.Fatalf("UpdateByIDs: %v", err)
	}
	if s.count() != 3 {
		t.Fatalf("made %d requests, want ceil(250/%d)", s.count(), BulkChunk)
	}
	for _, req := range s.seen() {
		if req.Method != http.MethodPatch {
			t.Fatalf("method = %s", req.Method)
		}
		values, err := url.ParseQuery(req.RawQuery)
		if err != nil {
			t.Fatalf("query = %q", req.RawQuery)
		}
		// A golden guarantee of §12.3: no limit= on a bulk write, ever.
		if values.Has("limit") {
			t.Fatalf("a bulk write sent limit=: %q", req.RawQuery)
		}
		where := values.Get("where")
		if !strings.Contains(where, `"id":{"in":[`) {
			t.Fatalf("where = %s, want an id-scoped clause", where)
		}
		if req.Header.Get(HeaderMethodOverride) != "" {
			t.Fatal("a bulk write must never use the method override")
		}
	}
}

func TestBulkDeleteChunks(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{"docs":[],"errors":[],"message":"Deleted 0 Crm Contacts successfully."}`))
	c, _ := newTestClient(t, s, nil)
	ids := make([]any, 100)
	for i := range ids {
		ids[i] = i + 1
	}
	if _, err := c.DeleteByIDs(context.Background(), "crm-contacts", ids, query.Params{}); err != nil {
		t.Fatalf("DeleteByIDs: %v", err)
	}
	if s.count() != 1 {
		t.Fatalf("made %d requests, want 1", s.count())
	}
	if s.last().Method != http.MethodDelete {
		t.Fatalf("method = %s", s.last().Method)
	}
}

func TestBulkWithNoIDsIsRefused(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{}`))
	c, _ := newTestClient(t, s, nil)
	_, err := c.DeleteByIDs(context.Background(), "pages", nil, query.Params{})
	if !apierr.HasCode(err, apierr.CodeWhereRequired) {
		t.Fatalf("err = %v, want where_required", err)
	}
	if s.count() != 0 {
		t.Fatal("an empty id set must not reach the network")
	}
}

func TestBulkPartialFailureKeepsTheCommittedDocs(t *testing.T) {
	// Payload answers 400 whenever errors[] is non-empty, even though the
	// documents in docs[] were committed.
	body := `{"docs":[{"id":1}],"errors":[{"message":"nope","id":2}],"message":"Updated 1"}`
	s := newStub(t, jsonHandler(400, body))
	c, _ := newTestClient(t, s, nil)
	res, err := c.UpdateByIDs(context.Background(), "crm-contacts", []any{1, 2}, map[string]any{"status": "x"}, query.Params{})
	if !apierr.HasCode(err, apierr.CodePartialFailure) {
		t.Fatalf("err = %v, want partial_failure", err)
	}
	if apierr.ExitCode(err) != 7 {
		t.Fatalf("exit = %d, want 7", apierr.ExitCode(err))
	}
	if res == nil || len(res.Docs) != 1 || len(res.Errors) != 1 {
		t.Fatalf("result = %+v", res)
	}
	if idToString(res.IDs()[0]) != "1" {
		t.Fatalf("committed ids = %v", res.IDs())
	}
}

func TestDuplicate(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{"doc":{"id":9},"message":"ok"}`))
	c, _ := newTestClient(t, s, nil)
	if _, err := c.Duplicate(context.Background(), "crm-contacts", "174", query.Params{}); err != nil {
		t.Fatalf("Duplicate: %v", err)
	}
	if s.last().Path != "/api/crm-contacts/174/duplicate" || s.last().Method != http.MethodPost {
		t.Fatalf("%s %s", s.last().Method, s.last().Path)
	}
}

func TestDeleteWhereUnsafeSendsTheWhereStraightThrough(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{"docs":[],"errors":[],"message":"Deleted 4"}`))
	c, _ := newTestClient(t, s, nil)
	_, err := c.DeleteWhereUnsafe(context.Background(), "crm-contacts",
		query.Term("firstName", query.OpEquals, "PayCLIArch"), query.Params{Limit: query.IntPtr(1)})
	if err != nil {
		t.Fatalf("DeleteWhereUnsafe: %v", err)
	}
	values, _ := url.ParseQuery(s.last().RawQuery)
	if values.Has("limit") {
		t.Fatalf("even the unsafe path must not send limit (DELETE ignores it): %v", values)
	}
	if values.Get("where") == "" {
		t.Fatalf("where = %v", values)
	}
}

// TestResolveIDsScopedKeepsTheWriteScope is the §12.3 "all three phases address
// the same documents" rule. A where-only resolution cannot see a soft-deleted
// document, so `delete --permanent` (which sends trash=true) counted one
// population and deleted another.
func TestResolveIDsScopedKeepsTheWriteScope(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{"docs":[{"id":3},{"id":4}],"hasNextPage":false,"totalDocs":2,"limit":200,"page":1}`))
	c, _ := newTestClient(t, s, nil)

	p := query.Params{
		Where:  query.Term("deletedAt", query.OpExists, true),
		Trash:  query.BoolPtr(true),
		Draft:  query.BoolPtr(true),
		Locale: "de",
		Select: []string{"title"},                // the write's projection, not the resolution's
		Depth:  query.IntPtr(3),                  // ditto
		Extra:  url.Values{"autosave": {"true"}}, // write-only, must not reach a GET
	}
	ids, err := c.ResolveIDsScoped(context.Background(), "crm-contacts", p, 2)
	if err != nil {
		t.Fatalf("ResolveIDsScoped: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("ids = %v", ids)
	}
	values, _ := url.ParseQuery(s.last().RawQuery)
	if values.Get("trash") != "true" {
		t.Fatalf("the resolution dropped the write's trash scope: %v", values)
	}
	if values.Get("draft") != "true" || values.Get("locale") != "de" {
		t.Fatalf("the resolution dropped draft/locale: %v", values)
	}
	if values.Get("select[id]") != "true" || values.Get("select[title]") != "" {
		t.Fatalf("the resolution must project id only: %v", values)
	}
	if values.Get("depth") != "0" || values.Get("limit") != "200" {
		t.Fatalf("the resolution owns depth/limit: %v", values)
	}
	if values.Get("autosave") != "" {
		t.Fatalf("a write-only extra leaked onto the resolution GET: %v", values)
	}
}

func TestResolveIDsScopedRequiresWhere(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{}`))
	c, _ := newTestClient(t, s, nil)
	if _, err := c.ResolveIDsScoped(context.Background(), "pages", query.Params{Trash: query.BoolPtr(true)}, 10); !apierr.HasCode(err, apierr.CodeWhereRequired) {
		t.Fatalf("err = %v, want where_required", err)
	}
	if s.count() != 0 {
		t.Fatal("no request should have been sent")
	}
}

// TestBulkPinsTheFirstFailingChunk is §12.5's provenance rule: error.http and
// error.raw must describe the request that FAILED, not whichever chunk happened
// to be sent last.
func TestBulkPinsTheFirstFailingChunk(t *testing.T) {
	ids := make([]any, 0, 150)
	for i := 1; i <= 150; i++ {
		ids = append(ids, i)
	}
	s := newStub(t, func(w http.ResponseWriter, r *http.Request, attempt int) {
		w.Header().Set("Content-Type", "application/json")
		if attempt == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"docs":[{"id":1}],"errors":[{"id":42,"message":"The following field is invalid: Title"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"docs":[{"id":101}],"errors":[],"message":"Updated 50 Pages successfully."}`)
	})
	c, _ := newTestClient(t, s, nil)

	res, _ := c.UpdateByIDs(context.Background(), "pages", ids, map[string]any{"title": "x"}, query.Params{})
	if res == nil {
		t.Fatal("no result")
	}
	if res.Chunks != 2 || res.FailedChunk != 1 {
		t.Fatalf("chunks = %d, failed chunk = %d, want 2 and 1", res.Chunks, res.FailedChunk)
	}
	if res.HTTP == nil || res.HTTP.Status != 400 {
		t.Fatalf("error.http would report %+v — the failing chunk answered 400", res.HTTP)
	}
	if !strings.Contains(string(res.Raw), `"id":42`) {
		t.Fatalf("error.raw = %s, want the failing chunk's body", res.Raw)
	}
	// Both chunks are still accumulated.
	if len(res.Docs) != 2 || len(res.Errors) != 1 {
		t.Fatalf("docs = %v, errors = %v", res.Docs, res.Errors)
	}
}

// TestBulkPinsAPerIDFailureUnderA200 covers the same rule when Payload reports
// the rejection in errors[] with a 2xx status.
func TestBulkPinsAPerIDFailureUnderA200(t *testing.T) {
	ids := make([]any, 0, 150)
	for i := 1; i <= 150; i++ {
		ids = append(ids, i)
	}
	s := newStub(t, func(w http.ResponseWriter, r *http.Request, attempt int) {
		w.Header().Set("Content-Type", "application/json")
		if attempt == 1 {
			_, _ = io.WriteString(w, `{"docs":[],"errors":[{"id":7,"message":"nope"}],"message":"chunk-one"}`)
			return
		}
		_, _ = io.WriteString(w, `{"docs":[{"id":101}],"errors":[],"message":"chunk-two"}`)
	})
	c, _ := newTestClient(t, s, nil)

	res, _ := c.UpdateByIDs(context.Background(), "pages", ids, map[string]any{"title": "x"}, query.Params{})
	if res.FailedChunk != 1 {
		t.Fatalf("failed chunk = %d, want 1", res.FailedChunk)
	}
	if !strings.Contains(string(res.Raw), "chunk-one") {
		t.Fatalf("raw = %s, want chunk one's body", res.Raw)
	}
}

// TestBulkKeepsTheLastChunkWhenNothingFailed pins the unchanged behaviour: with
// no failure there is nothing to point at, so the last response is kept.
func TestBulkKeepsTheLastChunkWhenNothingFailed(t *testing.T) {
	ids := make([]any, 0, 150)
	for i := 1; i <= 150; i++ {
		ids = append(ids, i)
	}
	s := newStub(t, func(w http.ResponseWriter, r *http.Request, attempt int) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"docs":[{"id":`+string(rune('0'+attempt))+`}],"errors":[],"message":"chunk"}`)
	})
	c, _ := newTestClient(t, s, nil)
	res, err := c.UpdateByIDs(context.Background(), "pages", ids, map[string]any{"title": "x"}, query.Params{})
	if err != nil {
		t.Fatalf("UpdateByIDs: %v", err)
	}
	if res.Chunks != 2 || res.FailedChunk != 0 {
		t.Fatalf("chunks = %d, failed = %d", res.Chunks, res.FailedChunk)
	}
	if res.HTTP == nil || res.HTTP.Status != 200 {
		t.Fatalf("http = %+v", res.HTTP)
	}
}
