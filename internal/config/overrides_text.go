package config

import (
	"bytes"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	crlf        = "\r\n"
	childIndent = 2
)

// matchLineEndings gives encoded the line endings of data: a file written
// with CRLF stays CRLF.
func matchLineEndings(data, encoded []byte) []byte {
	if usesCRLF(data) {
		return bytes.ReplaceAll(encoded, []byte("\n"), []byte(crlf))
	}
	return encoded
}

func usesCRLF(data []byte) bool {
	crlfs := bytes.Count(data, []byte(crlf))
	return crlfs > 0 && crlfs*2 > bytes.Count(data, []byte("\n"))
}

// sameConfig reports whether both texts decode to the same data, aliases and
// merge keys resolved.
func sameConfig(a, b []byte) bool {
	var x, y any
	if yaml.Unmarshal(a, &x) != nil || yaml.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// textEdits collects the changes to a file's lines. Every line keeps its own
// terminator; replaced holds the new text of a line and after the lines to
// insert below one.
type textEdits struct {
	replaced map[int]string
	after    map[int][]string
	eol      string
	lines    []string
	step     int
}

func (t *textEdits) line(i int) string {
	if r, ok := t.replaced[i]; ok {
		return r
	}
	return t.lines[i]
}

func (t *textEdits) replaceToken(line, col, length int, with string) bool {
	if line < 1 || line > len(t.lines) {
		return false
	}
	text := t.line(line - 1)
	start := byteOffset(text, col)
	if start < 0 || start+length > len(text) {
		return false
	}
	head := text[:start]
	if with == "" {
		head = strings.TrimRight(head, " ")
	}
	t.replaced[line-1] = head + with + text[start+length:]
	return true
}

func (t *textEdits) insertAfter(line, indent int, text ...string) {
	for _, s := range text {
		t.after[line] = append(t.after[line], strings.Repeat(" ", indent)+s)
	}
}

func (t *textEdits) render() []byte {
	var b strings.Builder
	for i := range t.lines {
		line := t.line(i)
		b.WriteString(line)
		ins := t.after[i]
		if len(ins) == 0 {
			continue
		}
		if !strings.HasSuffix(line, "\n") {
			b.WriteString(t.eol)
		}
		for _, s := range ins {
			b.WriteString(s)
			b.WriteString(t.eol)
		}
	}
	return []byte(b.String())
}

// byteOffset converts a 1-based character column to a byte offset, or -1.
func byteOffset(line string, col int) int {
	offset := 0
	for range col - 1 {
		if offset >= len(line) {
			return -1
		}
		_, w := utf8.DecodeRuneInString(line[offset:])
		offset += w
	}
	return offset
}

// spliceOverrides enables the changed entries by editing only the lines they
// need: a new entry is appended to providers, `enabled: true` is added to or
// set on an existing entry. It reports false for any layout it does not
// handle (flow style, an empty value, anything unexpected), and the caller
// then re-encodes the document.
func spliceOverrides(data []byte, entries []Override, changed []string) ([]byte, bool) {
	var doc yaml.Node
	if yaml.Unmarshal(data, &doc) != nil || doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil, false
	}
	top := doc.Content[0]
	if top.Kind != yaml.MappingNode || top.Style&yaml.FlowStyle != 0 {
		return nil, false
	}

	edits := &textEdits{
		lines:    strings.SplitAfter(string(data), "\n"),
		replaced: map[int]string{},
		after:    map[int][]string{},
		eol:      "\n",
		step:     childIndent,
	}
	if usesCRLF(data) {
		edits.eol = crlf
	}
	if last := len(edits.lines) - 1; edits.lines[last] == "" {
		edits.lines = edits.lines[:last]
	}

	wanted := make([]Override, 0, len(changed))
	for _, e := range entries {
		if slices.Contains(changed, e.Name) {
			wanted = append(wanted, e)
		}
	}
	var ok bool
	if at := pairIndex(top, "providers"); at < 0 {
		ok = appendProviders(edits, wanted)
	} else {
		ok = editProviders(edits, top.Content[at], top.Content[at+1], wanted)
	}
	if !ok {
		return nil, false
	}
	return edits.render(), true
}

// pairIndex is the position of key in a mapping's content, or -1.
func pairIndex(m *yaml.Node, key string) int {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return i
		}
	}
	return -1
}

// newEntryLines renders a provider entry added to providers at indent.
func newEntryLines(e Override, indent, step int) ([]string, bool) {
	name, ok := renderScalar(e.Name)
	if !ok {
		return nil, false
	}
	pad := strings.Repeat(" ", indent)
	lines := []string{pad + name + ":"}
	child := pad + strings.Repeat(" ", step)
	if e.Define == nil {
		return append(lines, child+"enabled: true"), true
	}
	def := *e.Define
	def.Enabled = true
	var node yaml.Node
	if node.Encode(def) != nil {
		return nil, false
	}
	body, err := encodeRoot(&node)
	if err != nil {
		return nil, false
	}
	for line := range strings.SplitSeq(strings.TrimRight(string(body), "\n"), "\n") {
		lines = append(lines, child+line)
	}
	return lines, true
}

