package cache

import (
	"testing"
	"time"
)

func TestNewGenerationShape(t *testing.T) {
	g := NewGeneration(testNow)
	if len(g) != GenerationLen {
		t.Fatalf("generation %q is %d chars, want %d", g, len(g), GenerationLen)
	}
	if !ValidGeneration(g) {
		t.Fatalf("NewGeneration produced an invalid value: %q", g)
	}
	got, err := GenerationTime(g)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(testNow) {
		t.Fatalf("GenerationTime = %s, want %s", got, testNow)
	}
}

func TestGenerationsAreUnique(t *testing.T) {
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		g := NewGeneration(testNow) // same millisecond every time
		if _, dup := seen[g]; dup {
			t.Fatalf("duplicate generation %q after %d mints", g, i)
		}
		seen[g] = struct{}{}
	}
}

func TestGenerationsSortByMintTime(t *testing.T) {
	a := NewGeneration(testNow)
	b := NewGeneration(testNow.Add(time.Second))
	if !(a < b) {
		t.Fatalf("%q should sort before %q", a, b)
	}
}

func TestValidGeneration(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{NewGeneration(testNow), true},
		{"01K5Q7TZ4V3B8CJK2M9N0P1Q2R", true},
		{"", false},
		{"01K5Q7TZ4V3B8CJK2M9N0P1Q2", false},
		{"01K5Q7TZ4V3B8CJK2M9N0P1Q2RR", false},
		{"01k5q7tz4v3b8cjk2m9n0p1q2r", false}, // lowercase is not emitted
		{"I1K5Q7TZ4V3B8CJK2M9N0P1Q2R", false}, // I is not in Crockford
		{"ZZZZZZZZZZZZZZZZZZZZZZZZZZ", false}, // overflows 128 bits
	}
	for _, tc := range tests {
		if got := ValidGeneration(tc.in); got != tc.want {
			t.Errorf("ValidGeneration(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	if _, err := GenerationTime("nope"); err == nil {
		t.Fatal("GenerationTime accepted a malformed value")
	}
}

func TestGenerationZeroTime(t *testing.T) {
	g := NewGeneration(time.Time{})
	if !ValidGeneration(g) {
		t.Fatalf("a zero time produced an invalid generation: %q", g)
	}
}
