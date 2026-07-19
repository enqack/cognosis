package mcpserver

import (
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
)

// schemaFor infers the input schema for a tool's argument struct and collapses
// the nullable-type unions jsonschema-go emits for Go slices and maps
// ("type": ["null","array"]) into the single non-null type.
//
// The unions are technically correct — a nil Go slice marshals to JSON null —
// but real MCP clients degrade on them: a client whose schema model only
// represents a single type string drops the field to untyped, then serializes
// array arguments as strings, which this server's own validation rejects.
// Observed 2026-07-19 with compile_lifecycle's reinforce from Claude Code.
// Absent-vs-null is not a distinction any tool here cares about, so the
// portable single-type form costs nothing.
//
// Tools whose argument structs contain a slice or map must register with
// InputSchema: schemaFor[args](), or the SDK re-infers the union form;
// TestToolSchemasCarryNoTypeUnions holds the whole surface to that.
func schemaFor[T any]() *jsonschema.Schema {
	s, err := jsonschema.For[T](nil)
	if err != nil {
		// Registration-time, deterministic in the args struct: a failure here
		// is a programming error, same class as a bad tool name.
		panic(fmt.Sprintf("schemaFor: %v", err))
	}
	stripNullUnions(s)
	return s
}

func stripNullUnions(s *jsonschema.Schema) {
	if s == nil {
		return
	}
	if len(s.Types) > 0 {
		kept := make([]string, 0, len(s.Types))
		for _, t := range s.Types {
			if t != "null" {
				kept = append(kept, t)
			}
		}
		switch len(kept) {
		case 1:
			s.Type, s.Types = kept[0], nil
		default:
			s.Types = kept
		}
	}
	for _, p := range s.Properties {
		stripNullUnions(p)
	}
	for _, p := range s.PatternProperties {
		stripNullUnions(p)
	}
	stripNullUnions(s.Items)
	stripNullUnions(s.AdditionalProperties)
	for _, sub := range s.AllOf {
		stripNullUnions(sub)
	}
	for _, sub := range s.AnyOf {
		stripNullUnions(sub)
	}
	for _, sub := range s.OneOf {
		stripNullUnions(sub)
	}
}
