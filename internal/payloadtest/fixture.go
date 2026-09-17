// Package payloadtest is PayCLI's hermetic test harness: a fixture-backed
// Payload server, a golden-file comparator and a deterministic clock.
//
// It is imported only by _test.go files. Fixtures are plain, pretty-printed,
// key-sorted JSON that a human or an agent can `cat` and diff — go-vcr's
// cassettes were rejected precisely because they are not (§17.2).
package payloadtest

import (
	"encoding/json"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FixtureDirName and GoldenDirName are the two directories under testdata/.
const (
	FixtureDirName = "fixtures"
	GoldenDirName  = "golden"
	// IndexName is the routing index NewServer reads.
	//
	// §17.2 calls it "manifest.json", but testdata/fixtures/manifest.json is
	// itself a fixture — a recorded discovery manifest (§3's fixture list). Two
	// different documents cannot share one name, so the router's index is
	// _index.json and manifest.json stays what §3 says it is.
	IndexName = "_index.json"
)

// Route is one recorded request/response pair.
type Route struct {
	Name        string `json:"name"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	Query       string `json:"query,omitempty"`
	Status      int    `json:"status"`
	ContentType string `json:"content_type"`
	File        string `json:"file"`
	// Headers are extra response headers to replay, e.g. Retry-After.
	Headers map[string]string `json:"headers,omitempty"`

	// The three matchers below disambiguate fixtures that share a method and
	// path. Payload's GraphQL endpoint is one URL for every query, /me answers
	// differently with and without a credential, and a German validation error
	// is the same request with one extra header (§17.1's regression guard).
	MatchBody      string            `json:"match_body,omitempty"`
	MatchHeader    map[string]string `json:"match_header,omitempty"`
	MatchAnonymous bool              `json:"match_anonymous,omitempty"`
}

// specificity ranks a route so the most constrained fixture wins a tie.
func (r Route) specificity() int {
	n := 0
	if r.MatchBody != "" {
		n += 4
	}
	n += 2 * len(r.MatchHeader)
	if r.MatchAnonymous {
		n++
	}
	return n
}

// matches reports whether a recorded route can answer this request.
func (r Route) matches(method, path, query, body string, header map[string][]string) bool {
	if r.Method != method || r.Path != path {
		return false
	}
	if r.MatchBody != "" && !strings.Contains(body, r.MatchBody) {
		return false
	}
	for name, want := range r.MatchHeader {
		if !headerHas(header, name, want) {
			return false
		}
	}
	authorized := headerPresent(header, "Authorization")
	if r.MatchAnonymous && authorized {
		return false
	}
	return true
}

func headerHas(header map[string][]string, name, want string) bool {
	for _, v := range header[textproto.CanonicalMIMEHeaderKey(name)] {
		if strings.EqualFold(v, want) {
			return true
		}
	}
	return false
}

func headerPresent(header map[string][]string, name string) bool {
	return len(header[textproto.CanonicalMIMEHeaderKey(name)]) > 0
}

// Index is testdata/fixtures/_index.json.
type Index struct {
	PayloadVersion string  `json:"payload_version"`
	RecordedAt     string  `json:"recorded_at"`
	BaseURL        string  `json:"base_url"`
	Requests       []Route `json:"requests"`
}

// repoRoot walks up from the working directory until it finds testdata/. Tests
// run with the package directory as the working directory, so the depth varies
// with the package.
func repoRoot(t testing.TB) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("payloadtest: getwd: %v", err)
	}
	for i := 0; i < 12; i++ {
		candidate := filepath.Join(dir, "testdata", FixtureDirName)
		if st, err := os.Stat(candidate); err == nil && st.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("payloadtest: no testdata/%s directory above the working directory", FixtureDirName)
	return ""
}

// FixtureDir is the absolute path of testdata/fixtures.
func FixtureDir(t testing.TB) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "testdata", FixtureDirName)
}

// GoldenDir is the absolute path of testdata/golden.
func GoldenDir(t testing.TB) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "testdata", GoldenDirName)
}

// FixturePath resolves one fixture by name, with or without the .json suffix.
func FixturePath(t testing.TB, name string) string {
	t.Helper()
	if filepath.Ext(name) == "" {
		name += ".json"
	}
	return filepath.Join(FixtureDir(t), name)
}

// Load reads one fixture's bytes. A missing fixture is a test failure naming
// the command that re-records them.
func Load(t testing.TB, name string) []byte {
	t.Helper()
	path := FixturePath(t, name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("payloadtest: %v\nrun \"make fixtures\" to re-record", err)
	}
	return data
}

// LoadJSON decodes one fixture into v.
func LoadJSON(t testing.TB, name string, v any) {
	t.Helper()
	if err := json.Unmarshal(Load(t, name), v); err != nil {
		t.Fatalf("payloadtest: %s is not valid JSON: %v", name, err)
	}
}

// Has reports whether a fixture exists, for tests that adapt to what was
// recorded rather than failing on an optional one.
func Has(t testing.TB, name string) bool {
	t.Helper()
	_, err := os.Stat(FixturePath(t, name))
	return err == nil
}

// LoadIndex reads the routing index.
func LoadIndex(t testing.TB) Index {
	t.Helper()
	var idx Index
	LoadJSON(t, IndexName, &idx)
	return idx
}
