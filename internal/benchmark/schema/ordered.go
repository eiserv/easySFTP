package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Ordered is a JSON object whose keys keep the order they were written in.
//
// It exists because a Go map re-encodes its keys sorted, and the documents this
// package describes are committed to the repository. index.json lists a
// median per scenario in the order the run measured them, and rewriting that
// object as "large, mixed, small" on the next store would be a change to a
// stored document made by nobody, which is exactly what issue #190 asks the
// migration not to do.
//
// Keys are unique: setting one twice replaces its value and keeps its place.
type Ordered[V any] struct {
	keys   []string
	values map[string]V
}

// NewOrdered builds an empty object.
func NewOrdered[V any]() *Ordered[V] {
	return &Ordered[V]{values: map[string]V{}}
}

// Set appends a key, or replaces the value of one that is already there.
func (o *Ordered[V]) Set(key string, value V) {
	if o.values == nil {
		o.values = map[string]V{}
	}
	if _, seen := o.values[key]; !seen {
		o.keys = append(o.keys, key)
	}
	o.values[key] = value
}

// Get reports the value stored under a key.
func (o Ordered[V]) Get(key string) (V, bool) {
	value, ok := o.values[key]
	return value, ok
}

// Keys is the insertion order.
func (o Ordered[V]) Keys() []string { return o.keys }

// Len is how many keys the object has.
func (o Ordered[V]) Len() int { return len(o.keys) }

// MarshalJSON writes the keys in the order they were set. An object that was
// never written to is "{}", not "null": an empty median list means "this kind
// of result does not report one", and a null there would read as missing data.
//
// Nothing here is HTML-escaped, for the same reason the documents around it are
// not: jq leaves the three HTML-significant characters alone, and the index
// layout strings are full of them ("manual-<stamp>-<label>").
func (o Ordered[V]) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, key := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		name, err := encode(key)
		if err != nil {
			return nil, err
		}
		buf.Write(name)
		buf.WriteByte(':')
		value, err := encode(o.values[key])
		if err != nil {
			return nil, err
		}
		buf.Write(value)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func encode(value any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	// Encode appends a newline that a JSON value inside an object must not
	// carry.
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// UnmarshalJSON reads an object through the token stream, which is the only way
// to see the order the keys arrived in.
func (o *Ordered[V]) UnmarshalJSON(data []byte) error {
	o.keys = nil
	o.values = map[string]V{}
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return fmt.Errorf("expected a JSON object, got %v", token)
	}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok {
			return fmt.Errorf("expected an object key, got %v", token)
		}
		var value V
		if err := decoder.Decode(&value); err != nil {
			return err
		}
		o.Set(key, value)
	}
	_, err = decoder.Token() // the closing brace
	return err
}
