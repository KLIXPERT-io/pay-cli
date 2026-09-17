package cli

import (
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/cache"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/payloadtest"
	"github.com/KLIXPERT-io/pay-cli/internal/safety"
	"github.com/spf13/cobra"
)

// TestDeletePlan is §12.4's soft-delete-by-default rule.
func TestDeletePlan(t *testing.T) {
	tests := []struct {
		name           string
		trashEnabled   bool
		permanent      bool
		wantSoft       bool
		wantMethod     string
		wantTrashParam bool
	}{
		{"trash-enabled default is a soft delete", true, false, true, "PATCH", false},
		{"--permanent on a trash collection adds trash=true", true, true, false, "DELETE", true},
		{"no trash is always a hard delete", false, false, false, "DELETE", false},
		{"no trash with --permanent", false, true, false, "DELETE", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := deletePlan(tc.trashEnabled, tc.permanent)
			if got.Soft != tc.wantSoft || got.Method != tc.wantMethod || got.SendTrashParam != tc.wantTrashParam {
				t.Fatalf("deletePlan = %+v, want soft=%v method=%s trash=%v",
					got, tc.wantSoft, tc.wantMethod, tc.wantTrashParam)
			}
		})
	}
}

// TestDeleteRiskLevels encodes §12.1 and §12.4: a soft delete is recoverable
// and therefore L1; a delete with no trash to fall back on is L2.
func TestDeleteRiskLevels(t *testing.T) {
	tests := []struct {
		name string
		op   safety.Op
		want safety.Level
	}{
		{"soft delete by id", safety.Op{Command: safety.CmdDelete, Selector: safety.SelectorID, TrashEnabled: true}, safety.L1},
		{"permanent delete by id", safety.Op{Command: safety.CmdDelete, Selector: safety.SelectorID, TrashEnabled: true, Permanent: true}, safety.L2},
		{"delete on a collection with no trash", safety.Op{Command: safety.CmdDelete, Selector: safety.SelectorID}, safety.L2},
		{"bulk delete", safety.Op{Command: safety.CmdDelete, Selector: safety.SelectorBulk}, safety.L3},
		{"find", safety.Op{Command: safety.CmdFind}, safety.L0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.op.Level(); got != tc.want {
				t.Fatalf("level = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTrashFieldComesFromTheManifest covers §12.4's "never hardcoded" rule as
// far as the manifest can express it.
func TestTrashFieldComesFromTheManifest(t *testing.T) {
	s := discovery.NewShard("g", "pages")
	s.Fields = []discovery.Field{testField("deletedAt", discovery.TypeDate)}
	s.Finalize()
	if got := trashField(s); got != "deletedAt" {
		t.Fatalf("trashField = %q", got)
	}
	// A shard PayCLI could not read falls back to Payload's default rather
	// than failing the command.
	if got := trashField(nil); got != defaultTrashField {
		t.Fatalf("trashField(nil) = %q", got)
	}
}

func TestDeleteCommandWiring(t *testing.T) {
	cmd := newDeleteCmd(nil)
	for _, name := range []string{"permanent", "where", "max-docs", "all", "limit", "unsafe-passthrough-where"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("pay delete is missing --%s", name)
		}
	}
	for _, phrase := range []string{
		"Bulk DELETE on the server IGNORES limit",
		"never passes --where through to",
		"chunks of 100",
	} {
		if !strings.Contains(cmd.Long, phrase) {
			t.Fatalf("delete help is missing %q", phrase)
		}
	}
}

// TestEveryDataCommandBuilds catches a duplicate flag registration, which
// pflag answers with a panic at construction time.
func TestEveryDataCommandBuilds(t *testing.T) {
	factories := map[string]func(*Runtime) *cobra.Command{
		"find":      newFindCmd,
		"get":       newGetCmd,
		"count":     newCountCmd,
		"create":    newCreateCmd,
		"update":    newUpdateCmd,
		"delete":    newDeleteCmd,
		"restore":   newRestoreCmd,
		"duplicate": newDuplicateCmd,
		"publish":   newPublishCmd,
		"unpublish": newUnpublishCmd,
		"upload":    newUploadCmd,
		"download":  newDownloadCmd,
		"globals":   newGlobalsCmd,
		"versions":  newVersionsCmd,
	}
	for name, f := range factories {
		t.Run(name, func(t *testing.T) {
			cmd := f(nil)
			if cmd == nil {
				t.Fatal("nil command")
			}
			if cmd.Short == "" {
				t.Fatal("every command needs a Short description")
			}
			if cmd.RunE == nil && !cmd.HasSubCommands() {
				t.Fatal("a leaf command needs a RunE")
			}
		})
	}
}

// TestNonPositiveMaxDocsIsRejected is finding-11's regression: an explicit
// `--max-docs 0` used to fall through to defaults.max_bulk, so the cap the user
// typed was discarded and the refusal quoted a number they never wrote. It is
// rejected before anything is sent, on the bulk form and the scoped one alike.
func TestNonPositiveMaxDocsIsRejected(t *testing.T) {
	cases := [][]string{
		{"delete", "pages", "--where", "id gt 0", "--max-docs", "0", "--dry-run"},
		{"delete", "pages", "--where", "id gt 0", "--max-docs", "-1", "--dry-run"},
		{"delete", "pages", "16", "--max-docs", "0", "--dry-run"},
		{"update", "pages", "--where", "id gt 0", "--max-docs", "0", "--set", "title=x", "--dry-run"},
		{"update", "pages", "16", "--max-docs", "-5", "--set", "title=x", "--dry-run"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			srv := payloadtest.NewServer(t)
			home := t.TempDir()
			seedDiscovery(t, home, srv.URL)
			res := cliRun(t, invocation{Home: home, Args: args, Env: seededEnv(srv.URL)})
			if res.Code != apierr.ExitValidation {
				t.Fatalf("exit = %d, want %d\n%s", res.Code, apierr.ExitValidation, res.Stdout)
			}
			if got := res.code(t); got != string(apierr.CodeInvalidArgs) {
				t.Fatalf("code = %q, want invalid_args\n%s", got, res.Stdout)
			}
			if n := len(srv.Requests()); n != 0 {
				t.Fatalf("an invalid cap must be rejected before anything is sent; %d request(s): %+v", n, srv.Requests())
			}
		})
	}
	// A positive cap is still honoured.
	srv := payloadtest.NewServer(t)
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)
	res := cliRun(t, invocation{
		Home: home,
		Args: []string{"delete", "pages", "16", "--max-docs", "5", "--dry-run"},
		Env:  seededEnv(srv.URL),
	})
	if res.Code != 0 {
		t.Fatalf("exit = %d\n%s", res.Code, res.Stdout)
	}
}

// seedTrashCollectionShard adds a field shard for a trash-enabled collection to
// the already-seeded cache, so a --where over it resolves entirely offline.
func seedTrashCollectionShard(t *testing.T, home, baseURL, slug string) {
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

	now := payloadtest.Epoch
	generation := cache.NewGeneration(now)
	m.Generation = generation
	m.Meta.Scope = sc.Key
	m.GeneratedAt = now
	m.ExpiresAt = now.Add(24 * time.Hour)

	// Finalize AFTER the generation is set: it is what seals the checksum the
	// cache verifies on read.
	shard := discovery.NewShard(generation, slug)
	shard.Fields = []discovery.Field{
		testField("id", discovery.TypeNumber),
		testField("status", discovery.TypeText),
		testField("deletedAt", discovery.TypeDate),
	}
	shard.Finalize()

	var pages discovery.Shard
	payloadtest.LoadJSON(t, "fields_pages", &pages)
	pages.Generation = generation

	store := cache.New(filepath.Join(home, "cache"))
	ok, warns := store.WriteSet(sc, cache.Set{
		Generation: generation,
		Manifest:   &m,
		Shards: map[string]any{
			cache.ShardName(slug, cache.KindCollection):       &shard,
			cache.ShardName(pages.Slug, cache.KindCollection): &pages,
		},
	}, now)
	if !ok {
		t.Fatalf("seed cache write failed: %v", warns)
	}
}

// TestUnsafePassthroughDeleteTellsTheTruth is finding 14.
//
// `delete <trash-collection> --where … --unsafe-passthrough-where` puts a raw
// DELETE on the wire: the server has no concept of trash on that route, so the
// documents are purged, not trashed. Every safety signal used to describe the
// soft delete that WOULD have happened without the flag — would_affect 0 (the
// count phase was skipped and -1 collapsed to 0), a prompt that said "trash",
// no IRREVERSIBLE line, and an audit record claiming action=trash method=PATCH.
// An agent following the documented dry-run-then---yes workflow read "0
// documents, trash" and purged the collection.
func TestUnsafePassthroughDeleteTellsTheTruth(t *testing.T) {
	const total = 7
	var counted int
	srv := payloadtest.NewServer(t,
		payloadtest.WithHandler("GET", "/api/crm-contacts/count", func(w http.ResponseWriter, _ *http.Request) {
			counted++
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("X-Powered-By", "Payload")
			_, _ = io.WriteString(w, `{"totalDocs":7}`)
		}),
		payloadtest.WithHandler("GET", "/api/crm-contacts", func(w http.ResponseWriter, _ *http.Request) {
			counted++
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("X-Powered-By", "Payload")
			_, _ = io.WriteString(w, `{"docs":[],"totalDocs":7,"limit":0,"page":1,"totalPages":1}`)
		}))
	home := t.TempDir()
	seedDiscovery(t, home, srv.URL)
	seedTrashCollectionShard(t, home, srv.URL, "crm-contacts")

	res := cliRun(t, invocation{
		Home: home,
		Args: []string{"delete", "crm-contacts", "--where", "status eq lead",
			"--unsafe-passthrough-where", "--dry-run"},
		Env: seededEnv(srv.URL),
	})
	if res.Code != 0 {
		t.Fatalf("exit = %d, want 0\nstdout: %s\nstderr: %s", res.Code, res.Stdout, res.Stderr)
	}

	data, _ := res.Env["data"].(map[string]any)
	if data == nil {
		t.Fatalf("no data in envelope: %s", res.Stdout)
	}
	// 1. The blast radius is the real count, not a collapsed -1.
	if got := fmt.Sprintf("%v", data["would_affect"]); got != fmt.Sprint(total) {
		t.Errorf("would_affect = %s, want %d (the phase-1 count must run on this path)", got, total)
	}
	if counted == 0 {
		t.Error("the passthrough path never counted: phase 1 was skipped")
	}
	// 2. The previewed request is the DELETE that will actually be sent.
	req, _ := data["request"].(map[string]any)
	if req == nil || req["method"] != "DELETE" {
		t.Errorf("request.method = %v, want DELETE", req["method"])
	}
	// 3. The §12.4 disclosure reaches the dry run, and names a hard delete.
	if !strings.Contains(res.Stderr, "IRREVERSIBLE") {
		t.Errorf("stderr has no IRREVERSIBLE note:\n%s", res.Stderr)
	}
	if strings.Contains(res.Stderr, "trash will affect") {
		t.Errorf("stderr calls a hard DELETE a trash:\n%s", res.Stderr)
	}
	if !strings.Contains(res.Stderr, "delete will affect 7 document(s)") {
		t.Errorf("stderr does not state the resolved match count:\n%s", res.Stderr)
	}
	// 4. The warning is on the PREVIEW, not only on the executed envelope.
	warns, _ := res.Env["warnings"].([]any)
	found := false
	for _, w := range warns {
		if m, ok := w.(map[string]any); ok && m["code"] == "unsafe_passthrough_where" {
			found = true
		}
	}
	if !found {
		t.Errorf("dry run carries no unsafe_passthrough_where warning: %v", warns)
	}
}

// TestUnsafePassthroughOpIsAHardDelete pins the safety.Op half of finding 14:
// the op must describe the request on the wire, not the one the trash flag
// would have produced.
func TestUnsafePassthroughOpIsAHardDelete(t *testing.T) {
	op := safety.Op{
		Command:      safety.CmdDelete,
		Selector:     safety.SelectorBulk,
		TrashEnabled: true,  // trash exists…
		Permanent:    false, // …and --permanent was NOT passed…
		HardDelete:   true,  // …but the raw DELETE bypasses it anyway.
	}
	if op.SoftDelete() {
		t.Error("SoftDelete() is true for a raw passthrough DELETE")
	}
	if !op.Irreversible() {
		t.Error("Irreversible() is false, so the §12.4 stderr warning never fires")
	}
	if got := op.Action(); got != safety.ActionDelete {
		t.Errorf("Action() = %q, want %q (the audit record must not say trash)", got, safety.ActionDelete)
	}
	if got := op.IrreversibleReason(); !strings.Contains(got, "unsafe-passthrough-where") {
		t.Errorf("IrreversibleReason() = %q, want it to blame the flag", got)
	}
}
