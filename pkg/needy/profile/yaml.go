package profile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v4"
)

// The ordered bridge between YAML text and the JSON that the typed structs
// decode from. It resolves plain scalars as needy's YAML reader (serde_yaml
// 0.9) does, keeps mapping key order (on-conflict rules are ordered), and
// records the mapping keys written with an explicit null, which delete the
// parent's value along `extends`.
//
// NOTICE: sigs.k8s.io/yaml converts through unordered Go maps and resolves
// scalars as YAML 1.1 (`yes`, `on`, octal `0777`), so it can neither keep an
// on-conflict map's order nor read scalars the way needy does; this bridge
// uses go.yaml.in/yaml/v4 nodes.

const maxDepth = 256

const (
	yamlTagStr   = "!!str"
	yamlTagInt   = "!!int"
	yamlTagFloat = "!!float"
	yamlTagBool  = "!!bool"
	yamlTagNull  = "!!null"
	yamlTagMap   = "!!map"
	yamlTagSeq   = "!!seq"
	jsonNull     = "null"
	yamlTrue     = "true"
)

// Path is the location of a mapping key in a profile document: each element
// is a mapping key (string) or a list index (int).
type Path []any

// String renders the path as `actor.env.FOO` or `commands[0].when`.
func (p Path) String() string {
	var b strings.Builder
	for _, element := range p {
		switch e := element.(type) {
		case int:
			fmt.Fprintf(&b, "[%d]", e)
		default:
			if b.Len() > 0 {
				b.WriteByte('.')
			}
			fmt.Fprint(&b, e)
		}
	}
	return b.String()
}

// tree is one YAML value as JSON: a literal, a mapping in written order, or
// a sequence.
type tree struct {
	literal string
	mapping bool
	keys    []string
	values  []*tree
	items   []*tree
	isSeq   bool
}

func (t *tree) isNull() bool {
	return !t.mapping && !t.isSeq && t.literal == jsonNull
}

// yamlToJSON reads one YAML document into JSON twice, with and without the
// mapping entries whose value is null, and lists where those entries were.
func yamlToJSON(data []byte) ([]byte, []byte, []Path, error) {
	root, err := parseDocument(data)
	if err != nil {
		return nil, nil, nil, err
	}
	var nulls []Path
	collectNulls(root, nil, &nulls)
	var full, stripped bytes.Buffer
	writeJSON(&full, root, false)
	writeJSON(&stripped, root, true)
	return full.Bytes(), stripped.Bytes(), nulls, nil
}

func parseDocument(data []byte) (*tree, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	err := decoder.Decode(&document)
	if errors.Is(err, io.EOF) {
		return &tree{mapping: true}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("invalid YAML: %w", err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid YAML: a profile is one document")
	}
	node := &document
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) == 0 {
			return &tree{mapping: true}, nil
		}
		node = node.Content[0]
	}
	root, err := convert(node, 0)
	if err != nil {
		return nil, err
	}
	// An empty document, or one of only comments, is an empty mapping.
	if root.isNull() {
		return &tree{mapping: true}, nil
	}
	if !root.mapping {
		return nil, errors.New("a profile is not a mapping")
	}
	return root, nil
}

func convert(node *yaml.Node, depth int) (*tree, error) {
	if depth > maxDepth {
		return nil, errors.New("invalid YAML: nested too deeply")
	}
	switch node.Kind {
	case yaml.AliasNode:
		if node.Alias == nil {
			return nil, errors.New("invalid YAML: unknown alias")
		}
		return convert(node.Alias, depth+1)
	case yaml.ScalarNode:
		literal, err := scalarJSON(node)
		if err != nil {
			return nil, err
		}
		return &tree{literal: literal}, nil
	case yaml.SequenceNode:
		if node.Style&yaml.TaggedStyle != 0 && node.ShortTag() != yamlTagSeq {
			return nil, fmt.Errorf("line %d: unsupported tag %s", node.Line, node.Tag)
		}
		t := &tree{isSeq: true, items: make([]*tree, 0, len(node.Content))}
		for _, item := range node.Content {
			child, err := convert(item, depth+1)
			if err != nil {
				return nil, err
			}
			// NOTICE: no list in a profile takes a null item; Go would read
			// one as an empty value instead of refusing it.
			if child.isNull() {
				return nil, fmt.Errorf("line %d: a list item is null", item.Line)
			}
			t.items = append(t.items, child)
		}
		return t, nil
	case yaml.MappingNode:
		return convertMapping(node, depth)
	default:
		return nil, fmt.Errorf("line %d: unsupported YAML node", node.Line)
	}
}

