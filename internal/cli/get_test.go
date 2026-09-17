package cli

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/cache"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payloadtest"
)

// TestGetChecksTheIDType is §9.3's invalid_id row together with its tri-state
// gate: a bad id is rejected locally only when the id type is actually known.
func TestGetChecksTheIDType(t *testing.T) {
	tests := []struct {
		name    string
		idType  string
		id      string
		wantErr bool
	}{
		{"numeric collection rejects an ObjectId", "number", "66f1a2b3c4d5e6f708192a3b", true},
		{"numeric collection accepts a number", "number", "16", false},
		{"string collection accepts an ObjectId", "string", "66f1a2b3c4d5e6f708192a3b", false},
		{"string collection rejects an empty id", "string", " ", true},
		{"unknown id type never rejects", "", "66f1a2b3c4d5e6f708192a3b", false},
		{"unknown id type never rejects a number either", "unknown", "16", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := apierr.CheckID("pages", tc.id, tc.idType)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v", err)
			}
			if err != nil {
				if err.Code != apierr.CodeInvalidID {
					t.Fatalf("code = %s", err.Code)
				}
				if err.Exit != apierr.ExitValidation {
					t.Fatalf("exit = %d, want 5", err.Exit)
				}
			}
		})
	}
}

func TestGetCommandWiring(t *testing.T) {
	cmd := newGetCmd(nil)
	for _, name := range []string{"depth", "select", "populate", "draft", "trash", "locale"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("pay get is missing --%s", name)
		}
	}
	if cmd.Args == nil {
		t.Fatal("pay get must take exactly <collection> <id>")
	}
}

// seedUnknownIDType seeds a manifest in which no collection's id type could be
// determined — the §21 A2/A3 portability case where GraphQL is off and the
// probes were inconclusive.
func seedUnknownIDType(t *testing.T, home, baseURL string) {
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
	payloadtest.LoadJSON(t, "manifest_id_unknown", &m)
	var shard discovery.Shard
	payloadtest.LoadJSON(t, "fields_pages", &shard)

	now := payloadtest.Epoch
	generation := cache.NewGeneration(now)
	m.Generation = generation
	m.Meta.Scope = sc.Key
	m.GeneratedAt = now
	m.ExpiresAt = now.Add(24 * time.Hour)
	shard.Generation = generation

	store := cache.New(filepath.Join(home, "cache"))
	if ok, warns := store.WriteSet(sc, cache.Set{
		Generation: generation,
		Manifest:   &m,
		Shards:     map[string]any{cache.ShardName(shard.Slug, cache.KindCollection): &shard},
	}, now); !ok {
		t.Fatalf("seed cache write failed: %v", warns)
	}
}

// TestIDTypeUnknownIsAnnounced is finding 21's second half.
//
// §7.6(a) makes the warning mandatory whenever the local id check is SKIPPED:
// without it, `pay get pages not-a-number` on a project whose id type was never
// discovered looks exactly like a project where PayCLI checked the id and
// approved it — and the agent learns about the problem only from an opaque
// server 500. output.IDTypeUnknownWarning existed but no command called it.
func TestIDTypeUnknownIsAnnounced(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"get", []string{"get", "pages", "not-a-number"}},
		{"delete", []string{"delete", "pages", "not-a-number", "--dry-run"}},
		{"duplicate", []string{"duplicate", "pages", "not-a-number", "--dry-run"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := payloadtest.NewServer(t)
			home := t.TempDir()
			seedUnknownIDType(t, home, srv.URL)

			res := cliRun(t, invocation{
				Home: home, Args: tc.args, Env: seededEnv(srv.URL),
			})
			warns, _ := res.Env["warnings"].([]any)
			found := false
			for _, w := range warns {
				m, ok := w.(map[string]any)
				if !ok || m["code"] != output.WarnIDTypeUnknown {
					continue
				}
				found = true
				if msg, _ := m["message"].(string); !strings.Contains(msg, `"pages"`) {
					t.Errorf("warning does not name the collection: %q", msg)
				}
			}
			if !found {
				t.Fatalf("no %s warning; the skipped id check is invisible.\nwarnings: %v\nstdout: %s",
					output.WarnIDTypeUnknown, warns, res.Stdout)
			}
		})
	}
}

// TestIDTypeKnownIsSilent is the other half: the warning must NOT fire when the
// id type IS known, or it becomes noise the agent learns to ignore.
func TestIDTypeKnownIsSilent(t *testing.T) {
	srv := payloadtest.NewServer(t)
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL) // manifest.json: pages ids are numbers

	res := cliRun(t, invocation{
		Home: home, Args: []string{"get", "pages", "not-a-number"}, Env: seededEnv(srv.URL),
	})
	warns, _ := res.Env["warnings"].([]any)
	for _, w := range warns {
		if m, ok := w.(map[string]any); ok && m["code"] == output.WarnIDTypeUnknown {
			t.Fatalf("id_type_unknown fired on a collection whose id type IS known: %v", m)
		}
	}
	// And the local check still rejects the id rather than sending it.
	if res.Code == 0 {
		t.Errorf("exit = 0, want a local invalid_id refusal")
	}
}
