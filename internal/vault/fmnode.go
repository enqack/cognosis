package vault

import "gopkg.in/yaml.v3"

// Frontmatter node surgery: targeted key edits that preserve the file's key
// order and comments, used by the lifecycle's rewrites. Ported from silo-kb.

func mappingNode(doc *yaml.Node) *yaml.Node {
	if doc == nil {
		return nil
	}
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		return doc.Content[0]
	}
	return doc
}

// SetFM sets (or appends) one scalar key on the note's frontmatter node and
// mirrors it into the parsed map so both views stay consistent.
func (n *Note) SetFM(key, value string) {
	if n.Frontmatter == nil {
		n.Frontmatter = map[string]any{}
	}
	n.Frontmatter[key] = value
	m := mappingNode(n.FMNode)
	if m == nil || m.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1].SetString(value)
			// SetString forces !!str; clear tag/style so numbers stay numbers.
			m.Content[i+1].Tag = ""
			m.Content[i+1].Style = 0
			return
		}
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Value: value})
}

// DeleteFM removes one key from both the node and the parsed map.
func (n *Note) DeleteFM(key string) {
	delete(n.Frontmatter, key)
	m := mappingNode(n.FMNode)
	if m == nil || m.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}
