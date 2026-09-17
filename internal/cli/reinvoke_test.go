package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/cache"
	"github.com/KLIXPERT-io/pay-cli/internal/config"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/payloadtest"
)

// ---------------------------------------------------------------------------
// next.cmd continuity (finding 0)
//
// §10.1 and the agent skill both promise that next.cmd is a literally runnable
// command "with the current flags and profile already baked in". These tests
// take the CLI at its word: they run a query, feed the emitted string straight
// back through the same CLI, and check that the second run answers the SAME
// question. The fixture is deliberately the 69-of-104 case, because a builder
// that drops the filter returns a DIFFERENT number of documents — equal counts
// would let a weaker assertion pass.
// ---------------------------------------------------------------------------

const (
	reinvokeTotal = 104 // every contact
	reinvokeLeads = 69  // …of which this many have status "lead"
)

func reinvokeDocs() []map[string]any {
	docs := make([]map[string]any, 0, reinvokeTotal)
	for i := 1; i <= reinvokeTotal; i++ {
		// Statuses are INTERLEAVED on purpose: if the leads were the first 69
		// ids, a next.cmd that dropped the filter would still hand back 69
		// leads under --max 69 and the test would pass on a broken builder.
		status := "lead"
		switch {
		case i == reinvokeTotal:
			status = "customer"
		case i%3 == 0:
			status = "prospect"
		}
		docs = append(docs, map[string]any{
			"id":        i,
			"email":     fmt.Sprintf("c%03d@example.test", i),
			"status":    status,
			"updatedAt": "2026-09-01T00:00:00Z",
			"createdAt": "2026-09-01T00:00:00Z",
		})
	}
	return docs
}

