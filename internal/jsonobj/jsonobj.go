// Package jsonobj edits a JSON object and keeps what it does not touch: member order, raw values,
// two-space layout (the one pi, Codex and piggery write).
package jsonobj

import (
	"bytes"
	"encoding/json"
	"errors"
)

// Object is a JSON object whose members keep their order and raw values, so a settings file piggery
// edits keeps everything it does not touch. Written back with two-space indentation.
type Object []Member

type Member struct {
	Key string
	Val json.RawMessage
}

func Parse(b []byte) (Object, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return Object{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	var o Object
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := t.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		o = append(o, Member{key, v})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return o, nil
}

func (o Object) Get(key string) (json.RawMessage, bool) {
	for _, m := range o {
		if m.Key == key {
			return m.Val, true
		}
	}
	return nil, false
}

// Set replaces key's value in place, or appends it.
func (o Object) Set(key string, v json.RawMessage) Object {
	for i, m := range o {
		if m.Key == key {
			o[i].Val = v
			return o
		}
	}
	return append(o, Member{key, v})
}

func (o Object) Del(key string) Object {
	var out Object
	for _, m := range o {
		if m.Key != key {
			out = append(out, m)
		}
	}
	return out
}

// Bytes renders o at indent depth depth (0 = a file: ends with a newline).
func (o Object) Bytes(depth int) []byte {
	pad := bytes.Repeat([]byte("  "), depth)
	if len(o) == 0 {
		return []byte("{}")
	}
	var b bytes.Buffer
	b.WriteString("{\n")
	for i, m := range o {
		b.Write(pad)
		b.WriteString("  ")
		b.Write(String(m.Key))
		b.WriteString(": ")
		var v bytes.Buffer
		if json.Indent(&v, m.Val, string(pad)+"  ", "  ") != nil {
			v.Reset()
			v.Write(m.Val)
		}
		b.Write(v.Bytes())
		if i < len(o)-1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	b.Write(pad)
	b.WriteByte('}')
	if depth == 0 {
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// String encodes s without HTML escaping (as JSON.stringify and serde write it).
func String(s string) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return bytes.TrimRight(b.Bytes(), "\n")
}
