package config

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// documentEditor edits a parsed configuration document in place. It only ever
// rewrites the keys it is asked to change, so comments, key order, formatting
// and every untouched scalar survive a save. Both supported encodings
// implement it, which is what lets the patch mechanism stay format-agnostic.
type documentEditor interface {
	set(path []string, value any)
	remove(path ...string)
	encode() ([]byte, error)
}

// newDocumentEditor parses raw according to format and returns an editor for it.
func newDocumentEditor(format Format, raw []byte) (documentEditor, error) {
	switch format {
	case YAML:
		return newYAMLDocument(raw)
	case TOML:
		return newTOMLDocument(raw), nil
	}
	return nil, errors.New("unsupported configuration format")
}

// yamlDocument edits a YAML AST. The document node is kept so encoding emits
// exactly the same shape the parser read.
type yamlDocument struct {
	doc  *yaml.Node
	root *yaml.Node
}

func newYAMLDocument(raw []byte) (*yamlDocument, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("invalid configuration YAML: %w", err)
	}
	if len(doc.Content) == 0 {
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, errors.New("configuration root must be a mapping")
	}
	return &yamlDocument{doc: &doc, root: root}, nil
}

func (d *yamlDocument) set(path []string, value any) {
	node := d.root
	for _, key := range path[:len(path)-1] {
		node = ensureMapping(node, key)
	}
	setMappingKey(node, path[len(path)-1], yamlValueNode(value))
}

func (d *yamlDocument) remove(path ...string) {
	node := d.root
	for _, key := range path[:len(path)-1] {
		child := findMappingKey(node, key)
		if child == nil || child.Kind != yaml.MappingNode {
			return
		}
		node = child
	}
	removeMappingKey(node, path[len(path)-1])
}

func (d *yamlDocument) encode() ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(d.doc); err != nil {
		return nil, fmt.Errorf("cannot encode configuration: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("cannot encode configuration: %w", err)
	}
	return buf.Bytes(), nil
}

// yamlValueNode converts a patch value into the YAML node that represents it.
func yamlValueNode(value any) *yaml.Node {
	switch v := value.(type) {
	case string:
		return scalarNode("!!str", v)
	case bool:
		return scalarNode("!!bool", strconv.FormatBool(v))
	case int:
		return scalarNode("!!int", strconv.Itoa(v))
	case []string:
		return stringListNode(v)
	default:
		return scalarNode("!!str", fmt.Sprint(v))
	}
}

// removeMappingKey deletes key from a mapping if present, leaving every other
// key and comment in place.
func removeMappingKey(mapping *yaml.Node, key string) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
			return
		}
	}
}

// tomlDocument edits a TOML document line by line. TOML has no comment-
// preserving AST in the vendored parser, so the editor tracks table headers and
// rewrites only the assignment lines it is asked to change. Everything else,
// including comments and blank lines, is copied through verbatim.
type tomlDocument struct {
	lines []string
}

func newTOMLDocument(raw []byte) *tomlDocument {
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	return &tomlDocument{lines: strings.Split(text, "\n")}
}

func (d *tomlDocument) set(path []string, value any) {
	table := path[:len(path)-1]
	encoded := tomlValue(value)
	current := []string{}
	for i, line := range d.lines {
		if header, ok := tomlTableHeader(line); ok {
			current = header
			continue
		}
		if key, ok := tomlKey(line); ok && pathMatches(current, key, path) {
			// Replace only the value, preserving the key text, indentation
			// and any trailing comment exactly as written.
			eq := strings.Index(line, "=")
			d.lines[i] = line[:eq+1] + " " + encoded + tomlInlineComment(line)
			return
		}
	}
	d.insert(table, path[len(table):], encoded)
}

