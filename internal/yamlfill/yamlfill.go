// Package yamlfill adds the keys a YAML file lacks, with their defaults, and changes nothing else
// in it: every existing byte (values, order, comments, layout) stays. New keys go at the end of the
// mapping they belong to, in its style: lines at its indentation for a block mapping, ", key:
// value" before the closing brace of a flow mapping.
package yamlfill

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Key is one key the file should have.
type Key struct {
	Name string
	// Default is the value written when the key is missing, as YAML text (a scalar or a flow
	// collection, e.g. inherit, "[]", false). "" with no Fields: a required key, never added.
	Default string
	// Fields: the value is a mapping with these keys (filled when present, written as a block
	// when missing).
	Fields []Key
	// Each: the value is a mapping whose every entry is a mapping with these keys (roles).
	Each []Key
	// Items: the value is a sequence whose every item is a mapping with these keys (routing).
	Items []Key
}

type insert struct {
	off   int
	order int
	text  string
	line  bool // whole lines: starts a new line if the text before does not end one
}

type filler struct {
	src     []byte
	lines   []int // byte offset of each line's start (line n at lines[n-1])
	inserts []insert
	added   []string
}

// Fill returns src with the missing keys of schema added, and their paths (a.b, roles.r.spawn).
// A document that is not a mapping is an error.
func Fill(src []byte, schema []Key) ([]byte, []string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, nil, err
	}
	f := &filler{src: src, lines: []int{0}}
	for i, c := range src {
		if c == '\n' {
			f.lines = append(f.lines, i+1)
		}
	}
	if len(doc.Content) == 0 { // an empty file: every key at the top
		root := &yaml.Node{Kind: yaml.MappingNode}
		f.fill(root, schema, "", 0)
		return f.apply(), f.added, nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, nil, errors.New("not a mapping")
	}
	f.fill(root, schema, "", 0)
	return f.apply(), f.added, nil
}