func convertMapping(node *yaml.Node, depth int) (*tree, error) {
	if node.Style&yaml.TaggedStyle != 0 && node.ShortTag() != yamlTagMap {
		return nil, fmt.Errorf("line %d: unsupported tag %s", node.Line, node.Tag)
	}
	t := &tree{mapping: true}
	seen := map[string]bool{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		for keyNode.Kind == yaml.AliasNode && keyNode.Alias != nil {
			keyNode = keyNode.Alias
		}
		if keyNode.Kind != yaml.ScalarNode {
			return nil, fmt.Errorf("line %d: a mapping key must be a string", keyNode.Line)
		}
		literal, err := scalarJSON(keyNode)
		if err != nil {
			return nil, err
		}
		var key string
		if json.Unmarshal([]byte(literal), &key) != nil {
			return nil, fmt.Errorf("line %d: mapping key %s must be a string", keyNode.Line, keyNode.Value)
		}
		if seen[key] {
			return nil, fmt.Errorf("line %d: duplicate entry with key %q", keyNode.Line, key)
		}
		seen[key] = true
		value, err := convert(node.Content[i+1], depth+1)
		if err != nil {
			return nil, err
		}
		t.keys = append(t.keys, key)
		t.values = append(t.values, value)
	}
	return t, nil
}

// scalarJSON is a scalar as a JSON literal, typed as needy's YAML reader
// types it.
func scalarJSON(node *yaml.Node) (string, error) {
	quoted := node.Style&(yaml.SingleQuotedStyle|yaml.DoubleQuotedStyle|yaml.LiteralStyle|yaml.FoldedStyle) != 0
	if node.Style&yaml.TaggedStyle != 0 {
		return taggedScalarJSON(node)
	}
	if quoted {
		return quoteJSON(node.Value), nil
	}
	return plainScalarJSON(node.Value)
}

func taggedScalarJSON(node *yaml.Node) (string, error) {
	value := node.Value
	invalid := func(what string) error {
		return fmt.Errorf("line %d: invalid value %q, expected %s", node.Line, value, what)
	}
	switch node.ShortTag() {
	case yamlTagStr:
		return quoteJSON(value), nil
	case yamlTagBool:
		if b, ok := parseYAMLBool(value); ok {
			return strconv.FormatBool(b), nil
		}
		return "", invalid("a boolean")
	case yamlTagInt:
		if literal, ok := parseYAMLInt(value); ok {
			return literal, nil
		}
		return "", invalid("an integer")
	case yamlTagFloat:
		if f, ok := parseYAMLFloat(value); ok {
			return floatJSON(f)
		}
		return "", invalid("a float")
	case yamlTagNull:
		if isYAMLNull(value) {
			return jsonNull, nil
		}
		return "", invalid("null")
	default:
		return "", fmt.Errorf("line %d: unsupported tag %s", node.Line, node.Tag)
	}
}

// plainScalarJSON resolves an untagged plain scalar as serde_yaml 0.9 does:
// null, then a boolean, an integer, a float, and otherwise a string.
func plainScalarJSON(value string) (string, error) {
	if value == "" || isYAMLNull(value) {
		return jsonNull, nil
	}
	if b, ok := parseYAMLBool(value); ok {
		return strconv.FormatBool(b), nil
	}
	if literal, ok := parseYAMLInt(value); ok {
		return literal, nil
	}
	if !digitsButNotNumber(value) {
		if f, ok := parseYAMLFloat(value); ok {
			return floatJSON(f)
		}
	}
	return quoteJSON(value), nil
}

// plainIsString reports whether needy reads `value`, written as a plain
// scalar, as that string.
func plainIsString(value string) bool {
	literal, err := plainScalarJSON(value)
	return err == nil && strings.HasPrefix(literal, `"`)
}

func isYAMLNull(value string) bool {
	switch value {
	case jsonNull, "Null", "NULL", "~":
		return true
	}
	return false
}

func parseYAMLBool(value string) (bool, bool) {
	switch {
	case slices.Contains([]string{yamlTrue, "True", "TRUE"}, value):
		return true, true
	case slices.Contains([]string{"false", "False", "FALSE"}, value):
		return false, true
	}
	return false, false
}

