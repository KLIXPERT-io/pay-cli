package manifest

import "sort"

// Order sorts n items so that every item comes after the items it depends
// on. deps[i] lists the items i depends on. Among items that are ready at the
// same time the lowest index goes first, so an independent manifest keeps
// its load order exactly and the result is deterministic.
//
// When the dependencies contain a cycle, Order returns ok=false and one
// cycle as a list of indices, first element repeated at the end
// ([a, b, a]), for the error message.
func Order(n int, deps map[int][]int) (order []int, cycle []int, ok bool) {
	indeg := make([]int, n)
	dependents := make([][]int, n)
	for i := 0; i < n; i++ {
		seen := map[int]bool{}
		for _, j := range deps[i] {
			if j < 0 || j >= n || seen[j] {
				continue
			}
			seen[j] = true
			indeg[i]++
			dependents[j] = append(dependents[j], i)
		}
	}
	ready := []int{}
	for i := 0; i < n; i++ {
		if indeg[i] == 0 {
			ready = append(ready, i)
		}
	}
	for len(ready) > 0 {
		sort.Ints(ready)
		i := ready[0]
		ready = ready[1:]
		order = append(order, i)
		for _, k := range dependents[i] {
			indeg[k]--
			if indeg[k] == 0 {
				ready = append(ready, k)
			}
		}
	}
	if len(order) == n {
		return order, nil, true
	}
	return nil, findCycle(n, deps, indeg), false
}

// findCycle walks the dependency edges among the items Kahn's algorithm
// could not place until it revisits one: every such item is on, or depends
// on, a cycle, so following dependencies from any of them reaches one.
func findCycle(n int, deps map[int][]int, indeg []int) []int {
	start := -1
	for i := 0; i < n; i++ {
		if indeg[i] > 0 {
			start = i
			break
		}
	}
	if start < 0 {
		return nil
	}
	pos := map[int]int{}
	var path []int
	cur := start
	for {
		if p, seen := pos[cur]; seen {
			cyc := append([]int(nil), path[p:]...)
			return append(cyc, cur)
		}
		pos[cur] = len(path)
		path = append(path, cur)
		next := -1
		ds := append([]int(nil), deps[cur]...)
		sort.Ints(ds)
		for _, j := range ds {
			if j >= 0 && j < n && indeg[j] > 0 {
				next = j
				break
			}
		}
		if next < 0 {
			return path
		}
		cur = next
	}
}