// reinvokeMatches evaluates the subset of Payload's where grammar these tests
// emit: and/or groups over {field:{equals|contains: value}}.
func reinvokeMatches(t *testing.T, doc, where map[string]any) bool {
	t.Helper()
	group := func(raw any) []map[string]any {
		items, _ := raw.([]any)
		out := make([]map[string]any, 0, len(items))
		for _, it := range items {
			if m, ok := it.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	for key, raw := range where {
		switch key {
		case "and":
			for _, sub := range group(raw) {
				if !reinvokeMatches(t, doc, sub) {
					return false
				}
			}
		case "or":
			hit := false
			for _, sub := range group(raw) {
				if reinvokeMatches(t, doc, sub) {
					hit = true
					break
				}
			}
			if !hit {
				return false
			}
		default:
			ops, ok := raw.(map[string]any)
			if !ok {
				t.Errorf("test server: unsupported where term %q: %#v", key, raw)
				return false
			}
			have := fmt.Sprint(doc[key])
			for op, want := range ops {
				switch op {
				case "equals":
					if have != fmt.Sprint(want) {
						return false
					}
				case "contains", "like":
					if !strings.Contains(have, fmt.Sprint(want)) {
						return false
					}
				default:
					t.Errorf("test server: unsupported operator %q", op)
					return false
				}
			}
		}
	}
	return true
}

// reinvokeServer answers /api/crm-contacts and /api/crm-contacts/count over the
// fixed 104-document set, honouring where, sort, limit, page and select. It has
// to be a real filter: a stub that ignored `where` could not tell a next.cmd
// that kept the filter from one that dropped it.
func reinvokeServer(t *testing.T) *payloadtest.Server {
	t.Helper()
	all := reinvokeDocs()

	selected := func(r *http.Request, docs []map[string]any) []map[string]any {
		keep := map[string]bool{"id": true}
		n := 0
		for key := range r.URL.Query() {
			if rest, ok := strings.CutPrefix(key, "select["); ok {
				keep[strings.TrimSuffix(rest, "]")] = true
				n++
			}
		}
		if n == 0 {
			return docs
		}
		out := make([]map[string]any, 0, len(docs))
		for _, d := range docs {
			slim := map[string]any{}
			for k, v := range d {
				if keep[k] {
					slim[k] = v
				}
			}
			out = append(out, slim)
		}
		return out
	}

	matching := func(t *testing.T, r *http.Request) []map[string]any {
		var where map[string]any
		if raw := r.URL.Query().Get("where"); raw != "" {
			if err := json.Unmarshal([]byte(raw), &where); err != nil {
				t.Errorf("test server: undecodable where %q: %v", raw, err)
			}
		}
		out := make([]map[string]any, 0, len(all))
		for _, d := range all {
			if where == nil || reinvokeMatches(t, d, where) {
				out = append(out, d)
			}
		}
		if s := r.URL.Query().Get("sort"); s != "" {
			desc := strings.HasPrefix(s, "-")
			key := strings.TrimPrefix(s, "-")
			sort.SliceStable(out, func(i, j int) bool {
				a, b := fmt.Sprint(out[i][key]), fmt.Sprint(out[j][key])
				if desc {
					return a > b
				}
				return a < b
			})
		}
		return out
	}

	writeJSON := func(w http.ResponseWriter, body any) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("X-Powered-By", "Payload")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(body)
	}

	return payloadtest.NewServer(t,
		payloadtest.WithHandler("GET", "/api/crm-contacts", func(w http.ResponseWriter, r *http.Request) {
			docs := matching(t, r)
			total := len(docs)
			limit := reinvokeQueryInt(r, "limit", 10)
			page := reinvokeQueryInt(r, "page", 1)
			if limit <= 0 {
				limit = total
			}
			totalPages := 1
			if limit > 0 {
				totalPages = (total + limit - 1) / limit
			}
			if totalPages < 1 {
				totalPages = 1
			}
			start := (page - 1) * limit
			if start > total {
				start = total
			}
			end := start + limit
			if end > total {
				end = total
			}
			body := map[string]any{
				"docs":          selected(r, docs[start:end]),
				"totalDocs":     total,
				"limit":         limit,
				"totalPages":    totalPages,
				"page":          page,
				"pagingCounter": start + 1,
				"hasPrevPage":   page > 1,
				"hasNextPage":   page < totalPages,
			}
			if page > 1 {
				body["prevPage"] = page - 1
			} else {
				body["prevPage"] = nil
			}
			if page < totalPages {
				body["nextPage"] = page + 1
			} else {
				body["nextPage"] = nil
			}
			writeJSON(w, body)
		}),
		payloadtest.WithHandler("GET", "/api/crm-contacts/count", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"totalDocs": len(matching(t, r))})
		}))
}

func reinvokeQueryInt(r *http.Request, key string, def int) int {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return n
}

// reinvokeContactsShard is the schema the client-side validators see.
func reinvokeContactsShard() *discovery.Shard {
	s := discovery.NewShard("gen", "crm-contacts")
	s.Fields = []discovery.Field{
		testField("id", discovery.TypeID),
		testField("email", discovery.TypeEmail),
		testField("status", discovery.TypeSelect, withOptions("lead", "prospect", "customer")),
		testField("updatedAt", discovery.TypeDate),
		testField("createdAt", discovery.TypeDate),
	}
	s.Finalize()
	return s
}