// digitsButNotNumber is a leading zero followed by digits, which YAML 1.2
// reads as a string.
func digitsButNotNumber(value string) bool {
	value = strings.TrimPrefix(strings.TrimPrefix(value, "-"), "+")
	if len(value) <= 1 || value[0] != '0' {
		return false
	}
	for _, b := range []byte(value[1:]) {
		if b < '0' || b > '9' {
			return false
		}
	}
	return true
}

var (
	rustDecimal = regexp.MustCompile(`^[0-9]+$`)
	rustRadix   = map[int]*regexp.Regexp{
		16: regexp.MustCompile(`^[0-9a-fA-F]+$`),
		8:  regexp.MustCompile(`^[0-7]+$`),
		2:  regexp.MustCompile(`^[01]+$`),
	}
)

// rustParseUint parses digits as Rust's `from_str_radix` does: an optional
// leading `+`, then digits only.
func rustParseUint(text string, base int) (uint64, bool) {
	text = strings.TrimPrefix(text, "+")
	pattern := rustDecimal
	if base != 10 {
		pattern = rustRadix[base]
	}
	if !pattern.MatchString(text) {
		return 0, false
	}
	n, err := strconv.ParseUint(text, base, 64)
	return n, err == nil
}

// parseYAMLInt is an integer as serde_yaml reads one into a value (u64,
// then i64), as a decimal JSON literal.
func parseYAMLInt(value string) (string, bool) {
	unpositive := strings.TrimPrefix(value, "+")
	for _, radix := range []struct {
		prefix string
		base   int
	}{
		{"0x", 16},
		{"0o", 8},
		{"0b", 2},
	} {
		if rest, ok := strings.CutPrefix(unpositive, radix.prefix); ok {
			if strings.HasPrefix(rest, "+") || strings.HasPrefix(rest, "-") {
				return "", false
			}
			if n, ok := rustParseUint(rest, radix.base); ok {
				return strconv.FormatUint(n, 10), true
			}
		}
	}
	if !strings.HasPrefix(unpositive, "+") && !strings.HasPrefix(unpositive, "-") && !digitsButNotNumber(value) {
		if n, ok := rustParseUint(unpositive, 10); ok {
			return strconv.FormatUint(n, 10), true
		}
	}
	for _, radix := range []struct {
		prefix string
		base   int
	}{
		{"-0x", 16},
		{"-0o", 8},
		{"-0b", 2},
	} {
		if rest, ok := strings.CutPrefix(value, radix.prefix); ok {
			if n, ok := negativeInt(rest, radix.base); ok {
				return n, true
			}
		}
	}
	if digitsButNotNumber(value) {
		return "", false
	}
	if rest, ok := strings.CutPrefix(value, "-"); ok {
		return negativeInt(rest, 10)
	}
	return "", false
}

func negativeInt(digits string, base int) (string, bool) {
	if strings.HasPrefix(digits, "+") || strings.HasPrefix(digits, "-") {
		return "", false
	}
	n, ok := rustParseUint(digits, base)
	if !ok || n > 1<<63 {
		return "", false
	}
	if n == 0 {
		return "0", true
	}
	return "-" + strconv.FormatUint(n, 10), true
}

