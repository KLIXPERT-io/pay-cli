package payload

import (
	"context"
	"net/http"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
)

func TestVersionsListPaths(t *testing.T) {
	tests := []struct {
		name   string
		target VersionTarget
		want   string
	}{
		{"collection", CollectionTarget("pages"), "/api/pages/versions"},
		{"global", GlobalTarget("header"), "/api/globals/header/versions"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub(t, jsonHandler(200, `{"docs":[{"id":20,"parent":16}],"totalDocs":1,"limit":10,"page":1}`))
			c, _ := newTestClient(t, s, nil)
			res, hasVersions, err := c.VersionsList(context.Background(), tc.target, query.Params{Limit: query.IntPtr(1)})
			if err != nil {
				t.Fatalf("VersionsList: %v", err)
			}
			if !hasVersions {
				t.Fatal("docs[] was present, so the collection has versions")
			}
			if len(res.Docs) != 1 {
				t.Fatalf("docs = %v", res.Docs)
			}
			if s.last().Path != tc.want {
				t.Fatalf("path = %s, want %s", s.last().Path, tc.want)
			}
		})
	}
}

func TestVersionsListDocsCheckIsMandatory(t *testing.T) {
	// payload-preferences answers 200 {"message":"Not Found","value":null}
	// because a custom GET /:key route shadows the versions route. A 200 is
	// therefore not proof of versions support; the docs array is.
	s := newStub(t, jsonHandler(200, `{"message":"Not Found","value":null}`))
	c, _ := newTestClient(t, s, nil)
	_, hasVersions, err := c.VersionsList(context.Background(), CollectionTarget("payload-preferences"), query.Params{})
	if err != nil {
		t.Fatalf("VersionsList: %v", err)
	}
	if hasVersions {
		t.Fatal("a 200 without docs[] must not count as versions support")
	}
}

func TestVersionGetAndRestore(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{"id":20,"parent":16}`))
	c, _ := newTestClient(t, s, nil)
	if _, _, err := c.VersionGet(context.Background(), CollectionTarget("pages"), "20", query.Params{}); err != nil {
		t.Fatalf("VersionGet: %v", err)
	}
	if s.last().Path != "/api/pages/versions/20" || s.last().Method != http.MethodGet {
		t.Fatalf("%s %s", s.last().Method, s.last().Path)
	}
	if _, err := c.VersionRestore(context.Background(), GlobalTarget("header"), "20", query.Params{}); err != nil {
		t.Fatalf("VersionRestore: %v", err)
	}
	if s.last().Path != "/api/globals/header/versions/20" || s.last().Method != http.MethodPost {
		t.Fatalf("%s %s", s.last().Method, s.last().Path)
	}
}

func TestVersionTargetValidation(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{}`))
	c, _ := newTestClient(t, s, nil)
	for _, target := range []VersionTarget{{}, {Collection: "pages", Global: "header"}} {
		if _, _, err := c.VersionsList(context.Background(), target, query.Params{}); !apierr.HasCode(err, apierr.CodeInvalidArgs) {
			t.Fatalf("target %+v: err = %v, want invalid_args", target, err)
		}
	}
	if s.count() != 0 {
		t.Fatal("an invalid target must not reach the network")
	}
	if !GlobalTarget("header").IsGlobal() || CollectionTarget("pages").IsGlobal() {
		t.Fatal("IsGlobal is wrong")
	}
	if GlobalTarget("header").Slug() != "header" || CollectionTarget("pages").Slug() != "pages" {
		t.Fatal("Slug is wrong")
	}
}
