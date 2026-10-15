package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/smykla-skalski/bilgie/internal/fsx"
	"gopkg.in/yaml.v3"
)

// Override turns one provider on in the config file. A built-in provider
// carries only its name: the entry gets `enabled: true` and nothing else.
// Define adds a provider that has no entry yet, such as a custom
// dir-pattern; an entry that already exists is only enabled, never rewritten.
type Override struct {
	Define *Provider
	Name   string
}

// WriteOverrides merges the entries into the config file and touches nothing
// else: unrelated keys, comments and order stay as they are, and defaults are
// never dumped into the file. A missing file is created. It returns the names
// whose entry changed.
func (l *Loader) WriteOverrides(entries []Override) ([]string, error) {
	path, err := l.path()
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if err := validateOverride(e); err != nil {
			return nil, err
		}
	}

	data, err := fsx.ReadRegular(path, maxConfigBytes)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read config: %w", err)
	}
	root, prefix, err := parseRoot(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	providers, err := childMapping(root, "providers")
	if err != nil {
		return nil, err
	}

	var changed []string
	for _, e := range entries {
		did, err := applyOverride(providers, e)
		if err != nil {
			return nil, err
		}
		if did {
			changed = append(changed, e.Name)
		}
	}
	if len(changed) == 0 {
		return nil, nil
	}

	out, err := encodeRoot(root)
	if err != nil {
		return nil, err
	}
	out = append(prefix, out...)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create config dir: %w", err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return nil, fmt.Errorf("write config: %w", err)
	}
	return changed, nil
}

func validateOverride(e Override) error {
	if e.Name == "" {
		return errors.New("override: provider name is required")
	}
	if e.Define == nil {
		return nil
	}
	def := *e.Define
	def.Enabled = true
	if err := (&Config{Providers: map[string]Provider{e.Name: def}}).Validate(); err != nil {
		return fmt.Errorf("override %s: %w", e.Name, err)
	}
	return nil
}

// parseRoot returns the document node of the file. A file with no content, or
// only comments, gets a fresh mapping and the original text comes back as the
// prefix to keep in front of it. A file with several documents is refused:
// re-encoding would drop all but the first.
func parseRoot(data []byte) (doc *yaml.Node, prefix []byte, err error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var first yaml.Node
	switch err := dec.Decode(&first); {
	case errors.Is(err, io.EOF):
		return newRoot(), withNewline(data), nil
	case err != nil:
		return nil, nil, err
	}
	var next yaml.Node
	if err := dec.Decode(&next); !errors.Is(err, io.EOF) {
		return nil, nil, errors.New("the file holds more than one YAML document; merge them first")
	}
	if first.Kind != yaml.DocumentNode || len(first.Content) != 1 {
		return nil, nil, errors.New("not a YAML document")
	}
	if first.Content[0].Kind != yaml.MappingNode {
		return nil, nil, errors.New("the top level is not a mapping")
	}
	return &first, nil, nil
}

func newRoot() *yaml.Node {
	root := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	setKey(root, "version", scalar(currentVersion, "!!str"))
	return root
}

func withNewline(data []byte) []byte {
	if len(data) == 0 || data[len(data)-1] == '\n' {
		return data
	}
	return append(data, '\n')
}

// encodeRoot renders a node produced by parseRoot.
func encodeRoot(root *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	return buf.Bytes(), nil
}

// mapping unwraps a document node.
func mapping(n *yaml.Node) *yaml.Node {
	if n.Kind == yaml.DocumentNode {
		return n.Content[0]
	}
	return n
}

// childMapping returns the mapping stored under key, creating it when the key
// is missing or empty. Anything else under the key is an error: the file is
// the user's and is never reshaped.
func childMapping(parent *yaml.Node, key string) (*yaml.Node, error) {
	m := mapping(parent)
	value := lookup(m, key)
	switch {
	case value == nil:
		value = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		setKey(m, key, value)
	case value.Kind == yaml.ScalarNode && value.Tag == "!!null":
		*value = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	case value.Kind != yaml.MappingNode:
		return nil, fmt.Errorf("config key %q is not a mapping", key)
	}
	return value, nil
}

func lookup(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func setKey(m *yaml.Node, key string, value *yaml.Node) {
	m.Content = append(m.Content, scalar(key, "!!str"), value)
}

func scalar(value, tag string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value}
}

// applyOverride enables the provider's entry, defining it first when it has
// none. It reports whether the file content changes.
func applyOverride(providers *yaml.Node, e Override) (bool, error) {
	entry := lookup(providers, e.Name)
	if entry == nil {
		if e.Define == nil {
			entry = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			setKey(entry, "enabled", scalar("true", "!!bool"))
			setKey(providers, e.Name, entry)
			return true, nil
		}
		def := *e.Define
		def.Enabled = true
		var node yaml.Node
		if err := node.Encode(def); err != nil {
			return false, fmt.Errorf("encode provider %s: %w", e.Name, err)
		}
		setKey(providers, e.Name, &node)
		return true, nil
	}

	if entry.Kind == yaml.AliasNode && entry.Alias != nil {
		*entry = *deepCopy(entry.Alias)
	}
	if entry.Kind == yaml.ScalarNode && entry.Tag == "!!null" {
		*entry = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	}
	if entry.Kind != yaml.MappingNode {
		return false, fmt.Errorf("providers.%s is not a mapping", e.Name)
	}
	enabled := lookup(entry, "enabled")
	switch {
	case enabled == nil:
		setKey(entry, "enabled", scalar("true", "!!bool"))
	case enabled.Kind != yaml.ScalarNode:
		return false, fmt.Errorf("providers.%s.enabled is not a boolean", e.Name)
	case enabled.Tag == "!!bool" && strings.EqualFold(enabled.Value, "true"):
		return false, nil
	default:
		enabled.Kind, enabled.Tag, enabled.Value, enabled.Style = yaml.ScalarNode, "!!bool", "true", 0
	}
	return true, nil
}

// deepCopy duplicates a node so an entry that was an alias can be edited
// without changing the anchor it pointed at.
func deepCopy(n *yaml.Node) *yaml.Node {
	c := *n
	c.Anchor = ""
	c.Content = make([]*yaml.Node, len(n.Content))
	for i, child := range n.Content {
		c.Content[i] = deepCopy(child)
	}
	return &c
}
