package discovery

import (
	"strconv"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/cache"
)

// A block field, blockType or entity whose NAME looks like a secret (hash,
// csrfToken, a `tokenList` block, an `api-tokens` collection) must survive the
// cache: the census keys its maps by schema names, and the cache's
// document-style key redaction used to turn those profiles into
// "<redacted>", which no longer decoded — the census was silently rebuilt by
// every command. Observed VALUES of such fields are never sampled at all.
func TestCensusWithSecretLookingNamesSurvivesTheCache(t *testing.T) {
	b := NewCensusBuilder(EntityKindCollection, "api-tokens")
	for i := 1; i <= 5; i++ {
		b.AddDoc(i, decodeDoc(t, `{"id":`+strconv.Itoa(i)+`,"layout":[`+
			`{"id":"a","blockType":"embed","hash":"#top","hubspotFormToken":"tok-123","title":"T","variant":"wide"},`+
			`{"id":"b","blockType":"tokenList","clientSecret":{"sessions":"s"},"items":[{"csrfToken":"c","label":"L"}]}]}`))
	}
	e := b.Finish()
	sc, err := cache.NewScope(cache.ScopeInput{BaseURL: "https://example.test", APIPath: "/api",
		GraphQLPath: "/api/graphql", KeyFingerprint: cache.AnonKeyFingerprint})
	if err != nil {
		t.Fatal(err)
	}
	c := NewCensus(sc.Key)
	c.Put(e)
	store := cache.New(t.TempDir())
	if ok, w := store.WriteCensus(sc, c); !ok {
		t.Fatalf("write: %v", w)
	}
	raw, ok, w := store.ReadCensus(sc)
	if !ok || w != nil {
		t.Fatalf("read: ok=%v warning=%v", ok, w)
	}
	if strings.Contains(string(raw), "tok-123") || strings.Contains(string(raw), "#top") {
		t.Error("the value of a secret-named field reached the cache")
	}
	back, err := DecodeCensusErr(raw)
	if err != nil {
		t.Fatalf("a census with secret-looking names did not decode: %v\n%s", err, raw)
	}
	got, ok := back.Entity(EntityKindCollection, "api-tokens")
	if !ok {
		t.Fatal("entity lost")
	}
	embed := got.Types["embed"]
	if embed == nil || embed.Fields["hash"] == nil || embed.Fields["hubspotFormToken"] == nil {
		t.Fatalf("embed fields = %+v", embed)
	}
	if f := embed.Fields["hubspotFormToken"]; f.Kind != CensusKindScalar || f.Present != 5 || len(f.Values) != 0 {
		t.Errorf("hubspotFormToken profile = %+v", f)
	}
	if f := embed.Fields["variant"]; len(f.Values) == 0 {
		t.Error("an ordinary field lost its values")
	}
	list := got.Types["tokenList"]
	if list == nil || list.Fields["clientSecret"] == nil || list.Fields["clientSecret"].Kind != CensusKindGroup ||
		list.Fields["clientSecret"].Fields["sessions"] == nil ||
		list.Fields["items"] == nil || list.Fields["items"].Fields["csrfToken"] == nil {
		t.Fatalf("tokenList = %+v", list)
	}
	if got.Paths["layout"] == nil || got.Paths["layout"].Types["tokenList"] != 5 {
		t.Errorf("layout pattern = %+v", got.Paths["layout"])
	}
}

func TestDecodeCensusErrSaysWhy(t *testing.T) {
	if _, err := DecodeCensusErr([]byte(`{"census_version":99}`)); err == nil || !strings.Contains(err.Error(), "99") {
		t.Errorf("foreign version: %v", err)
	}
	if _, err := DecodeCensusErr([]byte(`{"census_version":1,"collections":{"p":{"types":{"x":{"fields":{"hash":"<redacted>"}}}}}}`)); err == nil {
		t.Error("a key-redacted field profile decoded")
	}
}
