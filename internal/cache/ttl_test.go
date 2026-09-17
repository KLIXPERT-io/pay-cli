package cache

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/output"
)

// TestTTLTable pins §8.5 exactly.
func TestTTLTable(t *testing.T) {
	tests := []struct {
		class   Class
		fresh   time.Duration
		hardMax time.Duration
		persist bool
	}{
		{ClassDiscoveryManifest, 10 * time.Minute, 24 * time.Hour, true},
		{ClassFieldShard, 10 * time.Minute, 24 * time.Hour, true},
		{ClassPermissions, 5 * time.Minute, 24 * time.Hour, true},
		{ClassGraphQLType, 24 * time.Hour, 7 * 24 * time.Hour, true},
		{ClassWhereInput, 24 * time.Hour, 7 * 24 * time.Hour, true},
		{ClassGraphQLMode, 24 * time.Hour, 7 * 24 * time.Hour, true},
		{ClassSkillsManifest, 6 * time.Hour, 0, true},
		{ClassIdentity, 60 * time.Second, 0, false},
		{ClassDocument, 0, 0, false},
	}
	for _, tc := range tests {
		t.Run(string(tc.class), func(t *testing.T) {
			got, ok := TTLFor(tc.class)
			if !ok {
				t.Fatalf("class %s is not in the table", tc.class)
			}
			if got.Fresh != tc.fresh || got.HardMax != tc.hardMax || got.Persist != tc.persist {
				t.Fatalf("TTLFor(%s) = %+v, want fresh=%s hardMax=%s persist=%v",
					tc.class, got, tc.fresh, tc.hardMax, tc.persist)
			}
			if got.Persist != tc.class.Persistable() {
				t.Fatal("Persistable disagrees with the table")
			}
		})
	}
	if len(Classes()) != len(tests) {
		t.Fatalf("Classes() = %v, want the %d table rows", Classes(), len(tests))
	}
	if _, ok := TTLFor("invented"); ok {
		t.Fatal("an unknown class resolved")
	}
	if Class("invented").Persistable() {
		t.Fatal("an unknown class is persistable")
	}
}

// TestDocumentDataIsNeverPersistable guards §8.3 rule 4 directly.
func TestDocumentDataIsNeverPersistable(t *testing.T) {
	for _, c := range []Class{ClassDocument, ClassIdentity} {
		if c.Persistable() {
			t.Fatalf("class %s may be written to disk", c)
		}
	}
}

func TestEvaluate(t *testing.T) {
	now := testNow
	tests := []struct {
		name  string
		class Class
		age   time.Duration
		want  Freshness
	}{
		{"manifest fresh", ClassDiscoveryManifest, 9 * time.Minute, FreshnessFresh},
		{"manifest just stale", ClassDiscoveryManifest, 11 * time.Minute, FreshnessStale},
		{"manifest at hard max", ClassDiscoveryManifest, 24 * time.Hour, FreshnessExpired},
		{"manifest past hard max", ClassDiscoveryManifest, 25 * time.Hour, FreshnessExpired},
		{"permissions stale at 6m", ClassPermissions, 6 * time.Minute, FreshnessStale},
		{"graphql fresh at 23h", ClassGraphQLType, 23 * time.Hour, FreshnessFresh},
		{"graphql stale at 25h", ClassGraphQLType, 25 * time.Hour, FreshnessStale},
		{"graphql expired at 8d", ClassGraphQLType, 8 * 24 * time.Hour, FreshnessExpired},
		{"identity fresh", ClassIdentity, 30 * time.Second, FreshnessFresh},
		{"identity has no stale window", ClassIdentity, 61 * time.Second, FreshnessExpired},
		{"skills has no stale window", ClassSkillsManifest, 7 * time.Hour, FreshnessExpired},
		{"documents are never fresh", ClassDocument, 0, FreshnessExpired},
		{"clock skew counts as age zero", ClassDiscoveryManifest, -time.Hour, FreshnessFresh},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, age := Evaluate(tc.class, now.Add(-tc.age), now)
			if got != tc.want {
				t.Fatalf("Evaluate(%s, age=%s) = %s, want %s", tc.class, tc.age, got, tc.want)
			}
			if age < 0 {
				t.Fatalf("negative age %s", age)
			}
		})
	}
	if got, _ := Evaluate(ClassDiscoveryManifest, time.Time{}, now); got != FreshnessExpired {
		t.Fatalf("a zero fetchedAt = %s, want expired", got)
	}
	if got, _ := Evaluate("invented", now, now); got != FreshnessExpired {
		t.Fatalf("an unknown class = %s, want expired", got)
	}
}