// seedContactsDiscovery is seedDiscovery plus a crm-contacts shard, so the
// whole round trip runs offline against the cached manifest.
func seedContactsDiscovery(t *testing.T, home, baseURL string) {
	t.Helper()
	sc, err := cache.NewScope(cache.ScopeInput{
		BaseURL:        baseURL,
		APIPath:        "/api",
		GraphQLPath:    "/api/graphql",
		KeyFingerprint: cache.AnonKeyFingerprint,
	})
	if err != nil {
		t.Fatalf("scope: %v", err)
	}
	var m discovery.Manifest
	payloadtest.LoadJSON(t, "manifest", &m)
	var pages discovery.Shard
	payloadtest.LoadJSON(t, "fields_pages", &pages)
	contacts := reinvokeContactsShard()

	now := payloadtest.Epoch
	generation := cache.NewGeneration(now)
	m.Generation = generation
	m.Meta.Scope = sc.Key
	m.GeneratedAt = now
	m.ExpiresAt = now.Add(24 * time.Hour)
	pages.Generation = generation
	contacts.Generation = generation

	store := cache.New(filepath.Join(home, "cache"))
	ok, warns := store.WriteSet(sc, cache.Set{
		Generation: generation,
		Manifest:   &m,
		Shards: map[string]any{
			cache.ShardName("pages", cache.KindCollection):        &pages,
			cache.ShardName("crm-contacts", cache.KindCollection): contacts,
		},
	}, now)
	if !ok {
		t.Fatalf("seed cache write failed: %v", warns)
	}
}

// shellSplit is a POSIX-ish word splitter. next.cmd is promised to a SHELL, so
// the test has to consume it the way a shell would rather than by splitting on
// spaces — that is the whole difference between a quoted --where term and four
// broken arguments.
func shellSplit(t *testing.T, s string) []string {
	t.Helper()
	var out []string
	var cur strings.Builder
	word := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\'':
			word = true
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				t.Fatalf("unterminated single quote in %q", s)
			}
			cur.WriteString(s[i+1 : i+1+j])
			i += j + 1
		case '"':
			word = true
			j := strings.IndexByte(s[i+1:], '"')
			if j < 0 {
				t.Fatalf("unterminated double quote in %q", s)
			}
			cur.WriteString(s[i+1 : i+1+j])
			i += j + 1
		case '\\':
			if i+1 < len(s) {
				cur.WriteByte(s[i+1])
				i++
				word = true
			}
		case ' ', '\t':
			if word {
				out = append(out, cur.String())
				cur.Reset()
				word = false
			}
		default:
			cur.WriteByte(c)
			word = true
		}
	}
	if word {
		out = append(out, cur.String())
	}
	return out
}

// reinvokeArgs turns an emitted command string into the argv a shell would
// hand back to `pay`.
func reinvokeArgs(t *testing.T, cmd string) []string {
	t.Helper()
	argv := shellSplit(t, cmd)
	if len(argv) == 0 || argv[0] != "pay" {
		t.Fatalf("next.cmd is not a pay invocation: %q", cmd)
	}
	return argv[1:]
}

func jsonInt(t *testing.T, v any) int {
	t.Helper()
	n, ok := v.(json.Number)
	if !ok {
		t.Fatalf("not a JSON number: %#v", v)
	}
	i, err := n.Int64()
	if err != nil {
		t.Fatalf("not an integer: %v", err)
	}
	return int(i)
}

func envPageField(t *testing.T, res cliResult, key string) int {
	t.Helper()
	page, ok := res.Env["page"].(map[string]any)
	if !ok {
		t.Fatalf("envelope has no page block:\n%s", res.Stdout)
	}
	return jsonInt(t, page[key])
}

