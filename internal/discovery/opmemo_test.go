package discovery

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
)

func at(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// §7.11 records an operator failure only when it is "provably caused by
// operator O". These cases pin what "provably" means, because the loose reading
// ("a 500 came back from a request that had an operator in it") would let one
// flaky 502 on `slug eq home` teach PayCLI that `equals` is broken and warn on
// every query for the rest of the cache's life.
func TestAttributeOperatorFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		serverFailed bool
		operators    []string
		want         string
		wantOK       bool
	}{
		{"one gated operator on a 500", true, []string{"all"}, "all", true},
		{"repeats of the same operator still name one", true, []string{"near", "near"}, "near", true},
		{"two operators prove nothing", true, []string{"all", "equals"}, "", false},
		{"a core operator is never blamed", true, []string{"equals"}, "", false},
		{"no operators", true, nil, "", false},
		{"blank operators", true, []string{"", "  "}, "", false},
		{"a success proves nothing", false, []string{"all"}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := AttributeOperatorFailure(tc.serverFailed, tc.operators)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("AttributeOperatorFailure(%v, %v) = %q,%v; want %q,%v",
					tc.serverFailed, tc.operators, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// The learning list and query.CheckOperator's blocking list are two rules over
// the same four operators — §7.11 blocks pre-emptively only on a pinned
// Postgres, and learns everywhere else. They are declared in different packages
// (query cannot import discovery), so this is what keeps them from drifting.
func TestOperatorFailureCandidatesTrackTheAdapterGatedSet(t *testing.T) {
	t.Parallel()
	for _, op := range query.Operators {
		blocked := query.CheckOperator(op, "postgres", "configured") != nil
		learnable := OperatorFailureCandidate(op)
		if blocked != learnable {
			t.Errorf("operator %q: query blocks it = %v, the memo learns it = %v; the two §7.11 lists have drifted",
				op, blocked, learnable)
		}
	}
	if OperatorFailureCandidate("equals") {
		t.Error("equals must never be learnable: every query uses it")
	}
}

func TestRecordOperatorFailure(t *testing.T) {
	t.Parallel()
	m := NewManifest()
	if len(m.LearnedOperatorFailures) != 0 {
		t.Fatalf("a fresh manifest already carries a memo: %v", m.LearnedOperatorFailures)
	}

	t0 := at("2026-01-01T00:00:00Z")
	if !m.RecordOperatorFailure("all", "crm-contacts", "HTTP 500 Something went wrong.", t0) {
		t.Fatal("the first record reported no change")
	}
	if got := len(m.LearnedOperatorFailures); got != 1 {
		t.Fatalf("len = %d, want 1", got)
	}

	// Same pair, same evidence, same clock: nothing changed, so the caller must
	// be told it can skip the cache rewrite.
	if m.RecordOperatorFailure("all", "crm-contacts", "HTTP 500 Something went wrong.", t0) {
		t.Error("an identical re-record reported a change")
	}
	if got := len(m.LearnedOperatorFailures); got != 1 {
		t.Errorf("an identical re-record grew the memo to %d", got)
	}

	// Same pair, fresher evidence: updated in place, never appended.
	t1 := at("2026-02-02T03:04:05Z")
	if !m.RecordOperatorFailure("all", "crm-contacts", "HTTP 500 column does not exist", t1) {
		t.Error("fresher evidence reported no change")
	}
	if got := len(m.LearnedOperatorFailures); got != 1 {
		t.Fatalf("fresher evidence appended instead of updating: %d records", got)
	}
	f, ok := m.LearnedOperatorFailure("all", "crm-contacts")
	if !ok || f.Evidence != "HTTP 500 column does not exist" || !f.LearnedAt.Equal(t1) {
		t.Fatalf("record = %+v, want the fresher evidence and timestamp", f)
	}

	// The memo is per collection, not per project.
	if _, ok := m.LearnedOperatorFailure("all", "pages"); ok {
		t.Error("a failure on crm-contacts answered for pages")
	}
	if _, ok := m.LearnedOperatorFailure("near", "crm-contacts"); ok {
		t.Error("a failure of `all` answered for `near`")
	}

	// Garbage in stays out.
	if m.RecordOperatorFailure("", "pages", "x", t1) || m.RecordOperatorFailure("all", "", "x", t1) {
		t.Error("an empty operator or collection was recorded")
	}
	var nilManifest *Manifest
	if nilManifest.RecordOperatorFailure("all", "pages", "x", t1) {
		t.Error("recording on a nil manifest reported a change")
	}
	if _, ok := nilManifest.LearnedOperatorFailure("all", "pages"); ok {
		t.Error("a nil manifest answered a lookup")
	}
}

func TestRecordOperatorFailureIsBounded(t *testing.T) {
	t.Parallel()
	m := NewManifest()
	base := at("2026-01-01T00:00:00Z")
	for i := 0; i < MaxLearnedOperatorFailures+10; i++ {
		m.RecordOperatorFailure("all", "coll-"+string(rune('a'+i%26))+string(rune('a'+i/26)),
			"HTTP 500", base.Add(time.Duration(i)*time.Minute))
	}
	if got := len(m.LearnedOperatorFailures); got > MaxLearnedOperatorFailures {
		t.Fatalf("memo grew to %d records; the cap is %d", got, MaxLearnedOperatorFailures)
	}
	// The oldest record is the one that goes.
	if _, ok := m.LearnedOperatorFailure("all", "coll-aa"); ok {
		t.Error("the oldest record survived the cap")
	}
}

// §8's cache schema keeps the key whether or not the memo has anything in it,
// so an agent reading manifest.json never has to tell null from absent.
func TestLearnedOperatorFailuresRoundTripThroughJSON(t *testing.T) {
	t.Parallel()
	m := NewManifest()
	m.RecordOperatorFailure("all", "crm-contacts", "HTTP 500 Something went wrong.",
		at("2026-01-01T00:00:00Z"))
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	back, ok := DecodeManifest(b)
	if !ok {
		t.Fatal("a manifest carrying a memo no longer decodes")
	}
	f, found := back.LearnedOperatorFailure("all", "crm-contacts")
	if !found {
		t.Fatalf("the memo did not survive the round trip: %s", b)
	}
	if f.Evidence != "HTTP 500 Something went wrong." {
		t.Errorf("evidence = %q", f.Evidence)
	}
}