// TestRevalidateLadder is §8.5's five-step ladder.
func TestRevalidateLadder(t *testing.T) {
	const cached = "topology-abc"

	tests := []struct {
		name          string
		probe         Probe
		budget        time.Duration
		wantDiscovery string
		wantRefresh   bool
		wantConfirm   bool
		wantWarning   bool
	}{
		{
			name:          "step 3: hash matches",
			probe:         func(context.Context) (string, error) { return cached, nil },
			wantDiscovery: output.CacheStaleServed,
			wantConfirm:   true,
		},
		{
			name:          "step 4: hash differs",
			probe:         func(context.Context) (string, error) { return "topology-xyz", nil },
			wantDiscovery: output.CacheRevalidated,
			wantRefresh:   true,
		},
		{
			name:          "step 5: probe errors",
			probe:         func(context.Context) (string, error) { return "", errors.New("connection refused") },
			wantDiscovery: output.CacheStaleServed,
			wantWarning:   true,
		},
		{
			name: "step 5: probe times out",
			probe: func(ctx context.Context) (string, error) {
				<-ctx.Done()
				return "", ctx.Err()
			},
			budget:        20 * time.Millisecond,
			wantDiscovery: output.CacheStaleServed,
			wantWarning:   true,
		},
		{
			name:          "an empty fingerprint confirms nothing",
			probe:         func(context.Context) (string, error) { return "", nil },
			wantDiscovery: output.CacheRevalidated,
			wantRefresh:   true,
		},
		{
			name:          "a nil probe serves stale",
			probe:         nil,
			wantDiscovery: output.CacheStaleServed,
			wantWarning:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Revalidate(context.Background(), cached, tc.probe, tc.budget)
			if got.Discovery != tc.wantDiscovery {
				t.Errorf("discovery = %q, want %q", got.Discovery, tc.wantDiscovery)
			}
			if got.Refresh != tc.wantRefresh {
				t.Errorf("refresh = %v, want %v", got.Refresh, tc.wantRefresh)
			}
			if got.Confirm != tc.wantConfirm {
				t.Errorf("confirm = %v, want %v", got.Confirm, tc.wantConfirm)
			}
			if (got.Warning != nil) != tc.wantWarning {
				t.Errorf("warning = %+v, wantWarning = %v", got.Warning, tc.wantWarning)
			}
			if got.Warning != nil {
				if got.Warning.Code != WarnRevalidationTimedOut {
					t.Errorf("warning code = %q", got.Warning.Code)
				}
				if got.Warning.Hint != "pay discover --refresh" {
					t.Errorf("warning hint = %q", got.Warning.Hint)
				}
			}
		})
	}
}

// TestRevalidateHonoursTheBudget asserts §8.5's "at most 250 ms more than a
// fresh one": a slow probe must not hold the output longer than the budget.
func TestRevalidateHonoursTheBudget(t *testing.T) {
	slow := func(ctx context.Context) (string, error) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(5 * time.Second):
			return "never", nil
		}
	}
	done := make(chan Revalidation, 1)
	go func() { done <- Revalidate(context.Background(), "x", slow, 50*time.Millisecond) }()

	select {
	case got := <-done:
		if got.Discovery != output.CacheStaleServed || got.Warning == nil {
			t.Fatalf("got %+v, want a warned stale serve", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Revalidate blocked well past its 50ms budget")
	}
}

func TestRevalidateDefaultBudget(t *testing.T) {
	if RevalidationBudget != 250*time.Millisecond {
		t.Fatalf("RevalidationBudget = %s, want 250ms", RevalidationBudget)
	}
	// A zero budget falls back to the default rather than timing out instantly.
	got := Revalidate(context.Background(), "x", func(context.Context) (string, error) { return "x", nil }, 0)
	if !got.Confirm {
		t.Fatalf("got %+v, want a confirmed stale serve", got)
	}
}

func TestRevalidateRespectsParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := Revalidate(ctx, "x", func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}, time.Second)
	if got.Discovery != output.CacheStaleServed || got.Warning == nil {
		t.Fatalf("got %+v, want a warned stale serve", got)
	}
}

func TestDiscoveryRanWarning(t *testing.T) {
	w := DiscoveryRanWarning(947 * time.Millisecond)
	if w.Code != WarnDiscoveryRan {
		t.Fatalf("code = %q", w.Code)
	}
	if w.Hint != "pay discover pre-warms this" {
		t.Fatalf("hint = %q", w.Hint)
	}
	if want := "947 ms"; !strings.Contains(w.Message, want) {
		t.Fatalf("message %q does not mention %q", w.Message, want)
	}
}

