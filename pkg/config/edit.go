package config

import (
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

func EditAgentConfig(path string, settings ...Setting) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading agent config %s: %w", path, err)
	}
	edited, err := EditAgentConfigBytes(data, settings...)
	if err != nil {
		return err
	}
	return os.WriteFile(path, edited, 0o644)
}

// EditAgentConfigBytes is EditAgentConfig on a document in memory.
func EditAgentConfigBytes(data []byte, settings ...Setting) ([]byte, error) {
	// Keys and values are checked against the struct before any text is
	// touched, so the error for a typo names the real fields.
	scratch, err := ParseAgentConfig(data)
	if err != nil {
		return nil, err
	}
	for _, s := range settings {
		if err := setByPath(reflect.ValueOf(&scratch).Elem(), strings.Split(resolveAlias(s.Path), "."), s.Value); err != nil {
			return nil, fmt.Errorf("config set %s: %w", s.Path, err)
		}
	}

	out := data
	for _, s := range settings {
		if out, err = editYAML(out, strings.Split(resolveAlias(s.Path), "."), s.Value); err != nil {
			return nil, fmt.Errorf("config set %s: %w", s.Path, err)
		}
	}
	if _, err := ParseAgentConfig(out); err != nil {
		return nil, err
	}
	return out, nil
}

func resolveAlias(path string) string {
	if alias, ok := setAliases[path]; ok {
		return alias
	}
	return path
}

func editYAML(data []byte, path []string, value string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")

	// Walk the existing document as far as the path goes.
	section := documentRoot(&doc)
	end, indent := len(lines), 0 // where the current section ends, and its key indentation
	for len(path) > 0 {
		keyNode, valueNode := childNode(section, path[0])
		if keyNode == nil {
			break
		}
		if len(path) == 1 {
			if valueNode.Kind != yaml.ScalarNode {
				return nil, fmt.Errorf("%q is a section, not a value", path[0])
			}
			i := valueNode.Line - 1
			line := lines[i][:valueNode.Column-1] + yamlScalar(value)
			if valueNode.LineComment != "" {
				line += "  " + valueNode.LineComment
			}
			lines[i] = line
			return join(lines), nil
		}
		switch valueNode.Kind {
		case yaml.MappingNode:
			section, end, indent = valueNode, lastLine(valueNode), valueNode.Content[0].Column-1
		case yaml.SequenceNode:
			section, end = valueNode, lastLine(valueNode)
		default:
			return nil, fmt.Errorf("%q is a value, not a section", path[0])
		}
		path = path[1:]
	}

	// Appending a scalar to an existing sequence of scalars - repositories.0.values.1,
	// e.g when values already has one item: one new line, no new header.
	if len(path) == 1 && section.Kind == yaml.SequenceNode && !holdsMappings(section) {
		if _, err := strconv.Atoi(path[0]); err == nil {
			itemIndent := indent + 2
			if n := len(section.Content); n > 0 {
				itemIndent = section.Content[n-1].Column - 1 - 2
			}
			line := strings.Repeat(" ", itemIndent) + "- " + yamlScalar(value)
			return join(append(lines[:end], append([]string{line}, lines[end:]...)...)), nil
		}
	}

	// A brand-new scalar sequence - repositories.0.values.0, when values does not exist at all yet
	if len(path) == 2 && section.Kind == yaml.MappingNode {
		if _, err := strconv.Atoi(path[1]); err == nil {
			pad := strings.Repeat(" ", indent)
			block := []string{pad + path[0] + ":", pad + "  - " + yamlScalar(value)}
			return join(append(lines[:end], append(block, lines[end:]...)...)), nil
		}
	}

	// The rest of the path is new. List items are never created here: a repository needs several keys at once, which is an edit, not a set.
	if _, isIndex := strconv.Atoi(path[0]); isIndex == nil || section.Kind == yaml.SequenceNode {
		return nil, fmt.Errorf("list item %q does not exist; add it by editing the file", path[0])
	}
	var block []string
	for i, key := range path {
		pad := strings.Repeat(" ", indent+2*i)
		if i == len(path)-1 {
			block = append(block, pad+key+": "+yamlScalar(value))
		} else {
			block = append(block, pad+key+":")
		}
	}
	return join(append(lines[:end], append(block, lines[end:]...)...)), nil
}

// holdsMappings reports whether a sequence's items are mappings like repositories, where a new item needs several keys rather than scalars
func holdsMappings(seq *yaml.Node) bool {
	for _, c := range seq.Content {
		if c.Kind == yaml.MappingNode {
			return true
		}
	}
	return false
}

func childNode(node *yaml.Node, key string) (*yaml.Node, *yaml.Node) {
	switch node.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == key {
				return node.Content[i], node.Content[i+1]
			}
		}
	case yaml.SequenceNode:
		if idx, err := strconv.Atoi(key); err == nil && idx >= 0 && idx < len(node.Content) {
			return node.Content[idx], node.Content[idx]
		}
	}
	return nil, nil
}

// lastLine is the 0-based line just past the last line a node occupies.
func lastLine(node *yaml.Node) int {
	end := node.Line
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n.Line > end {
			end = n.Line
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(node)
	return end
}

func documentRoot(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		return doc.Content[0]
	}
	return doc
}

func join(lines []string) []byte { return []byte(strings.Join(lines, "\n") + "\n") }