func (d *tomlDocument) remove(path ...string) {
	current := []string{}
	for i := 0; i < len(d.lines); i++ {
		if header, ok := tomlTableHeader(d.lines[i]); ok {
			current = header
			continue
		}
		if key, ok := tomlKey(d.lines[i]); ok && pathMatches(current, key, path) {
			d.lines = append(d.lines[:i], d.lines[i+1:]...)
			i--
		}
	}
}

func (d *tomlDocument) encode() ([]byte, error) {
	return []byte(strings.Join(d.lines, "\n")), nil
}

// insert adds an assignment to a table, creating the table header when needed.
// Top-level keys are placed before the first table so they stay top-level.
func (d *tomlDocument) insert(table, key []string, encoded string) {
	assignment := strings.Join(key, ".") + " = " + encoded
	if len(table) == 0 {
		index := len(d.lines)
		for i, line := range d.lines {
			if _, ok := tomlTableHeader(line); ok {
				index = i
				break
			}
		}
		d.lines = insertLine(d.lines, index, assignment)
		return
	}
	header := "[" + strings.Join(table, ".") + "]"
	start := -1
	for i, line := range d.lines {
		if h, ok := tomlTableHeader(line); ok && equalPath(h, table) {
			start = i
			break
		}
	}
	if start < 0 {
		if len(d.lines) > 0 && d.lines[len(d.lines)-1] != "" {
			d.lines = append(d.lines, "")
		}
		d.lines = append(d.lines, header, assignment)
		return
	}
	end := len(d.lines)
	for i := start + 1; i < len(d.lines); i++ {
		if _, ok := tomlTableHeader(d.lines[i]); ok {
			end = i
			break
		}
	}
	d.lines = insertLine(d.lines, end, assignment)
}

func insertLine(lines []string, index int, line string) []string {
	lines = append(lines, "")
	copy(lines[index+1:], lines[index:])
	lines[index] = line
	return lines
}

// tomlTableHeader parses a [table] or [a.b] header into its path segments.
func tomlTableHeader(line string) ([]string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "[[") {
		return nil, false
	}
	end := strings.Index(trimmed, "]")
	if end < 0 {
		return nil, false
	}
	parts := strings.Split(trimmed[1:end], ".")
	for i := range parts {
		parts[i] = strings.Trim(strings.TrimSpace(parts[i]), `"'`)
	}
	return parts, true
}

// tomlKey returns the dotted key path of an assignment line, or false for
// comments, blank lines and table headers.
func tomlKey(line string) ([]string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "[") {
		return nil, false
	}
	eq := strings.Index(trimmed, "=")
	if eq < 0 {
		return nil, false
	}
	parts := strings.Split(trimmed[:eq], ".")
	for i := range parts {
		parts[i] = strings.Trim(strings.TrimSpace(parts[i]), `"'`)
	}
	return parts, true
}

// tomlInlineComment returns the trailing comment of a line, including a leading
// space, or the empty string. A # inside a quoted string is not a comment.
func tomlInlineComment(line string) string {
	inSingle, inDouble := false, false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '#':
			if !inSingle && !inDouble {
				return " " + strings.TrimSpace(line[i:])
			}
		}
	}
	return ""
}

// tomlValue encodes a patch value as a TOML literal.
func tomlValue(value any) string {
	switch v := value.(type) {
	case string:
		return strconv.Quote(v)
	case bool:
		return strconv.FormatBool(v)
	case int:
		return strconv.Itoa(v)
	case []string:
		parts := make([]string, len(v))
		for i, entry := range v {
			parts[i] = strconv.Quote(entry)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	default:
		return strconv.Quote(fmt.Sprint(v))
	}
}

func equalPath(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// pathMatches reports whether the table context plus a (possibly dotted) key
// names exactly the wanted path.
func pathMatches(current, key, want []string) bool {
	if len(current)+len(key) != len(want) {
		return false
	}
	for i, part := range current {
		if part != want[i] {
			return false
		}
	}
	for i, part := range key {
		if part != want[len(current)+i] {
			return false
		}
	}
	return true
}
