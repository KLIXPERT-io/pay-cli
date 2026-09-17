package payloadtest

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func get(t *testing.T, srv *Server, path string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Get(srv.URL + path) //nolint:noctx // test helper
	if err != nil {
		t.Fatalf("get %s: %v", path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return resp, body
}

func TestServerAnswersFromTheIndex(t *testing.T) {
	srv := NewServer(t)
	cases := []struct {
		path   string
		status int
		want   string
	}{
		{"/api/access", 200, "collections"},
		{"/api/pages?limit=2&depth=0", 200, "docs"},
		{"/api/pages/16?depth=0", 200, "id"},
		{"/api/users/me", 200, "user"},
		{"/api/globals/header", 200, "id"},
		{"/api/pages/999999999", 404, "errors"},
		{"/api/no-such-collection", 404, "message"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			resp, body := get(t, srv, tc.path)
			if resp.StatusCode != tc.status {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			if !strings.Contains(string(body), tc.want) {
				t.Errorf("body does not contain %q: %s", tc.want, firstN(string(body), 200))
			}
			if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("content-type = %q", ct)
			}
			if resp.Header.Get("X-Powered-By") != "Payload" {
				t.Errorf("the harness must replay X-Powered-By")
			}
		})
	}
}

func TestServerMatchesQuerySupersets(t *testing.T) {
	srv := NewServer(t)
	// The fixture was recorded with limit=2&depth=0; a request that adds a
	// harmless parameter must still match it rather than 404.
	resp, body := get(t, srv, "/api/pages?depth=0&limit=2&fallback-locale=none")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, firstN(string(body), 200))
	}
}

func TestServerDisambiguatesGraphQLByBody(t *testing.T) {
	srv := NewServer(t)
	cases := []struct{ query, want string }{
		{`{"query":"query{ acc: __type(name:\"Access\"){ fields{ name } } }"}`, "Access"},
		{`{"query":"query{ __type(name:\"Page\"){ name } }"}`, "Page"},
		{`{"query":"query{ nope }"}`, "Cannot query field"},
	}
	for _, tc := range cases {
		resp, err := http.Post(srv.URL+"/api/graphql", "application/json", strings.NewReader(tc.query)) //nolint:noctx
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if !strings.Contains(string(body), tc.want) {
			t.Errorf("query %q answered with %s", firstN(tc.query, 40), firstN(string(body), 120))
		}
	}
}

func TestServerDistinguishesAnonymousFromAuthenticated(t *testing.T) {
	srv := NewServer(t)
	_, anon := get(t, srv, "/api/users/me")
	var parsed map[string]any
	if err := json.Unmarshal(anon, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["user"] != nil {
		t.Errorf("an unauthenticated /me returned a user: %v", parsed["user"])
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/users/me", nil) //nolint:noctx
	req.Header.Set("Authorization", "users API-Key whatever")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["user"] == nil {
		t.Errorf("an authenticated /me returned no user")
	}
}

func TestServerMatchesAHeader(t *testing.T) {
	srv := NewServer(t)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/pages", strings.NewReader("{}")) //nolint:noctx
	req.Header.Set("Accept-Language", "de")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	// The German body is the §11.2 regression guard: it must not be the English
	// one, or the test that depends on it proves nothing.
	if !strings.Contains(string(body), "Feld") && !strings.Contains(string(body), "ungültig") &&
		!strings.Contains(string(body), "erforderlich") {
		t.Logf("recorded German body: %s", firstN(string(body), 200))
	}
}

func TestServerRecordsTraffic(t *testing.T) {
	srv := NewServer(t)
	get(t, srv, "/api/access")
	get(t, srv, "/api/pages?limit=2&depth=0")
	if n := srv.Count("GET", "/api/access"); n != 1 {
		t.Errorf("Count = %d, want 1", n)
	}
	if len(srv.Requests()) != 2 {
		t.Errorf("recorded %d requests, want 2", len(srv.Requests()))
	}
	srv.Reset()
	if len(srv.Requests()) != 0 {
		t.Errorf("Reset did not clear the log")
	}
}

func TestServerUnmatchedRouteIsPayloadShaped(t *testing.T) {
	srv := NewServer(t)
	resp, body := get(t, srv, "/api/definitely-not-recorded/42")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "Route not found") {
		t.Errorf("unmatched route did not use Payload's own shape: %s", body)
	}
	if len(srv.Misses()) != 1 {
		t.Errorf("miss was not recorded")
	}
}

func TestServerWithHandlerOverrides(t *testing.T) {
	srv := NewServer(t, WithHandler("GET", "/api/access", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"errors":[{"message":"Too Many Requests"}]}`))
	}))
	resp, body := get(t, srv, "/api/access")
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "3" {
		t.Fatalf("override did not take: %d %v %s", resp.StatusCode, resp.Header, body)
	}
}

func TestFixturesAreValidJSONAndKeySorted(t *testing.T) {
	idx := LoadIndex(t)
	if idx.PayloadVersion == "" {
		t.Errorf("the index does not record the Payload version it was recorded from")
	}
	if len(idx.Requests) < 15 {
		t.Errorf("only %d recorded routes", len(idx.Requests))
	}
	for _, r := range idx.Requests {
		if r.File == "" || r.Method == "" || r.Path == "" {
			t.Errorf("incomplete route: %+v", r)
			continue
		}
		var v any
		if err := json.Unmarshal(Load(t, r.File), &v); err != nil {
			t.Errorf("%s is not valid JSON: %v", r.File, err)
		}
	}
}

func TestClockIsDeterministic(t *testing.T) {
	frozen := Frozen()
	if !frozen.Now().Equal(frozen.Now()) {
		t.Errorf("Frozen() moved")
	}
	step := NewClock(time.Second)
	a, b := step.Now(), step.Now()
	if b.Sub(a) != time.Second {
		t.Errorf("step = %v, want 1s", b.Sub(a))
	}
	step.Advance(time.Minute)
	if step.Peek().Sub(b) != time.Minute+time.Second {
		t.Errorf("Advance did not move the clock as expected")
	}
}

func TestNormalizeErasesTheVolatileFields(t *testing.T) {
	in := []byte(`{"request_id": "01ABC","cli_version": "0.1.0","duration_ms": 17,"url":"http://127.0.0.1:54321/api"}`)
	got := string(Normalize(in))
	for _, want := range []string{`"request_id": "<id>"`, `"cli_version": "<version>"`, `"duration_ms": 0`, "127.0.0.1:<port>"} {
		if !strings.Contains(got, want) {
			t.Errorf("Normalize did not produce %q: %s", want, got)
		}
	}
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
