package cli

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/payloadtest"
)

// prevalidateSite serves draft pages the way Payload 3 answers a find with a
// select: selecting a SUB-path of a group (`select[hero][type]`) narrows the
// group to the selected sub-paths, dropping `hero.links` altogether; selecting
// the group itself returns it whole.
func prevalidateSite(t *testing.T, docs map[string]map[string]any) (*payloadtest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var queries []string
	find := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.RawQuery)
		mu.Unlock()
		narrowed := false
		for k := range r.URL.Query() {
			if strings.HasPrefix(k, "select[hero][") {
				narrowed = true
			}
		}
		out := []any{}
		for _, id := range []string{"1", "2"} {
			d, ok := docs[id]
			if !ok {
				continue
			}
			cp := map[string]any{}
			for k, v := range d {
				cp[k] = v
			}
			if hero, ok := d["hero"].(map[string]any); ok && narrowed {
				cp["hero"] = map[string]any{"type": hero["type"]}
			}
			out = append(out, cp)
		}
		writeJSON(w, 200, map[string]any{"docs": out, "totalDocs": len(out), "limit": 100, "page": 1,
			"totalPages": 1, "hasNextPage": false})
	}
	count := func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"totalDocs": len(docs)})
	}
	opts := []payloadtest.Option{
		payloadtest.WithHandler("GET", "/api/pages/count", count),
		payloadtest.WithHandler("GET", "/api/pages", find),
	}
	for id, d := range docs {
		doc := d
		opts = append(opts, payloadtest.WithHandler("PATCH", "/api/pages/"+id, func(w http.ResponseWriter, _ *http.Request) {
			cp := map[string]any{"_status": "published"}
			for k, v := range doc {
				cp[k] = v
			}
			writeJSON(w, 200, map[string]any{"doc": cp, "message": "Updated successfully."})
		}))
	}
	return payloadtest.NewServer(t, opts...), &queries
}

func draftPage(id int, title string, hero map[string]any) map[string]any {
	d := map[string]any{"id": id, "slug": fmt.Sprintf("p%d", id), "_status": "draft", "hero": hero}
	if title != "" {
		d["title"] = title
	}
	return d
}

// §9.10.4 for several ids: an empty `hero.links` makes `hero.links.link.label`
// vacuous. The batched pre-check used to select `hero` together with its own
// sub-paths, which made Payload drop `links`, and every page was blocked for a
// field that is not the problem.
func TestPublishSeveralIDsDoesNotBlockAnEmptyRequiredArray(t *testing.T) {
	hero := map[string]any{"type": "lowImpact", "links": []any{}}
	srv, queries := prevalidateSite(t, map[string]map[string]any{
		"1": draftPage(1, "One", hero),
		"2": draftPage(2, "Two", hero),
	})
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)
	r := cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL), Args: []string{"publish", "pages", "1", "2"}})
	if r.Code != 0 {
		t.Fatalf("exit %d\n%s", r.Code, r.Stdout)
	}
	if n := srv.Count("PATCH", "/api/pages/"); n != 2 {
		t.Errorf("PATCH count = %d, want 2", n)
	}
	for _, q := range *queries {
		if strings.Contains(q, "select%5Bhero%5D%5B") || strings.Contains(q, "select[hero][") {
			t.Errorf("the pre-check selected a sub-path of hero: %s", q)
		}
	}
}

// When the pre-check blocks EVERY id nothing is sent: that is a validation
// failure (exit 5) with the per-id reasons, not a partial failure whose hint
// says successful writes are already committed.
func TestPublishSeveralIDsAllBlockedIsAValidationFailure(t *testing.T) {
	hero := map[string]any{"type": "lowImpact", "links": []any{map[string]any{"link": map[string]any{"label": ""}}}}
	srv, _ := prevalidateSite(t, map[string]map[string]any{
		"1": draftPage(1, "", hero),
		"2": draftPage(2, "Two", hero),
	})
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)
	r := cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL), Args: []string{"publish", "pages", "1", "2"}})
	if r.Code != apierr.ExitValidation || r.code(t) != string(apierr.CodeValidationFailed) {
		t.Fatalf("exit %d code %s, want 5 validation_failed\n%s", r.Code, r.code(t), r.Stdout)
	}
	if n := srv.Count("PATCH", "/api/pages/"); n != 0 {
		t.Errorf("%d PATCH(es) sent", n)
	}
	e := r.Env["error"].(map[string]any)
	if strings.Contains(e["hint"].(string), "ALREADY COMMITTED") {
		t.Errorf("hint = %v", e["hint"])
	}
	failures, _ := e["failures"].([]any)
	if len(failures) != 2 {
		t.Fatalf("failures = %v", failures)
	}
	msgs := []string{}
	for _, f := range failures {
		msgs = append(msgs, f.(map[string]any)["message"].(string))
	}
	joined := strings.Join(msgs, "\n")
	if !strings.Contains(joined, "title") || !strings.Contains(joined, "hero.links[0].link.label") {
		t.Errorf("failure messages = %v", msgs)
	}

	// The --where form answers the same way.
	r = cliRun(t, invocation{Home: home, Env: seededEnv(srv.URL),
		Args: []string{"publish", "pages", "--where", "id in 1,2", "--yes"}})
	if r.Code != apierr.ExitValidation || srv.Count("PATCH", "/api/pages") != 0 {
		t.Fatalf("--where: exit %d\n%s", r.Code, r.Stdout)
	}
}

func TestRequiredSelectKeepsOnlyTopLevelFields(t *testing.T) {
	got := requiredSelect([]string{"hero", "hero.links.link.label", "hero.type", "slug", "title", "meta.image"})
	if strings.Join(got, ",") != "id,hero,slug,title,meta" {
		t.Errorf("requiredSelect = %v", got)
	}
}
