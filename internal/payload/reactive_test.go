package payload

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

type fakeInvalidator struct {
	calls   []StaleSignal
	outcome Outcome
	err     error
}

func (f *fakeInvalidator) Invalidate(_ context.Context, sig StaleSignal) (Outcome, error) {
	f.calls = append(f.calls, sig)
	return f.outcome, f.err
}

func TestDetectStale(t *testing.T) {
	tests := []struct {
		name       string
		resp       Response
		knownRoute bool
		want       string
	}{
		{
			"query error", Response{Status: 400, Body: []byte(`{"errors":[{"name":"QueryError","data":[{"path":"newField"}]}]}`)},
			false, StaleQueryPath,
		},
		{
			"route not found for a known route",
			Response{Status: 404, Body: []byte(`{"message":"Route not found \"/api/pages\""}`)},
			true, StaleRouteMissing,
		},
		{
			"route not found for an unknown route is just a typo",
			Response{Status: 404, Body: []byte(`{"message":"Route not found \"/api/nope\""}`)},
			false, "",
		},
		{
			"endpoints disabled",
			Response{Status: 501, Body: []byte(`{"message":"Cannot GET http://h/api/payload-migrations"}`)},
			false, StaleEndpoints,
		},
		{
			"document not found is not a schema signal",
			Response{Status: 404, Body: []byte(`{"errors":[{"message":"Not Found"}]}`)},
			true, "",
		},
		{"validation error is not a schema signal",
			Response{Status: 400, Body: []byte(`{"errors":[{"name":"ValidationError","data":{"errors":[]}}]}`)},
			false, "",
		},
		{"a 200 is never a signal", Response{Status: 200, Body: []byte(`{}`)}, true, ""},
		{"garbage body never panics", Response{Status: 400, Body: []byte("\x00not json")}, true, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.resp
			got := DetectStale(&r, tc.knownRoute)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("signal = %+v, want none", got)
				}
				return
			}
			if got == nil || got.Kind != tc.want {
				t.Fatalf("signal = %+v, want %s", got, tc.want)
			}
		})
	}
}

func TestReactiveIsOptIn(t *testing.T) {
	s := newStub(t, jsonHandler(400, `{"errors":[{"name":"QueryError","data":[{"path":"newField"}]}]}`))
	inv := &fakeInvalidator{outcome: Outcome{Revalidated: true}}
	c, _ := newTestClient(t, s, func(cfg *Config) { cfg.Invalidator = inv })

	// Without WithReactiveInvalidation nothing fires — this is what keeps
	// discovery, which deliberately generates every trigger, exempt by
	// construction.
	_, err := c.Do(context.Background(), &Request{Path: "/pages", Query: "where=x"})
	if !apierr.HasCode(err, apierr.CodeQueryPathInvalid) {
		t.Fatalf("err = %v, want query_path_invalid", err)
	}
	if len(inv.calls) != 0 {
		t.Fatalf("the invalidator fired without the opt-in: %+v", inv.calls)
	}
	if !ReactiveRequested(WithReactiveInvalidation(context.Background())) {
		t.Fatal("WithReactiveInvalidation did not mark the context")
	}
}

func TestReactiveRetriesAReadOnce(t *testing.T) {
	s := newStub(t, func(w http.ResponseWriter, _ *http.Request, n int) {
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"errors":[{"name":"QueryError","data":[{"path":"newField"}]}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"docs":[],"totalDocs":0}`)
	})
	inv := &fakeInvalidator{outcome: Outcome{Revalidated: true, Fields: []string{"newField"}}}
	c, _ := newTestClient(t, s, func(cfg *Config) { cfg.Invalidator = inv })

	ctx := WithReactiveInvalidation(context.Background())
	resp, err := c.Do(ctx, &Request{Path: "/pages", Query: "where=x"})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !resp.OK() {
		t.Fatalf("status = %d", resp.Status)
	}
	if len(inv.calls) != 1 || inv.calls[0].Kind != StaleQueryPath {
		t.Fatalf("invalidator calls = %+v", inv.calls)
	}
	if s.count() != 2 {
		t.Fatalf("made %d requests, want the original plus one replay", s.count())
	}
}

func TestReactiveNeverReplaysAWrite(t *testing.T) {
	s := newStub(t, jsonHandler(400, `{"errors":[{"name":"QueryError","data":[{"path":"newField"}]}]}`))
	inv := &fakeInvalidator{outcome: Outcome{Revalidated: true, Fields: []string{"newFields"}}}
	c, _ := newTestClient(t, s, func(cfg *Config) { cfg.Invalidator = inv })

	ctx := WithReactiveInvalidation(context.Background())
	_, err := c.Do(ctx, &Request{Method: http.MethodPatch, Path: "/pages", Query: "where=x",
		Body: []byte(`{}`), ContentType: "application/json"})
	if !apierr.HasCode(err, apierr.CodeSchemaStale) {
		t.Fatalf("err = %v, want schema_stale", err)
	}
	if apierr.ExitCode(err) != 10 {
		t.Fatalf("exit = %d, want 10", apierr.ExitCode(err))
	}
	if s.count() != 1 {
		t.Fatalf("the write was re-sent %d times", s.count()-1)
	}
	if len(inv.calls) != 1 {
		t.Fatalf("invalidator calls = %+v", inv.calls)
	}
}

func TestReactiveFiresAtMostOncePerProcess(t *testing.T) {
	s := newStub(t, jsonHandler(400, `{"errors":[{"name":"QueryError","data":[{"path":"newField"}]}]}`))
	inv := &fakeInvalidator{outcome: Outcome{Revalidated: true}}
	c, _ := newTestClient(t, s, func(cfg *Config) { cfg.Invalidator = inv })
	ctx := WithReactiveInvalidation(context.Background())

	_, _ = c.Do(ctx, &Request{Path: "/pages", Query: "where=x"})
	_, err := c.Do(ctx, &Request{Path: "/pages", Query: "where=x"})
	if !apierr.HasCode(err, apierr.CodeSchemaStale) {
		t.Fatalf("the second occurrence must be schema_stale, got %v", err)
	}
	if len(inv.calls) != 1 {
		t.Fatalf("re-discovered %d times, want exactly one", len(inv.calls))
	}
}

func TestReactiveInvalidatorFailureIsReported(t *testing.T) {
	s := newStub(t, jsonHandler(400, `{"errors":[{"name":"QueryError","data":[{"path":"x"}]}]}`))
	inv := &fakeInvalidator{err: errors.New("cache is locked")}
	c, _ := newTestClient(t, s, func(cfg *Config) { cfg.Invalidator = inv })
	_, err := c.Do(WithReactiveInvalidation(context.Background()), &Request{Path: "/pages"})
	if !apierr.HasCode(err, apierr.CodeDiscoveryFailed) {
		t.Fatalf("err = %v, want discovery_failed", err)
	}
}

func TestReactiveWithoutRevalidationFallsBackToTheOriginalError(t *testing.T) {
	s := newStub(t, jsonHandler(400, `{"errors":[{"name":"QueryError","data":[{"path":"x"}]}]}`))
	inv := &fakeInvalidator{outcome: Outcome{Revalidated: false}}
	c, _ := newTestClient(t, s, func(cfg *Config) { cfg.Invalidator = inv })
	_, err := c.Do(WithReactiveInvalidation(context.Background()), &Request{Path: "/pages"})
	if !apierr.HasCode(err, apierr.CodeQueryPathInvalid) {
		t.Fatalf("err = %v, want the original query_path_invalid", err)
	}
}