var rustFloat = regexp.MustCompile(`^[+-]?(?:[0-9]+\.?[0-9]*|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)

// rustParseFloat parses text as Rust's `f64::from_str` does, finite values
// only.
func rustParseFloat(text string) (float64, bool) {
	if !rustFloat.MatchString(text) {
		return 0, false
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, false
	}
	return f, true
}

// parseYAMLFloat is a finite float as serde_yaml reads one; `.inf` and
// `.nan` have no JSON form and are refused.
func parseYAMLFloat(value string) (float64, bool) {
	unpositive := value
	if rest, ok := strings.CutPrefix(value, "+"); ok {
		if strings.HasPrefix(rest, "+") || strings.HasPrefix(rest, "-") {
			return 0, false
		}
		unpositive = rest
	}
	return rustParseFloat(unpositive)
}

// floatJSON is a float as a JSON literal that still reads as a float.
func floatJSON(f float64) (string, error) {
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return "", fmt.Errorf("unsupported float %v", f)
	}
	text := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.ContainsAny(text, ".eEn") {
		text += ".0"
	}
	return text, nil
}

func quoteJSON(value string) string {
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
	return strings.TrimSuffix(b.String(), "\n")
}

// collectNulls lists the mapping keys whose value is null.
func collectNulls(t *tree, path Path, nulls *[]Path) {
	switch {
	case t.mapping:
		for i, key := range t.keys {
			child := append(append(Path{}, path...), key)
			if t.values[i].isNull() {
				*nulls = append(*nulls, child)
				continue
			}
			collectNulls(t.values[i], child, nulls)
		}
	case t.isSeq:
		for i, item := range t.items {
			collectNulls(item, append(append(Path{}, path...), i), nulls)
		}
	}
}

func writeJSON(b *bytes.Buffer, t *tree, strip bool) {
	switch {
	case t.mapping:
		b.WriteByte('{')
		first := true
		for i, key := range t.keys {
			if strip && t.values[i].isNull() {
				continue
			}
			if !first {
				b.WriteByte(',')
			}
			first = false
			b.WriteString(quoteJSON(key))
			b.WriteByte(':')
			writeJSON(b, t.values[i], strip)
		}
		b.WriteByte('}')
	case t.isSeq:
		b.WriteByte('[')
		for i, item := range t.items {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJSON(b, item, strip)
		}
		b.WriteByte(']')
	default:
		b.WriteString(t.literal)
	}
}

// jsonToYAML writes JSON as YAML in the JSON's key order, adds the null
// entries at `nulls`, and quotes every string that needy would otherwise
// read as another type.
func jsonToYAML(data []byte, nulls []Path) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	root, err := jsonNode(decoder)
	if err != nil {
		return nil, err
	}
	for _, path := range nulls {
		insertNull(root, path)
	}
	var b bytes.Buffer
	encoder := yaml.NewEncoder(&b)
	encoder.SetIndent(2)
	if err := encoder.Encode(root); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func jsonNode(decoder *json.Decoder) (*yaml.Node, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	return jsonTokenNode(decoder, token)
}

func jsonTokenNode(decoder *json.Decoder, token json.Token) (*yaml.Node, error) {
	switch v := token.(type) {
	case json.Delim:
		switch v {
		case '{':
			node := &yaml.Node{Kind: yaml.MappingNode, Tag: yamlTagMap}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return nil, err
				}
				key, _ := keyToken.(string)
				value, err := jsonNode(decoder)
				if err != nil {
					return nil, err
				}
				node.Content = append(node.Content, stringNode(key), value)
			}
			if _, err := decoder.Token(); err != nil {
				return nil, err
			}
			return node, nil
		case '[':
			node := &yaml.Node{Kind: yaml.SequenceNode, Tag: yamlTagSeq}
			for decoder.More() {
				item, err := jsonNode(decoder)
				if err != nil {
					return nil, err
				}
				node.Content = append(node.Content, item)
			}
			if _, err := decoder.Token(); err != nil {
				return nil, err
			}
			return node, nil
		}
		return nil, fmt.Errorf("unexpected JSON delimiter %v", v)
	case string:
		return stringNode(v), nil
	case json.Number:
		return &yaml.Node{Kind: yaml.ScalarNode, Value: v.String()}, nil
	case bool:
		return &yaml.Node{Kind: yaml.ScalarNode, Value: strconv.FormatBool(v)}, nil
	case nil:
		return nullNode(), nil
	}
	return nil, fmt.Errorf("unexpected JSON token %v", token)
}

func nullNode() *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Value: jsonNull}
}

func stringNode(value string) *yaml.Node {
	node := &yaml.Node{Kind: yaml.ScalarNode, Tag: yamlTagStr, Value: value}
	if !plainIsString(value) {
		node.Style = yaml.DoubleQuotedStyle
	}
	return node
}

// insertNull adds `key: null` at `path` when the key is absent, creating
// the mappings on the way.
func insertNull(node *yaml.Node, path Path) {
	for i, element := range path {
		last := i == len(path)-1
		switch e := element.(type) {
		case string:
			if node.Kind != yaml.MappingNode {
				return
			}
			next := mappingValue(node, e)
			if last {
				if next == nil {
					node.Content = append(node.Content, stringNode(e), nullNode())
				}
				return
			}
			if next == nil {
				next = &yaml.Node{Kind: yaml.MappingNode, Tag: yamlTagMap}
				node.Content = append(node.Content, stringNode(e), next)
			}
			node = next
		case int:
			if last || node.Kind != yaml.SequenceNode || e < 0 || e >= len(node.Content) {
				return
			}
			node = node.Content[e]
		default:
			return
		}
	}
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}