func envDocs(t *testing.T, res cliResult) []map[string]any {
	t.Helper()
	raw, ok := res.Env["data"].([]any)
	if !ok {
		t.Fatalf("envelope has no document list:\n%s", res.Stdout)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, d := range raw {
		m, ok := d.(map[string]any)
		if !ok {
			t.Fatalf("document is not an object: %#v", d)
		}
		out = append(out, m)
	}
	return out
}

func envIDs(t *testing.T, res cliResult) []int {
	t.Helper()
	var ids []int
	for _, d := range envDocs(t, res) {
		ids = append(ids, jsonInt(t, d["id"]))
	}
	return ids
}

// nextCmdOf digs the string out of next[] — either next.cmd or the first
// alternative, because both are re-run instructions and both were broken.
func nextCmdOf(t *testing.T, res cliResult, alternative bool) string {
	t.Helper()
	next, ok := res.Env["next"].(map[string]any)
	if !ok {
		t.Fatalf("envelope has no next block:\n%s", res.Stdout)
	}
	if !alternative {
		cmd, _ := next["cmd"].(string)
		if cmd == "" {
			t.Fatalf("next.cmd is empty:\n%s", res.Stdout)
		}
		return cmd
	}
	alts, ok := next["alternatives"].([]any)
	if !ok || len(alts) == 0 {
		t.Fatalf("next has no alternatives:\n%s", res.Stdout)
	}
	alt, _ := alts[0].(map[string]any)
	cmd, _ := alt["cmd"].(string)
	if cmd == "" {
		t.Fatalf("alternatives[0].cmd is empty:\n%s", res.Stdout)
	}
	return cmd
}

func TestNextCmdRerunsTheSameQuery(t *testing.T) {
	// Control: the collection really does hold more documents than the filter
	// matches, so "same count" is a meaningful assertion.
	t.Run("fixture really is 69 of 104", func(t *testing.T) {
		srv := reinvokeServer(t)
		home := t.TempDir()
		seedContactsDiscovery(t, home, srv.URL)
		unfiltered := cliRun(t, invocation{
			Home: home, Args: []string{"find", "crm-contacts", "--limit", "3"},
			Env: seededEnv(srv.URL),
		})
		if got := envPageField(t, unfiltered, "total_docs"); got != reinvokeTotal {
			t.Fatalf("unfiltered total_docs = %d, want %d", got, reinvokeTotal)
		}
	})

	filtered := []string{"find", "crm-contacts", "--where", "status eq lead",
		"--select", "email,status", "--limit", "3"}

	cases := []struct {
		name         string
		first        []string
		alternative  bool
		wantLimit    int
		wantReturned int  // 0 = "whatever the page holds"
		disjoint     bool // the follow-up must not repeat the first page
	}{
		{
			name:      "find page next.cmd",
			first:     filtered,
			wantLimit: 3,
			disjoint:  true,
		},
		{
			name:         "find alternatives --all --max",
			first:        filtered,
			alternative:  true,
			wantLimit:    3,
			wantReturned: reinvokeLeads,
		},
		{
			name: "find --all truncated next.cmd",
			first: []string{"find", "crm-contacts", "--where", "status eq lead",
				"--select", "email,status", "--all", "--max", "5"},
			wantLimit:    20,
			wantReturned: reinvokeLeads,
		},
		{
			name:      "count hand-off to find",
			first:     []string{"count", "crm-contacts", "--where", "status eq lead"},
			wantLimit: countHandoffLimit,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := reinvokeServer(t)
			home := t.TempDir()
			seedContactsDiscovery(t, home, srv.URL)

			first := cliRun(t, invocation{Home: home, Args: tc.first, Env: seededEnv(srv.URL)})
			if first.Code != 0 {
				t.Fatalf("first run exit = %d\n%s\n%s", first.Code, first.Stdout, first.Stderr)
			}
			if tc.first[0] == "find" {
				if got := envPageField(t, first, "total_docs"); got != reinvokeLeads {
					t.Fatalf("first run total_docs = %d, want %d", got, reinvokeLeads)
				}
			}

			cmd := nextCmdOf(t, first, tc.alternative)
			second := cliRun(t, invocation{
				Home: home, Args: reinvokeArgs(t, cmd), Env: seededEnv(srv.URL),
			})
			if second.Code != 0 {
				t.Fatalf("re-running %q exited %d\n%s\n%s", cmd, second.Code, second.Stdout, second.Stderr)
			}

			// (a) the same question…
			if got := envPageField(t, second, "total_docs"); got != reinvokeLeads {
				t.Errorf("re-running %q: total_docs = %d, want %d — the filter was lost",
					cmd, got, reinvokeLeads)
			}
			// (b) …at the same page size…
			if got := envPageField(t, second, "limit"); got != tc.wantLimit {
				t.Errorf("re-running %q: limit = %d, want %d", cmd, got, tc.wantLimit)
			}
			// (c) …returning documents that all still satisfy the filter.
			docs := envDocs(t, second)
			if tc.wantReturned > 0 && len(docs) != tc.wantReturned {
				t.Errorf("re-running %q returned %d documents, want %d", cmd, len(docs), tc.wantReturned)
			}
			for _, d := range docs {
				if status, ok := d["status"]; ok && status != "lead" {
					t.Fatalf("re-running %q returned a non-lead document: %v", cmd, d)
				}
			}
			// (d) …and, for a page hand-off, without repeating a single row.
			if tc.disjoint {
				seen := map[int]bool{}
				for _, id := range envIDs(t, first) {
					seen[id] = true
				}
				if len(seen) == 0 {
					t.Fatal("the first page returned no ids")
				}
				for _, id := range envIDs(t, second) {
					if seen[id] {
						t.Errorf("re-running %q repeated document %d from the first page", cmd, id)
					}
				}
			}
		})
	}
}

