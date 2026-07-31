package stats

import (
	"sort"
	"strings"
)

// Key is a grouping key, the Go equivalent of the arrays both scripts pass to
// jq's group_by, for example [.link_profile, .label, .scenario].
//
// Its elements may be strings, numbers or nil. Nothing else appears in a
// benchmark grouping key.
type Key []any

// CompareKeys orders two keys the way jq's group_by and sort_by order them:
// element by element, with jq's own type order (null before every number,
// numbers before strings). Reproducing that ordering is what makes the rows of
// a Go-generated result come out in the same sequence as the rows of the stored
// ones, so the two can be diffed as text and not only as sets.
func CompareKeys(a, b Key) int {
	for i := range a {
		if i >= len(b) {
			return 1
		}
		if c := compareValues(a[i], b[i]); c != 0 {
			return c
		}
	}
	if len(a) < len(b) {
		return -1
	}
	return 0
}

// rank is jq's type order, reduced to the types a benchmark key can hold.
func rank(v any) int {
	switch v.(type) {
	case nil:
		return 0
	case int, int64, float64:
		return 1
	case string:
		return 2
	default:
		return 3
	}
}

func compareValues(a, b any) int {
	if ra, rb := rank(a), rank(b); ra != rb {
		if ra < rb {
			return -1
		}
		return 1
	}
	switch left := a.(type) {
	case nil:
		return 0
	case string:
		return strings.Compare(left, b.(string))
	default:
		x, y := number(a), number(b)
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		default:
			return 0
		}
	}
}

func number(v any) float64 {
	switch n := v.(type) {
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case float64:
		return n
	default:
		return 0
	}
}

// GroupBy is jq's group_by: it partitions the items by key and returns the
// groups ordered by that key.
//
// Like jq's, the grouping is order-preserving inside a group, so the repeats of
// one coordinate stay in the order they were measured in.
func GroupBy[T any](items []T, key func(T) Key) [][]T {
	type group struct {
		key   Key
		items []T
	}
	var groups []*group
	index := map[string]*group{}
	for _, item := range items {
		k := key(item)
		id := keyID(k)
		g, ok := index[id]
		if !ok {
			g = &group{key: k}
			index[id] = g
			groups = append(groups, g)
		}
		g.items = append(g.items, item)
	}
	sort.SliceStable(groups, func(i, j int) bool {
		return CompareKeys(groups[i].key, groups[j].key) < 0
	})
	out := make([][]T, len(groups))
	for i, g := range groups {
		out[i] = g.items
	}
	return out
}

// SortByKey orders a slice by a key function, stably.
func SortByKey[T any](items []T, key func(T) Key) {
	sort.SliceStable(items, func(i, j int) bool {
		return CompareKeys(key(items[i]), key(items[j])) < 0
	})
}

// keyID is an identity for map lookup only. It never decides ordering, which is
// what CompareKeys is for, so it only has to separate keys that differ.
func keyID(k Key) string {
	var b strings.Builder
	for _, v := range k {
		switch value := v.(type) {
		case nil:
			b.WriteString("\x00null")
		case string:
			b.WriteString("\x00s")
			b.WriteString(value)
		default:
			b.WriteString("\x00n")
			b.WriteString(formatNumber(number(value)))
		}
	}
	return b.String()
}
