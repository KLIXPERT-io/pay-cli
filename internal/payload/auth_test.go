package payload

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

func TestMeVerifiesTheUser(t *testing.T) {
	// A WRONG API key answers HTTP 200 with {"user":null} (verified), so the
	// status is useless and `user != null` is the only signal.
	s := newStub(t, jsonHandler(200, `{"user":null,"message":"Account"}`))
	c, _ := newTestClient(t, s, nil)
	id, err := c.Me(context.Background(), "")
	if err != nil {
		t.Fatalf("Me: %v", err)
	}
	if id.Verified {
		t.Fatal("user:null must not count as verified")
	}
	if err := AssertIdentity(id); !apierr.HasCode(err, apierr.CodeAuthInvalid) {
		t.Fatalf("err = %v, want auth_invalid", err)
	}
	if s.last().Path != "/api/users/me" {
		t.Fatalf("path = %s", s.last().Path)
	}
}

func TestMeParsesTheIdentity(t *testing.T) {
	s := newStub(t, jsonHandler(200,
		`{"user":{"id":1,"email":"a@b.test","apiKey":"paycli-dev-key","canAccessAdmin":true,"_strategy":"api-key"},"exp":1789000000}`))
	c, _ := newTestClient(t, s, nil)
	id, err := c.Me(context.Background(), "users")
	if err != nil {
		t.Fatalf("Me: %v", err)
	}
	if !id.Verified || idToString(id.UserID) != "1" {
		t.Fatalf("identity = %+v", id)
	}
	if !id.CanAccessAdmin || id.Strategy != "api-key" {
		t.Fatalf("identity = %+v", id)
	}
	if id.Exp != 1789000000 {
		t.Fatalf("exp = %d", id.Exp)
	}
	if err := AssertIdentity(id); err != nil {
		t.Fatalf("AssertIdentity: %v", err)
	}
}

func TestMeNeedsAnAuthCollection(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{}`))
	c, _ := newTestClient(t, s, func(cfg *Config) {
		cfg.AuthMode = AuthModeAnonymous
		cfg.AuthCollection = ""
		cfg.Credential = ""
	})
	if _, err := c.Me(context.Background(), ""); !apierr.HasCode(err, apierr.CodeAuthCollectionUnknown) {
		t.Fatalf("err = %v, want auth_collection_unknown", err)
	}
	if s.count() != 0 {
		t.Fatal("no request should have been sent")
	}
}

func TestAccessEnforcesH1(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		contentType string
		body        string
		wantErr     bool
	}{
		{"good", 200, "application/json", `{"canAccessAdmin":true,"collections":{"pages":{}},"globals":{}}`, false},
		{"html 200 from a wrong base url", 200, "text/html", `<!DOCTYPE html>`, true},
		{"json without collections", 200, "application/json", `{"hello":"world"}`, true},
		{"404", 404, "application/json", `{"message":"Route not found \"/api/access\""}`, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				w.Header().Set("Content-Type", tc.contentType)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			c, _ := newTestClient(t, s, nil)
			res, err := c.Access(context.Background())
			if tc.wantErr {
				if err == nil {
					t.Fatal("want an error")
				}
				if tc.status == 200 && !apierr.HasCode(err, apierr.CodeEndpointNotPayload) {
					t.Fatalf("err = %v, want endpoint_not_payload", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Access: %v", err)
			}
			if !res.CanAccessAdmin || len(res.Slugs()) != 1 || res.Slugs()[0] != "pages" {
				t.Fatalf("result = %+v", res)
			}
		})
	}
}

func TestAccessCan(t *testing.T) {
	s := newStub(t, jsonHandler(200,
		`{"collections":{"pages":{"create":{"permission":true},"delete":{"permission":false},"fields":true}},"globals":{}}`))
	c, _ := newTestClient(t, s, nil)
	res, err := c.Access(context.Background())
	if err != nil {
		t.Fatalf("Access: %v", err)
	}
	if ok, known := res.Can("pages", "create"); !ok || !known {
		t.Fatalf("create = %v, %v", ok, known)
	}
	if ok, known := res.Can("pages", "delete"); ok || !known {
		t.Fatalf("delete = %v, %v", ok, known)
	}
	if _, known := res.Can("payload-kv", "read"); known {
		t.Fatal("an absent collection must report unknown, not false")
	}
}

func TestAnonymousAccessSendsNoCredential(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{"collections":{"pages":{}},"globals":{}}`))
	c, _ := newTestClient(t, s, nil)
	if _, err := c.AnonymousAccess(context.Background()); err != nil {
		t.Fatalf("AnonymousAccess: %v", err)
	}
	if got := s.last().Header.Get(HeaderAuthorization); got != "" {
		t.Fatalf("Authorization = %q, want none", got)
	}
}

func TestInitIsTheAuthCollectionFilter(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		wantAuth bool
	}{
		{"auth collection", 200, `{"initialized":true}`, true},
		{"not an auth collection", 500, `{"errors":[{"message":"Something went wrong."}]}`, false},
		{"access gated", 403, `{"errors":[{"message":"You are not allowed to perform this action."}]}`, false},
		{"200 without the boolean", 200, `{"message":"hi"}`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub(t, jsonHandler(tc.status, tc.body))
			c, _ := newTestClient(t, s, nil)
			res, err := c.Init(context.Background(), "users")
			if err != nil {
				t.Fatalf("Init: %v", err)
			}
			if res.IsAuthCollection != tc.wantAuth {
				t.Fatalf("IsAuthCollection = %v, want %v", res.IsAuthCollection, tc.wantAuth)
			}
			if got := s.last().Header.Get(HeaderAuthorization); got != "" {
				t.Fatalf("/init must be probed without a credential, sent %q", got)
			}
		})
	}
}

func TestProbeIdentityUsesTheCandidateSlugInTheHeader(t *testing.T) {
	// Verified: a wrong auth-collection slug answers 200 with the anonymous
	// view, so the candidate must be probed by the header it would really use.
	s := newStub(t, jsonHandler(200, `{"user":{"id":7}}`))
	c, _ := newTestClient(t, s, nil)
	id, err := c.ProbeIdentity(context.Background(), "admins")
	if err != nil {
		t.Fatalf("ProbeIdentity: %v", err)
	}
	if !id.Verified {
		t.Fatal("want a verified identity")
	}
	if got := s.last().Header.Get(HeaderAuthorization); got != "admins API-Key test-key-0123456789" {
		t.Fatalf("Authorization = %q", got)
	}
	if s.last().Path != "/api/admins/me" {
		t.Fatalf("path = %s", s.last().Path)
	}
	if c.AuthCollection() != "users" {
		t.Fatal("the probe mutated the client's configuration")
	}
}

func TestDocAccessIsAReadShapedPost(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{"fields":true,"create":{"permission":true}}`))
	c, _ := newTestClient(t, s, nil)
	doc, err := c.DocAccess(context.Background(), "pages", "1")
	if err != nil {
		t.Fatalf("DocAccess: %v", err)
	}
	if doc["fields"] != true {
		t.Fatalf("doc = %v", doc)
	}
	if s.last().Method != http.MethodPost || s.last().Path != "/api/pages/access/1" {
		t.Fatalf("%s %s", s.last().Method, s.last().Path)
	}
}
