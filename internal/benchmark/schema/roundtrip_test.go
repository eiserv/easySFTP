package schema_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"testing"
)

// assertRoundTrip re-encodes a decoded document and compares it against the
// original, value by value.
//
// Two normalizations are applied to both sides first, and both are properties
// of JSON rather than concessions to these types:
//
//   - Object key order is not data. Comparing parsed values rather than bytes
//     is what makes that true here.
//   - An absent key and a null one carry the same information. A schema_version
//     1 result simply has no mad_ms, a version 2 one has mad_ms: null where a
//     single repeat left no spread, and both mean "no value". Dropping nulls on
//     both sides is what lets one set of types read both without the round trip
//     failing on the difference.
//
// Everything else is compared exactly, so a value that changed, was dropped or
// changed type still fails.
func assertRoundTrip(t *testing.T, original json.RawMessage, decoded any) {
	t.Helper()

	reencoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("re-encoding: %v", err)
	}

	want := normalize(t, original)
	got := normalize(t, reencoded)
	if reflect.DeepEqual(want, got) {
		return
	}
	for _, d := range diff(nil, want, got) {
		t.Errorf("round trip: %s", d)
	}
}

func normalize(t *testing.T, data []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("parsing: %v", err)
	}
	return dropNulls(v)
}

func dropNulls(v any) any {
	switch value := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		for k, inner := range value {
			if inner == nil {
				continue
			}
			out[k] = dropNulls(inner)
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, inner := range value {
			out[i] = dropNulls(inner)
		}
		return out
	default:
		return v
	}
}

// diff reports where two parsed documents differ, as paths rather than as two
// walls of JSON: a stored result is thousands of lines and the useful part of a
// failure is which key moved.
func diff(path []string, want, got any) []string {
	at := "$"
	if len(path) > 0 {
		at = fmt.Sprintf("$.%s", joinPath(path))
	}

	switch expected := want.(type) {
	case map[string]any:
		actual, ok := got.(map[string]any)
		if !ok {
			return []string{fmt.Sprintf("%s: expected an object, got %T", at, got)}
		}
		var out []string
		for _, k := range sortedKeys(expected) {
			inner, ok := actual[k]
			if !ok {
				out = append(out, fmt.Sprintf("%s: key %q was dropped", at, k))
				continue
			}
			out = append(out, diff(append(path, k), expected[k], inner)...)
		}
		for _, k := range sortedKeys(actual) {
			if _, ok := expected[k]; !ok {
				out = append(out, fmt.Sprintf("%s: key %q was added", at, k))
			}
		}
		return out
	case []any:
		actual, ok := got.([]any)
		if !ok {
			return []string{fmt.Sprintf("%s: expected an array, got %T", at, got)}
		}
		if len(expected) != len(actual) {
			return []string{fmt.Sprintf("%s: length %d, want %d", at, len(actual), len(expected))}
		}
		var out []string
		for i := range expected {
			out = append(out, diff(append(path, fmt.Sprint(i)), expected[i], actual[i])...)
		}
		return out
	default:
		if !reflect.DeepEqual(want, got) {
			return []string{fmt.Sprintf("%s: %v, want %v", at, got, want)}
		}
		return nil
	}
}

func joinPath(path []string) string {
	out := path[0]
	for _, part := range path[1:] {
		out += "." + part
	}
	return out
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
