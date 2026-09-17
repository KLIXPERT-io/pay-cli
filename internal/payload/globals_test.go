package payload

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
)

func TestGlobalGet(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{"id":1,"navItems":[],"globalType":"header"}`))
	c, _ := newTestClient(t, s, nil)
	doc, _, err := c.GlobalGet(context.Background(), "header", query.Params{Depth: query.IntPtr(0), Draft: query.BoolPtr(true)})
	if err != nil {
		t.Fatalf("GlobalGet: %v", err)
	}
	if doc["globalType"] != "header" {
		t.Fatalf("doc = %v", doc)
	}
	if s.last().Path != "/api/globals/header" {
		t.Fatalf("path = %s", s.last().Path)
	}
	values, _ := url.ParseQuery(s.last().RawQuery)
	if values.Get("depth") != "0" || values.Get("draft") != "true" {
		t.Fatalf("query = %v", values)
	}
}

func TestGlobalUpdateIsPost(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{"doc":{"id":1},"message":"Updated successfully."}`))
	c, _ := newTestClient(t, s, nil)
	res, err := c.GlobalUpdate(context.Background(), "header", map[string]any{"navItems": []any{}}, query.Params{})
	if err != nil {
		t.Fatalf("GlobalUpdate: %v", err)
	}
	if res.Message == "" {
		t.Fatalf("result = %+v", res)
	}
	seen := s.last()
	// Payload's update operation for a global is a POST, not a PATCH.
	if seen.Method != http.MethodPost || seen.Path != "/api/globals/header" {
		t.Fatalf("%s %s", seen.Method, seen.Path)
	}
	if seen.Body != `{"navItems":[]}` {
		t.Fatalf("body = %s", seen.Body)
	}
}

func TestGlobalPathEscapes(t *testing.T) {
	if got := GlobalPath("site settings"); got != "/globals/site%20settings" {
		t.Fatalf("GlobalPath = %q", got)
	}
}
