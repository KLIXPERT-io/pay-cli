package config

import "testing"

func TestInterpolate(t *testing.T) {
	env := Env{"TOKEN": "s3cr3t", "EMPTY": ""}
	tests := []struct {
		name        string
		in          string
		want        string
		wantMissing []string
	}{
		{name: "no reference", in: "plain value", want: "plain value"},
		{name: "whole value", in: "${TOKEN}", want: "s3cr3t"},
		{name: "embedded", in: "Bearer ${TOKEN}!", want: "Bearer s3cr3t!"},
		{name: "twice", in: "${TOKEN}/${TOKEN}", want: "s3cr3t/s3cr3t"},
		{name: "unset", in: "a${NOPE}b", want: "ab", wantMissing: []string{"NOPE"}},
		{name: "set but empty counts as unset", in: "${EMPTY}", want: "", wantMissing: []string{"EMPTY"}},
		{name: "bare dollar is literal", in: "$TOKEN", want: "$TOKEN"},
		{name: "escaped", in: "$${TOKEN}", want: "${TOKEN}"},
		{name: "unterminated", in: "${TOKEN", want: "${TOKEN"},
		{name: "dollar at end", in: "abc$", want: "abc$"},
		{name: "two missing are deduped and sorted", in: "${B}${A}${B}", want: "", wantMissing: []string{"A", "B"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, missing := Interpolate(tc.in, env)
			if got != tc.want {
				t.Errorf("Interpolate(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if len(missing) != len(tc.wantMissing) {
				t.Fatalf("missing = %v, want %v", missing, tc.wantMissing)
			}
			for i := range missing {
				if missing[i] != tc.wantMissing[i] {
					t.Fatalf("missing = %v, want %v", missing, tc.wantMissing)
				}
			}
		})
	}
}

func TestInterpolateMap(t *testing.T) {
	out, missing := InterpolateMap(map[string]string{
		"X-A":    "${TOKEN}",
		"X-B":    "static",
		"${KEY}": "value",
	}, Env{"TOKEN": "t", "KEY": "interpolated"})
	if out["X-A"] != "t" || out["X-B"] != "static" {
		t.Errorf("out = %v", out)
	}
	if _, ok := out["${KEY}"]; !ok {
		t.Error("header names must never be interpolated")
	}
	if len(missing) != 0 {
		t.Errorf("missing = %v", missing)
	}
	if out, missing := InterpolateMap(nil, Env{}); out != nil || missing != nil {
		t.Error("an empty map must round-trip to nil")
	}
}