func TestSessionRevalidateReadAppliesTheLadder(t *testing.T) {
	s, sc := newTestStore(t)
	gen := NewGeneration(testNow)
	writeFixture(t, s, sc, gen, "pages")

	// Step 3: the probe confirms the cached topology, so confirmed_at moves.
	sess := NewSession(s, sc, SessionOptions{})
	m, ok := sess.LoadManifest()
	if !ok {
		t.Fatal("manifest miss")
	}
	later := testNow.Add(20 * time.Minute)
	r := sess.RevalidateRead(context.Background(), func(context.Context) (string, error) {
		return m.Fingerprint.TopologySHA256, nil
	}, later)
	if !r.Confirm || r.Discovery != output.CacheStaleServed {
		t.Fatalf("revalidation = %+v", r)
	}
	if sess.DiscoveryMode() != output.CacheStaleServed {
		t.Fatalf("discovery mode = %q", sess.DiscoveryMode())
	}
	reread, _, _ := s.ReadManifest(sc)
	if !reread.Meta.ConfirmedAt.Equal(later) {
		t.Fatalf("confirmed_at = %s, want %s", reread.Meta.ConfirmedAt, later)
	}

	// Step 4: a changed topology forces a re-discovery and does not touch.
	sess2 := NewSession(s, sc, SessionOptions{})
	sess2.LoadManifest()
	r2 := sess2.RevalidateRead(context.Background(), func(context.Context) (string, error) {
		return "a-different-topology", nil
	}, testNow.Add(time.Hour))
	if !r2.Refresh || r2.Discovery != output.CacheRevalidated {
		t.Fatalf("revalidation = %+v", r2)
	}
	reread2, _, _ := s.ReadManifest(sc)
	if !reread2.Meta.ConfirmedAt.Equal(later) {
		t.Fatalf("a failed revalidation bumped confirmed_at to %s", reread2.Meta.ConfirmedAt)
	}

	// Step 5: a timeout serves stale and warns.
	sess3 := NewSession(s, sc, SessionOptions{})
	sess3.LoadManifest()
	r3 := sess3.RevalidateRead(context.Background(), func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}, testNow)
	if r3.Discovery != output.CacheStaleServed || r3.Warning == nil {
		t.Fatalf("revalidation = %+v", r3)
	}
	warns := sess3.Warnings()
	if len(warns) != 1 || warns[0].Code != WarnRevalidationTimedOut {
		t.Fatalf("warnings = %+v", warns)
	}
}

// TestSessionRevalidateWriteNeverServesStale is §8.5's "serve-stale is
// read-path only".
func TestSessionRevalidateWriteNeverServesStale(t *testing.T) {
	s, sc := newTestStore(t)
	gen := NewGeneration(testNow)
	writeFixture(t, s, sc, gen, "pages")

	sess := NewSession(s, sc, SessionOptions{})
	sess.LoadManifest()
	// A probe that cannot answer forces a re-discovery instead of a stale serve.
	r := sess.RevalidateWrite(context.Background(), func(context.Context) (string, error) {
		return "", errors.New("connection refused")
	}, testNow)
	if !r.Refresh || r.Discovery != output.CacheRevalidated {
		t.Fatalf("a failed write-path probe = %+v, want a forced refresh", r)
	}

	// A confirming probe still serves the cached schema.
	sess2 := NewSession(s, sc, SessionOptions{})
	m, _ := sess2.LoadManifest()
	r2 := sess2.RevalidateWrite(context.Background(), func(context.Context) (string, error) {
		return m.Fingerprint.TopologySHA256, nil
	}, testNow.Add(time.Minute))
	if !r2.Confirm || r2.Refresh {
		t.Fatalf("a confirming write-path probe = %+v", r2)
	}
}

func TestRevalidationUnderNoCacheDoesNotTouchDisk(t *testing.T) {
	s, sc := newTestStore(t)
	gen := NewGeneration(testNow)
	writeFixture(t, s, sc, gen, "pages")
	before, _, _ := s.ReadManifest(sc)

	sess := NewSession(s, sc, SessionOptions{NoCache: true})
	sess.SetManifest(before)
	sess.RevalidateRead(context.Background(), func(context.Context) (string, error) {
		return before.Fingerprint.TopologySHA256, nil
	}, testNow.Add(time.Hour))

	after, _, _ := s.ReadManifest(sc)
	if !after.Meta.ConfirmedAt.Equal(before.Meta.ConfirmedAt) {
		t.Fatal("--no-cache wrote to disk")
	}
	if sess.DiscoveryMode() != output.CacheDisabled {
		t.Fatalf("discovery mode = %q, want disabled", sess.DiscoveryMode())
	}
}
