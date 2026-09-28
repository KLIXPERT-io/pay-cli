package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/cache"
)

// A census.json whose header matches but whose body this build cannot decode
// (e.g. one an older build wrote with a field profile key-redacted to
// "<redacted>") is a miss PLUS a cache_unreadable warning (§8.3), never a
// silent miss.
func TestUndecodableCensusWarns(t *testing.T) {
	home := t.TempDir()
	sc := seedDiscovery(t, home, testBaseURL)
	store := cache.New(filepath.Join(home, "cache"))
	body := `{"census_version":1,"scope":"` + sc.Key + `","collections":{"pages":{"slug":"pages","types":{"embed":{"fields":{"hash":"<redacted>"}}}}}}`
	if err := os.WriteFile(store.CensusPath(sc), []byte(body), cache.FilePerm); err != nil {
		t.Fatal(err)
	}
	doc := pageEnvelope(t, `{"id":1,"layout":[{"id":"a","blockType":"hero","title":"T"}]}`)
	r := censusRun(t, home, doc, "outline")
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stdout)
	}
	w := censusWarning(t, r, cache.WarnCacheUnreadable)
	if w == nil {
		t.Fatalf("no cache_unreadable warning for an undecodable census: %s", r.Stdout)
	}
	if w["hint"] != "pay blocks learn" {
		t.Errorf("hint = %v", w["hint"])
	}
}
