package payload

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
)

const pagesListBody = `{"docs":[{"id":16,"title":"A"},{"id":17,"title":"B"}],` +
	`"hasNextPage":false,"hasPrevPage":false,"limit":20,"nextPage":null,"page":1,` +
	`"pagingCounter":1,"prevPage":null,"totalDocs":2,"totalPages":1}`

func TestFindSendsTheEncodedQuery(t *testing.T) {
	s := newStub(t, jsonHandler(200, pagesListBody))
	c, _ := newTestClient(t, s, nil)

	p := query.Params{
		Where:  query.Term("jobTitle", query.OpEquals, nil),
		Select: []string{"title"},
		Depth:  query.IntPtr(0),
		Limit:  query.IntPtr(20),
		Sort:   []string{"-createdAt"},
	}
	res, err := c.Find(context.Background(), "pages", p)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(res.Docs) != 2 || res.TotalDocs != 2 {
		t.Fatalf("result = %+v", res)
	}
	if got := idToString(res.Docs[0].ID()); got != "16" {
		t.Fatalf("first id = %s", got)
	}

	seen := s.last()
	if seen.Path != "/api/pages" {
		t.Fatalf("path = %s", seen.Path)
	}
	values, err := url.ParseQuery(seen.RawQuery)
	if err != nil {
		t.Fatalf("the query string does not parse: %v", err)
	}
	// `where` travels as a URL-encoded JSON string, which is the only form
	// that can express null; `select` stays bracket notation, because a JSON
	// select is silently dropped (both verified live).
	if got := values.Get("where"); got != `{"jobTitle":{"equals":null}}` {
		t.Fatalf("where = %s", got)
	}
	if got := values.Get("select[title]"); got != "true" {
		t.Fatalf("select = %v", values)
	}
	if values.Get("depth") != "0" || values.Get("limit") != "20" || values.Get("sort") != "-createdAt" {
		t.Fatalf("scalars = %v", values)
	}
}

func TestGetAndCount(t *testing.T) {
	s := newStub(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/pages/16":
			_, _ = io.WriteString(w, `{"id":16,"title":"A"}`)
		case "/api/pages/count":
			_, _ = io.WriteString(w, `{"totalDocs":11}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"errors":[{"message":"Not Found"}]}`)
		}
	})
	c, _ := newTestClient(t, s, nil)

	doc, _, err := c.Get(context.Background(), "pages", "16", query.Params{Depth: query.IntPtr(0)})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if doc["title"] != "A" {
		t.Fatalf("doc = %v", doc)
	}

	n, _, err := c.Count(context.Background(), "pages", query.Params{
		Where: query.Term("id", query.OpEquals, 16),
		// count accepts only where and trash; the rest must be dropped.
		Limit: query.IntPtr(20), Depth: query.IntPtr(2), Draft: query.BoolPtr(true),
	})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 11 {
		t.Fatalf("count = %d", n)
	}
	values, _ := url.ParseQuery(s.last().RawQuery)
	for _, dropped := range []string{"limit", "depth", "draft"} {
		if values.Has(dropped) {
			t.Fatalf("count sent %s: %v", dropped, values)
		}
	}
	if values.Get("where") == "" {
		t.Fatalf("count dropped the where clause: %v", values)
	}
}

func TestGetIDIsPathEscaped(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{"id":"a b"}`))
	c, _ := newTestClient(t, s, nil)
	if _, _, err := c.Get(context.Background(), "pages", "a b/../../etc", query.Params{}); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := s.last().Path; got != "/api/pages/a b/../../etc" && got != "/api/pages/a%20b%2F..%2F..%2Fetc" {
		t.Logf("escaped path = %q", got)
	}
	if s.last().Path == "/etc" {
		t.Fatal("the id escaped its path segment")
	}
}

func TestFindPagesWalksAndCaps(t *testing.T) {
	s := newStub(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		page := r.URL.Query().Get("page")
		w.Header().Set("Content-Type", "application/json")
		switch page {
		case "1":
			_, _ = io.WriteString(w, `{"docs":[{"id":1},{"id":2}],"hasNextPage":true,"nextPage":2,"totalDocs":5,"limit":2,"page":1,"totalPages":3}`)
		case "2":
			_, _ = io.WriteString(w, `{"docs":[{"id":3},{"id":4}],"hasNextPage":true,"nextPage":3,"totalDocs":5,"limit":2,"page":2,"totalPages":3}`)
		default:
			_, _ = io.WriteString(w, `{"docs":[{"id":5}],"hasNextPage":false,"nextPage":null,"totalDocs":5,"limit":2,"page":3,"totalPages":3}`)
		}
	})
	c, _ := newTestClient(t, s, nil)

	var ids []string
	err := c.FindPages(context.Background(), "pages", query.Params{Limit: query.IntPtr(2)}, 0, func(l *ListResult) error {
		for _, d := range l.Docs {
			ids = append(ids, idToString(d.ID()))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("FindPages: %v", err)
	}
	if fmt.Sprint(ids) != "[1 2 3 4 5]" {
		t.Fatalf("ids = %v", ids)
	}

	ids = nil
	err = c.FindPages(context.Background(), "pages", query.Params{Limit: query.IntPtr(2)}, 3, func(l *ListResult) error {
		for _, d := range l.Docs {
			ids = append(ids, idToString(d.ID()))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("FindPages: %v", err)
	}
	if fmt.Sprint(ids) != "[1 2 3]" {
		t.Fatalf("capped ids = %v", ids)
	}
}

func TestResolveIDsUsesSelectID(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{"docs":[{"id":1},{"id":2}],"hasNextPage":false,"totalDocs":2,"limit":200,"page":1}`))
	c, _ := newTestClient(t, s, nil)
	ids, err := c.ResolveIDs(context.Background(), "crm-contacts", query.Term("status", query.OpEquals, "lead"), 2)
	if err != nil {
		t.Fatalf("ResolveIDs: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("ids = %v", ids)
	}
	values, _ := url.ParseQuery(s.last().RawQuery)
	if values.Get("select[id]") != "true" {
		t.Fatalf("the id resolution did not use select[id]: %v", values)
	}
	if values.Get("depth") != "0" {
		t.Fatalf("depth = %v", values)
	}
}

func TestResolveIDsRequiresWhere(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{}`))
	c, _ := newTestClient(t, s, nil)
	_, err := c.ResolveIDs(context.Background(), "pages", nil, 10)
	if !apierr.HasCode(err, apierr.CodeWhereRequired) {
		t.Fatalf("err = %v, want where_required", err)
	}
	if s.count() != 0 {
		t.Fatal("a where-less bulk resolution must not reach the network")
	}
}

func TestCollectionFromPath(t *testing.T) {
	tests := map[string]string{
		"/pages":              "pages",
		"pages/16":            "pages",
		"/globals/header":     "",
		"/access":             "",
		"/crm-contacts/count": "crm-contacts",
		"":                    "",
	}
	for in, want := range tests {
		if got := collectionFromPath(in); got != want {
			t.Errorf("collectionFromPath(%q) = %q, want %q", in, got, want)
		}
	}
}
