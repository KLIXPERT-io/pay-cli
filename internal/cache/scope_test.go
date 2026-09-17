package cache

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLayoutPaths(t *testing.T) {
	s, sc := newTestStore(t)
	root := s.Root()

	want := map[string]string{
		"epoch":    filepath.Join(root, "v1"),
		"scope":    filepath.Join(root, "v1", sc.Key),
		"manifest": filepath.Join(root, "v1", sc.Key, "manifest.json"),
		"lock":     filepath.Join(root, "v1", sc.Key, ".lock"),
		"auth":     filepath.Join(root, "v1", "auth-resolution.json"),
	}
	got := map[string]string{
		"epoch":    s.EpochDir(),
		"scope":    s.ScopeDir(sc),
		"manifest": s.ManifestPath(sc),
		"lock":     s.LockPath(sc),
		"auth":     s.AuthResolutionPath(),
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s path = %s, want %s", k, got[k], w)
		}
	}

	shard, ok := s.shardPath(sc, ShardName("pages", KindCollection))
	if !ok || shard != filepath.Join(root, "v1", sc.Key, "fields", "pages.json") {
		t.Errorf("shard path = %s (ok=%v)", shard, ok)
	}
	gql, ok := s.graphQLPath(sc, "Page")
	if !ok || gql != filepath.Join(root, "v1", sc.Key, "graphql", "Page.json") {
		t.Errorf("graphql path = %s (ok=%v)", gql, ok)
	}
}

func TestShardName(t *testing.T) {
	if got, want := ShardName("pages", KindCollection), "fields/pages.json"; got != want {
		t.Errorf("collection shard = %q, want %q", got, want)
	}
	if got, want := ShardName("crm-brand", KindGlobal), "fields/_global_crm-brand.json"; got != want {
		t.Errorf("global shard = %q, want %q", got, want)
	}
	// A global and a collection with the same slug must not collide.
	if ShardName("nav", KindCollection) == ShardName("nav", KindGlobal) {
		t.Error("a global and a collection shard collided")
	}
}

func TestValidShardRef(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"fields/pages.json", true},
		{"fields/_global_crm-brand.json", true},
		{"fields/crm-contacts.json", true},
		{"", false},
		{"pages.json", false},
		{"graphql/Page.json", false},
		{"fields/../../etc/passwd", false},
		{"fields/../manifest.json", false},
		{"/etc/passwd", false},
		{`fields\pages.json`, false},
		{"fields/pages.txt", false},
		{"fields/.json", false},
	}
	for _, tc := range tests {
		if got := ValidShardRef(tc.in); got != tc.want {
			t.Errorf("ValidShardRef(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestValidSlug(t *testing.T) {
	for _, ok := range []string{"pages", "crm-contacts", "payload-jobs-stats", "a_b.c"} {
		if !ValidSlug(ok) {
			t.Errorf("ValidSlug(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", "..", "../x", "a/b", "-leading", ".hidden", "with space"} {
		if ValidSlug(bad) {
			t.Errorf("ValidSlug(%q) = true", bad)
		}
	}
}

func TestGraphQLTypeNamesAreValidated(t *testing.T) {
	s, sc := newTestStore(t)
	for _, bad := range []string{"", "../escape", "Page.json", "a/b", "1Page"} {
		if _, ok := s.graphQLPath(sc, bad); ok {
			t.Errorf("graphQLPath accepted %q", bad)
		}
	}
}

func TestScopesListing(t *testing.T) {
	s, sc := newTestStore(t)
	writeFixture(t, s, sc, NewGeneration(testNow), "pages", "media")

	other := base()
	other.Credential = "a-second-credential-entirely"
	sc2 := mustScope(t, other)
	gen2 := NewGeneration(testNow)
	doc := manifestDoc(sc2, gen2)
	doc["meta"].(map[string]any)["confirmed_at"] = testNow.Add(-time.Hour).Format(time.RFC3339)
	ok, warns := s.WriteSet(sc2, Set{Generation: gen2, Manifest: doc}, testNow)
	if !ok {
		t.Fatalf("WriteSet: %+v", warns)
	}

	infos := s.Scopes()
	if len(infos) != 2 {
		t.Fatalf("got %d scopes, want 2", len(infos))
	}
	// Newest confirmed_at first.
	if infos[0].Scope != sc.Key {
		t.Fatalf("scopes are not ordered by confirmed_at: %+v", infos)
	}
	first := infos[0]
	if !first.Readable || first.Collections != 2 || first.Shards != 2 || first.Bytes <= 0 {
		t.Fatalf("unexpected scope info: %+v", first)
	}
	if first.KeyFP != sc.KeyFingerprint || first.APIPath != "/api" {
		t.Fatalf("scope info identity is wrong: %+v", first)
	}
	// `pay cache ls` must never be able to print a credential.
	for _, info := range infos {
		if strings.Contains(info.KeyFP, base().Credential) {
			t.Fatal("scope listing leaked a credential")
		}
	}
}

func TestScopesIgnoresNonScopeDirectories(t *testing.T) {
	s, sc := newTestStore(t)
	writeFixture(t, s, sc, NewGeneration(testNow), "pages")
	mustWriteFile(t, filepath.Join(s.EpochDir(), "not-a-scope", "manifest.json"), []byte(`{}`))
	mustWriteFile(t, filepath.Join(s.EpochDir(), sc.Key+trashInfix+NewGeneration(testNow), "manifest.json"), []byte(`{}`))

	if got := len(s.Scopes()); got != 1 {
		t.Fatalf("got %d scopes, want 1", got)
	}
}

func TestAuthResolutionKeyIgnoresHeadersAndGraphQL(t *testing.T) {
	a := base()
	a.Headers = map[string]string{"X-Bypass": "1"}
	b := base()
	b.GraphQLPath = "/graphql"

	if AuthResolutionKey(mustScope(t, a)) != AuthResolutionKey(mustScope(t, b)) {
		t.Fatal("the auth-resolution key must depend only on (normURL, keyFP)")
	}
	c := base()
	c.Credential = "different"
	if AuthResolutionKey(mustScope(t, a)) == AuthResolutionKey(mustScope(t, c)) {
		t.Fatal("the auth-resolution key ignored the credential")
	}
}