func value(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// fill adds m's missing keys and walks into the ones it has. depth is m's indentation for a
// block mapping written from scratch (an empty file).
func (f *filler) fill(m *yaml.Node, schema []Key, path string, depth int) {
	var missing []Key
	for _, k := range schema {
		v := value(m, k.Name)
		p := strings.TrimPrefix(path+"."+k.Name, ".")
		switch {
		case v == nil && k.Default == "" && k.Fields == nil:
			continue // required: the parser reports it
		case v == nil:
			missing = append(missing, k)
			f.added = append(f.added, p)
		case k.Fields != nil && v.Kind == yaml.MappingNode:
			f.fill(v, k.Fields, p, v.Column-1)
		case k.Each != nil && v.Kind == yaml.MappingNode:
			for i := 0; i+1 < len(v.Content); i += 2 {
				if e := v.Content[i+1]; e.Kind == yaml.MappingNode {
					f.fill(e, k.Each, p+"."+v.Content[i].Value, e.Column-1)
				}
			}
		case k.Items != nil && v.Kind == yaml.SequenceNode:
			for i, it := range v.Content {
				if it.Kind == yaml.MappingNode {
					f.fill(it, k.Items, fmt.Sprintf("%s[%d]", p, i), it.Column-1)
				}
			}
		}
	}
	if len(missing) == 0 {
		return
	}
	if m.Style&yaml.FlowStyle != 0 {
		var b strings.Builder
		for _, k := range missing {
			if len(m.Content) > 0 || b.Len() > 0 {
				b.WriteString(", ")
			}
			b.WriteString(k.Name + ": " + flow(k))
		}
		f.add(f.closing(m), b.String(), false)
		return
	}
	indent := depth
	if len(m.Content) > 0 {
		indent = m.Content[0].Column - 1
	}
	var b strings.Builder
	for _, k := range missing {
		block(&b, k, indent)
	}
	off := len(f.src)
	if len(m.Content) > 0 {
		off = f.lineAfter(f.end(m))
	}
	f.add(off, b.String(), true)
}

// flow is k's value in flow style.
func flow(k Key) string {
	if k.Fields == nil {
		return k.Default
	}
	var parts []string
	for _, c := range k.Fields {
		if c.Default != "" || c.Fields != nil {
			parts = append(parts, c.Name+": "+flow(c))
		}
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// block writes k as block lines at indent.
func block(b *strings.Builder, k Key, indent int) {
	pad := strings.Repeat(" ", indent)
	if k.Fields == nil {
		fmt.Fprintf(b, "%s%s: %s\n", pad, k.Name, k.Default)
		return
	}
	fmt.Fprintf(b, "%s%s:\n", pad, k.Name)
	for _, c := range k.Fields {
		if c.Default != "" || c.Fields != nil {
			block(b, c, indent+2)
		}
	}
}

func (f *filler) add(off int, text string, line bool) {
	f.inserts = append(f.inserts, insert{off, len(f.inserts), text, line})
}

func (f *filler) offset(line, col int) int {
	if line-1 >= len(f.lines) {
		return len(f.src)
	}
	return min(f.lines[line-1]+col-1, len(f.src))
}

// lineAfter is the offset where the line after line starts.
func (f *filler) lineAfter(line int) int {
	if line >= len(f.lines) {
		return len(f.src)
	}
	return f.lines[line]
}

// end is the last line n's text takes: a block scalar's lines, a flow collection's closing
// bracket, else the end of its last child.
func (f *filler) end(n *yaml.Node) int {
	switch {
	case n.Kind == yaml.ScalarNode && (n.Style&(yaml.LiteralStyle|yaml.FoldedStyle)) != 0:
		return n.Line + strings.Count(strings.TrimRight(n.Value, "\n"), "\n") + 1
	case (n.Kind == yaml.MappingNode || n.Kind == yaml.SequenceNode) && n.Style&yaml.FlowStyle != 0:
		return lineOf(f.lines, f.closing(n))
	case len(n.Content) > 0:
		return f.end(n.Content[len(n.Content)-1])
	case n.Kind == yaml.ScalarNode && (n.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle)) != 0:
		return lineOf(f.lines, f.closingQuote(n))
	}
	return n.Line
}

func lineOf(lines []int, off int) int {
	return sort.Search(len(lines), func(i int) bool { return lines[i] > off })
}

// closing is the offset of the bracket that closes flow collection n.
func (f *filler) closing(n *yaml.Node) int {
	depth := 0
	var quote byte
	for i := f.offset(n.Line, n.Column); i < len(f.src); i++ {
		c := f.src[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '#' && i > 0 && (f.src[i-1] == ' ' || f.src[i-1] == '\t'):
			for i < len(f.src) && f.src[i] != '\n' {
				i++
			}
		case c == '{' || c == '[':
			depth++
		case c == '}' || c == ']':
			if depth--; depth == 0 {
				return i
			}
		}
	}
	return len(f.src)
}

// closingQuote is the offset of the quote that ends quoted scalar n.
func (f *filler) closingQuote(n *yaml.Node) int {
	start := f.offset(n.Line, n.Column)
	if start >= len(f.src) {
		return start
	}
	q := f.src[start]
	for i := start + 1; i < len(f.src); i++ {
		if f.src[i] == '\\' && q == '"' {
			i++
		} else if f.src[i] == q {
			if q == '\'' && i+1 < len(f.src) && f.src[i+1] == '\'' {
				i++
				continue
			}
			return i
		}
	}
	return len(f.src)
}

func (f *filler) apply() []byte {
	sort.SliceStable(f.inserts, func(i, j int) bool {
		if f.inserts[i].off != f.inserts[j].off {
			return f.inserts[i].off < f.inserts[j].off
		}
		return f.inserts[i].order < f.inserts[j].order
	})
	var b bytes.Buffer
	prev := 0
	for _, in := range f.inserts {
		b.Write(f.src[prev:in.off])
		if in.line && b.Len() > 0 && b.Bytes()[b.Len()-1] != '\n' {
			b.WriteByte('\n')
		}
		b.WriteString(in.text)
		prev = in.off
	}
	b.Write(f.src[prev:])
	return b.Bytes()
}
