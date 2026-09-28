package manifest

import (
	"fmt"
	"testing"
)

func TestOrder(t *testing.T) {
	cases := []struct {
		n     int
		deps  map[int][]int
		order string
		cycle string
	}{
		{3, nil, "[0 1 2]", ""},
		{3, map[int][]int{0: {2}}, "[1 2 0]", ""},
		{4, map[int][]int{0: {3}, 1: {3}, 3: {2}}, "[2 3 0 1]", ""},
		{3, map[int][]int{0: {1, 1, 9, -1}}, "[1 0 2]", ""},
		{3, map[int][]int{0: {1}, 1: {0}}, "", "[0 1 0]"},
		{4, map[int][]int{3: {2}, 2: {1}, 1: {2}}, "", "[1 2 1]"},
		{2, map[int][]int{1: {1}}, "", "[1 1]"},
	}
	for _, tc := range cases {
		order, cycle, ok := Order(tc.n, tc.deps)
		if tc.cycle == "" {
			if !ok || fmt.Sprint(order) != tc.order {
				t.Errorf("%v: order %v ok %v, want %s", tc.deps, order, ok, tc.order)
			}
			continue
		}
		if ok || fmt.Sprint(cycle) != tc.cycle {
			t.Errorf("%v: cycle %v ok %v, want %s", tc.deps, cycle, ok, tc.cycle)
		}
	}
}