// TestNextCmdKeepsTheOutputFormat covers the streaming alternative, whose whole
// point is --output jsonl and which therefore emits no envelope to parse.
func TestNextCmdKeepsTheOutputFormat(t *testing.T) {
	srv := reinvokeServer(t)
	home := t.TempDir()
	seedContactsDiscovery(t, home, srv.URL)

	first := cliRun(t, invocation{
		Home: home,
		Args: []string{"find", "crm-contacts", "--where", "status eq lead", "--all", "--max", "5"},
		Env:  seededEnv(srv.URL),
	})
	cmd := nextCmdOf(t, first, true)
	if !strings.Contains(cmd, "--output jsonl") {
		t.Fatalf("the streaming alternative lost --output jsonl: %q", cmd)
	}
	second := cliRun(t, invocation{Home: home, Args: reinvokeArgs(t, cmd), Env: seededEnv(srv.URL)})
	if second.Code != 0 {
		t.Fatalf("re-running %q exited %d\n%s", cmd, second.Code, second.Stderr)
	}
	lines := 0
	for _, line := range strings.Split(strings.TrimSpace(second.Stdout), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(line), &doc); err != nil {
			t.Fatalf("jsonl line is not JSON: %q", line)
		}
		if doc["status"] != nil && doc["status"] != "lead" {
			t.Fatalf("the streaming alternative lost the filter: %v", doc)
		}
		lines++
	}
	if lines != reinvokeLeads {
		t.Errorf("streamed %d documents, want %d", lines, reinvokeLeads)
	}
}

// TestNextArgsCarryTheWholeQuery is §10.1's structured half: a caller that
// parses next.args rather than the string must see the filter too.
func TestNextArgsCarryTheWholeQuery(t *testing.T) {
	srv := reinvokeServer(t)
	home := t.TempDir()
	seedContactsDiscovery(t, home, srv.URL)

	res := cliRun(t, invocation{
		Home: home,
		Args: []string{"find", "crm-contacts", "--where", "status eq lead", "--limit", "3"},
		Env:  seededEnv(srv.URL),
	})
	next, _ := res.Env["next"].(map[string]any)
	args, ok := next["args"].(map[string]any)
	if !ok {
		t.Fatalf("next.args is missing:\n%s", res.Stdout)
	}
	where, ok := args["where"].([]any)
	if !ok || len(where) != 1 || where[0] != "status eq lead" {
		t.Errorf("next.args.where = %#v, want the original filter term", args["where"])
	}
	if args["collection"] != "crm-contacts" {
		t.Errorf("next.args.collection = %v", args["collection"])
	}
	if got := jsonInt(t, args["limit"]); got != 3 {
		t.Errorf("next.args.limit = %d, want 3", got)
	}
	if got := jsonInt(t, args["page"]); got != 2 {
		t.Errorf("next.args.page = %d, want 2", got)
	}
}