func renderScalar(s string) (string, bool) {
	var node yaml.Node
	if node.Encode(s) != nil {
		return "", false
	}
	out, err := encodeRoot(&node)
	if err != nil {
		return "", false
	}
	text := strings.TrimRight(string(out), "\n")
	return text, text != "" && !strings.Contains(text, "\n")
}

func appendProviders(edits *textEdits, wanted []Override) bool {
	if len(edits.lines) == 0 {
		return false
	}
	last := len(edits.lines) - 1
	edits.insertAfter(last, 0, "providers:")
	for _, e := range wanted {
		lines, ok := newEntryLines(e, childIndent, childIndent)
		if !ok {
			return false
		}
		edits.insertAfter(last, 0, lines...)
	}
	return true
}

func editProviders(edits *textEdits, key, providers *yaml.Node, wanted []Override) bool {
	if providers.Kind != yaml.MappingNode || providers.Style&yaml.FlowStyle != 0 || len(providers.Content) == 0 {
		return false
	}
	indent, ok := blockIndent(edits, providers.Content[0], key.Line)
	if !ok {
		return false
	}

	edits.step = indentStep(edits, providers, indent)
	var added []string
	for _, e := range wanted {
		at := pairIndex(providers, e.Name)
		if at < 0 {
			lines, ok := newEntryLines(e, indent, edits.step)
			if !ok {
				return false
			}
			added = append(added, lines...)
			continue
		}
		if !enableEntry(edits, providers.Content[at], providers.Content[at+1]) {
			return false
		}
	}
	end := blockEnd(edits.lines, key.Line-1, indent)
	edits.insertAfter(end, 0, added...)
	return true
}

// indentStep is how far the file indents the body of an entry, so added lines
// follow its style.
func indentStep(edits *textEdits, providers *yaml.Node, indent int) int {
	for i := 1; i < len(providers.Content); i += 2 {
		v := providers.Content[i]
		if v.Kind != yaml.MappingNode || len(v.Content) == 0 {
			continue
		}
		if inner, ok := blockIndent(edits, v.Content[0], providers.Content[i-1].Line); ok && inner > indent {
			return inner - indent
		}
	}
	return childIndent
}

// enableEntry edits one existing entry so it decodes with enabled: true. An
// alias becomes a merge key, which keeps the entry tied to its anchor.
func enableEntry(edits *textEdits, key, value *yaml.Node) bool {
	if value.Kind == yaml.MappingNode {
		return enableMapping(edits, key, value)
	}
	if value.Kind != yaml.AliasNode || value.Line != key.Line || value.Alias == nil || value.Alias.Kind != yaml.MappingNode {
		return false
	}
	if !edits.replaceToken(value.Line, value.Column, 1+len(value.Value), "") {
		return false
	}
	edits.insertAfter(value.Line-1, key.Column-1+edits.step, "<<: *"+value.Value, "enabled: true")
	return true
}

func enableMapping(edits *textEdits, key, value *yaml.Node) bool {
	if value.Style&yaml.FlowStyle != 0 || len(value.Content) == 0 || value.Content[0].Line <= key.Line {
		return false
	}
	at := pairIndex(value, "enabled")
	if at < 0 {
		indent, ok := blockIndent(edits, value.Content[0], key.Line)
		if !ok {
			return false
		}
		edits.insertAfter(blockEnd(edits.lines, key.Line-1, indent), indent, "enabled: true")
		return true
	}
	enabled := value.Content[at+1]
	if enabled.Kind != yaml.ScalarNode || enabled.Style != 0 || enabled.Anchor != "" || !tokenIs(edits, enabled) {
		return false
	}
	return edits.replaceToken(enabled.Line, enabled.Column, len(enabled.Value), "true")
}

// tokenIs checks that the text at the node's position is its value, so a
// column the parser counted differently never edits the wrong bytes.
func tokenIs(edits *textEdits, n *yaml.Node) bool {
	if n.Line < 1 || n.Line > len(edits.lines) {
		return false
	}
	text := edits.line(n.Line - 1)
	start := byteOffset(text, n.Column)
	return start >= 0 && strings.HasPrefix(text[start:], n.Value)
}

// blockIndent is the indentation of a block mapping whose first key starts
// its line below ownerLine, or false when it does not.
func blockIndent(edits *textEdits, first *yaml.Node, ownerLine int) (int, bool) {
	if first.Line <= ownerLine || first.Line > len(edits.lines) {
		return 0, false
	}
	line := edits.lines[first.Line-1]
	indent := first.Column - 1
	if indent <= 0 || indent > len(line) || strings.TrimLeft(line[:indent], " ") != "" {
		return 0, false
	}
	return indent, true
}

// blockEnd is the index of the last line that belongs to the block mapping
// opened by the key on line owner and indented by indent: the last content
// line, or comment line indented as deep, before one indented less.
func blockEnd(lines []string, owner, indent int) int {
	last := owner
	for i := owner + 1; i < len(lines); i++ {
		text := strings.TrimRight(lines[i], "\r\n")
		trimmed := strings.TrimLeft(text, " ")
		if strings.TrimSpace(trimmed) == "" {
			continue
		}
		depth := len(text) - len(trimmed)
		if strings.HasPrefix(trimmed, "#") {
			if depth >= indent {
				last = i
			}
			continue
		}
		if depth < indent {
			break
		}
		last = i
	}
	return last
}