// TestReinvocationIsShellSafe pins the promise that the string is runnable in a
// SHELL: every element has to survive word splitting, quoting and expansion
// exactly as typed.
func TestReinvocationIsShellSafe(t *testing.T) {
	nasty := `title contains it's a "trap"; rm -rf $HOME && echo *`
	cmd, f := readCmd()
	f.limit = 20
	f.where = []string{nasty}
	f.sort = []string{"-publishedAt"}
	f.selectFields = []string{"title"}
	built, err := f.build(cmd, testDeps(), testTarget())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	got, _ := f.reinvocation(testDeps(), testTarget(), built, reinvokeOpts{Page: 2})
	want := []string{"pay", "find", "pages",
		"--where", nasty,
		"--select", "title",
		"--sort", "-publishedAt",
		"--limit", "20",
		"--page", "2"}
	if diff := shellSplit(t, got); !reflect.DeepEqual(diff, want) {
		t.Fatalf("next.cmd does not survive the shell\n cmd:  %s\n got:  %#v\n want: %#v", got, diff, want)
	}
}

// TestReinvocationCarriesTheResolvedContext pins the two facts that come from
// the run rather than from the sub-command's own flags: the profile the run
// resolved and the output format it is producing. An agent that copies
// next.cmd into a different shell has neither unless they are baked in.
func TestReinvocationCarriesTheResolvedContext(t *testing.T) {
	d := &Deps{
		Manifest: pagesManifest(),
		RT: &Runtime{Cfg: &config.Resolved{
			Profile: "acme prod", ProfileDefined: true, Output: "csv",
		}},
	}
	cmd, f := readCmd()
	f.limit = 20
	f.publishedOnly = true
	built, err := f.build(cmd, d, testTarget())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	got, args := f.reinvocation(d, testTarget(), built, reinvokeOpts{Page: 3})
	want := []string{"pay", "find", "pages",
		"--profile", "acme prod",
		"--published-only",
		"--limit", "20",
		"--page", "3",
		"--output", "csv"}
	if split := shellSplit(t, got); !reflect.DeepEqual(split, want) {
		t.Fatalf("next.cmd = %s\n got: %#v\nwant: %#v", got, split, want)
	}
	if args["profile"] != "acme prod" || args["output"] != "csv" || args["published_only"] != true {
		t.Errorf("next.args lost the resolved context: %#v", args)
	}
}

// A profile no config file defines must NOT be re-emitted: `--profile default`
// on an env-configured run is a profile_unknown error, which would replace a
// working command with a broken one.
func TestReinvocationOmitsAnUndefinedProfile(t *testing.T) {
	d := &Deps{
		Manifest: pagesManifest(),
		RT:       &Runtime{Cfg: &config.Resolved{Profile: "default"}},
	}
	cmd, f := readCmd()
	f.limit = 20
	built, err := f.build(cmd, d, testTarget())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	got, _ := f.reinvocation(d, testTarget(), built, reinvokeOpts{Page: 2})
	if strings.Contains(got, "--profile") {
		t.Fatalf("an undefined profile was re-emitted: %s", got)
	}
}

func TestShellQuote(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"pages", "pages"},
		{"-publishedAt", "-publishedAt"},
		{"2026-09-01T00:00:00Z", "2026-09-01T00:00:00Z"},
		{"", "''"},
		{"status eq lead", "'status eq lead'"},
		{"a*b", "'a*b'"},
		{"$HOME", "'$HOME'"},
		{"it's", `'it'\''s'`},
	} {
		if got := shellQuote(tc.in); got != tc.want {
			t.Errorf("shellQuote(%q) = %s, want %s", tc.in, got, tc.want)
		}
		if got := shellSplit(t, shellQuote(tc.in)); len(got) != 1 || got[0] != tc.in {
			if tc.in != "" || len(got) != 0 {
				t.Errorf("shellQuote(%q) does not round-trip: %#v", tc.in, got)
			}
		}
	}
}

// TestCountOnlyEmitsTheSameHandoffAsCount closes the asymmetry between the two
// ways to count. `pay count` emits a filter-preserving `next.cmd`; `pay find
// --count-only` emitted no next[] at all, so an agent that counted with find
// got no hand-off to the documents — and no way to discover that the other
// spelling would have given it one.
func TestCountOnlyEmitsTheSameHandoffAsCount(t *testing.T) {
	srv := payloadtest.NewServer(t,
		payloadtest.WithHandler("GET", "/api/pages/count", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("X-Powered-By", "Payload")
			_, _ = io.WriteString(w, `{"totalDocs":69}`)
		}))
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)

	nextOf := func(args ...string) map[string]any {
		t.Helper()
		res := cliRun(t, invocation{Home: home, Args: args, Env: seededEnv(srv.URL)})
		if res.Code != 0 {
			t.Fatalf("%v exit = %d: %s\n%s", args, res.Code, res.Stdout, res.Stderr)
		}
		n, _ := res.Env["next"].(map[string]any)
		return n
	}

	where := []string{"--where", "_status eq draft"}
	fromCount := nextOf(append([]string{"count", "pages"}, where...)...)
	fromFind := nextOf(append([]string{"find", "pages", "--count-only"}, where...)...)

	if fromCount == nil {
		t.Fatal("precondition: `pay count` is supposed to emit a hand-off")
	}
	if fromFind == nil {
		t.Fatal("`find --count-only` emits no next[]; the agent has no way to see the documents it just counted")
	}
	// Both hand-offs must preserve the filter, or "69 drafts" becomes a first
	// page of every page in the collection.
	cmd, _ := fromFind["cmd"].(string)
	if !strings.Contains(cmd, "_status eq draft") {
		t.Errorf("next.cmd = %q, want it to carry the --where", cmd)
	}
	if got, want := cmd, fromCount["cmd"]; got != want {
		t.Errorf("the two counting paths hand off differently:\n  find --count-only: %q\n  count:             %q", got, want)
	}
}

// TestVerifyWriteHandoffsCarryTheProfile is the nextcmd collateral.
//
// Every verify_write next.cmd was built as a bare `pay get <coll> <id>`, so an
// agent working against a non-default profile that copied the hand-off verbatim
// silently read the DEFAULT server instead of the one it had just written to.
// SKILL.md promises the emitted command is literally runnable.
func TestVerifyWriteHandoffsCarryTheProfile(t *testing.T) {
	if got := (&Deps{RT: &Runtime{Cfg: &config.Resolved{
		Profile: "staging", ProfileDefined: true,
	}}}).profileFlag(); got != " --profile staging" {
		t.Errorf("profileFlag() = %q, want \" --profile staging\"", got)
	}

	// A profile no config file defines must NOT be re-emitted: `--profile
	// default` on an env-configured, config-less run is a profile_unknown
	// error, which is a worse failure than omitting the flag. This mirrors
	// reinvocation()'s existing rule.
	for _, cfg := range []*config.Resolved{
		{Profile: "default", ProfileDefined: false},
		{Profile: "", ProfileDefined: true},
	} {
		if got := (&Deps{RT: &Runtime{Cfg: cfg}}).profileFlag(); got != "" {
			t.Errorf("profileFlag() for %+v = %q, want empty", cfg, got)
		}
	}
	// A profile name needing quoting is shell-quoted, not concatenated raw.
	if got := (&Deps{RT: &Runtime{Cfg: &config.Resolved{
		Profile: "my prof", ProfileDefined: true,
	}}}).profileFlag(); !strings.Contains(got, "'my prof'") {
		t.Errorf("profileFlag() = %q, want the name quoted", got)
	}
}
