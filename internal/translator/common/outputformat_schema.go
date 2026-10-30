package common

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"
)

// Anthropic compiles an output schema into a grammar and answers 400 for any
// JSON Schema feature outside the subset it supports. Its SDKs rewrite a
// schema before sending it, and anthropicOutputSchema rewrites it the same
// way: constraints the grammar cannot express (minimum, maxLength, pattern
// and the like) move into the description, where the model still reads them;
// every object is closed with additionalProperties: false; and oneOf becomes
// anyOf. Closing an object or dropping a constraint only narrows what the
// reply may be, so a reply that fits the rewritten schema still fits the
// client's.
//
// A schema that cannot be rewritten that way is not sent as a format, and the
// caller asks for it in the system prompt instead: recursion, which Anthropic
// rejects; free-form objects and maps, which a closed object could only
// answer with {}; tuples; a root that is not an object; and more optional or
// union-typed parameters than the API allows.

// Limits from Anthropic's structured outputs documentation. The API counts
// them across every strict schema in a request, tools included; only the
// output schema is counted here.
const (
	anthropicMaxOptionalParams = 24
	anthropicMaxUnionParams    = 16
)

// anthropicStringFormats are the string formats Anthropic's grammar knows.
var anthropicStringFormats = map[string]bool{
	"date-time": true, "time": true, "date": true, "duration": true, "email": true,
	"hostname": true, "uri": true, "ipv4": true, "ipv6": true, "uuid": true,
}

type anthropicSchemaRewrite struct {
	defs     map[string]gjson.Result // definitions at the root, by name
	refs     map[string][]string     // definition ("" for the root) -> definitions it refers to
	optional int
	unions   int
}

// anthropicOutputSchema returns raw rewritten for Anthropic's
// output_config.format, or ok=false when it cannot be sent there.
func anthropicOutputSchema(raw string) (string, bool) {
	root := gjson.Parse(raw)
	if !root.IsObject() {
		return "", false
	}
	rw := &anthropicSchemaRewrite{defs: map[string]gjson.Result{}, refs: map[string][]string{}}
	var names []string
	collected := true
	root.ForEach(func(key, defs gjson.Result) bool {
		if k := key.String(); k != "$defs" && k != "definitions" {
			return true
		}
		if !defs.IsObject() {
			collected = false
			return false
		}
		defs.ForEach(func(name, def gjson.Result) bool {
			if _, duplicate := rw.defs[name.String()]; duplicate {
				collected = false
				return false
			}
			rw.defs[name.String()] = def
			names = append(names, name.String())
			return true
		})
		return collected
	})
	if !collected {
		return "", false
	}

	body, ok := rw.rewrite(root, "", true)
	if !ok {
		return "", false
	}
	// The reply must be an object, written at the root or behind a reference.
	top := root
	if body.has("$ref") {
		top = rw.defs[rw.refs[""][0]]
	}
	if !isObjectRoot(top) {
		return "", false
	}
	if len(names) > 0 {
		defs := &jsonPairs{}
		for _, name := range names {
			def, ok := rw.rewrite(rw.defs[name], name, false)
			if !ok {
				return "", false
			}
			defs.add(name, def.String())
		}
		// Written as $defs whichever name the client used, with every
		// reference pointing there.
		body.add("$defs", defs.String())
	}
	if rw.recursive() || rw.optional > anthropicMaxOptionalParams || rw.unions > anthropicMaxUnionParams {
		return "", false
	}
	return body.String(), true
}

func (rw *anthropicSchemaRewrite) rewrite(node gjson.Result, def string, root bool) (*jsonPairs, bool) {
	if !node.IsObject() {
		// true and false are schemas too, accepting anything or nothing.
		return nil, false
	}
	var keys []string
	values := map[string]gjson.Result{}
	node.ForEach(func(key, value gjson.Result) bool {
		if _, seen := values[key.String()]; !seen {
			keys = append(keys, key.String())
		}
		values[key.String()] = value
		return true
	})

	if ref, isRef := values["$ref"]; isRef {
		name, ok := rw.refName(ref.String())
		if !ok {
			return nil, false
		}
		rw.refs[def] = append(rw.refs[def], name)
		// Like Anthropic's SDKs, a reference goes alone: the grammar does not
		// read what is written beside it.
		out := &jsonPairs{}
		out.add("$ref", jsonString("#/$defs/"+name))
		return out, true
	}

	_, hasAnyOf := values["anyOf"]
	_, hasOneOf := values["oneOf"]
	_, hasAllOf := values["allOf"]
	if hasAnyOf && hasOneOf {
		return nil, false
	}
	combined := hasAnyOf || hasOneOf || hasAllOf
	kind := values["type"]
	properties := values["properties"]
	_, hasAdditional := values["additionalProperties"]
	isObject := typeIncludes(kind, "object") || (!kind.Exists() && !combined && (properties.Exists() || hasAdditional))
	isString := typeIncludes(kind, "string")

	out := &jsonPairs{}
	var notes []string
	descriptionAt := -1
	for _, key := range keys {
		value := values[key]
		switch key {
		case "type":
			if combined {
				continue // as in the SDKs: the branches carry the types
			}
			if value.Type != gjson.String {
				if !isStringArray(value) {
					return nil, false
				}
				if len(value.Array()) > 1 {
					rw.unions++
				}
			}
			out.add(key, value.Raw)
		case "description":
			descriptionAt = out.count()
			out.add(key, `""`) // set below, with the notes
		case "title":
			if value.Type == gjson.String {
				out.add(key, value.Raw)
			}
		case "enum":
			if !value.IsArray() {
				return nil, false
			}
			out.add(key, value.Raw)
		case "const":
			out.add(key, value.Raw)
		case "anyOf", "oneOf", "allOf":
			branches, ok := rw.rewriteEach(value, def)
			if !ok {
				return nil, false
			}
			if key == "allOf" {
				out.add(key, branches)
				continue
			}
			rw.unions++
			out.add("anyOf", branches)
		case "properties":
			rewritten, ok := rw.rewriteProperties(value, def)
			if !ok {
				return nil, false
			}
			out.add(key, rewritten)
		case "required":
			if !isStringArray(value) {
				return nil, false
			}
			out.add(key, value.Raw)
		case "additionalProperties":
			// Written below, once it is known whether the object can be closed.
		case "items":
			if !value.IsObject() {
				return nil, false // a tuple
			}
			items, ok := rw.rewrite(value, def, false)
			if !ok {
				return nil, false
			}
			out.add(key, items.String())
		case "minItems":
			if value.Raw == "0" || value.Raw == "1" {
				out.add(key, value.Raw)
			} else {
				notes = append(notes, key+": "+compactJSON(value.Raw))
			}
		case "format":
			if isString && anthropicStringFormats[value.String()] {
				out.add(key, value.Raw)
			} else {
				notes = append(notes, key+": "+compactJSON(value.Raw))
			}
		case "$defs", "definitions":
			if !root {
				return nil, false // a nested definition needs a pointer this rewrite does not follow
			}
		case "$schema", "$id", "$comment":
			// About the schema document, not about the reply.
		default:
			if strings.HasPrefix(key, "x-") {
				continue // vendor extensions
			}
			notes = append(notes, key+": "+compactJSON(value.Raw))
		}
	}

	if isObject {
		closed := values["additionalProperties"].Type == gjson.False
		if !closed && countMembers(properties) == 0 {
			// A free-form object or a map: closed, it could only be {}.
			return nil, false
		}
		out.add("additionalProperties", "false")
		required := map[string]bool{}
		values["required"].ForEach(func(_, name gjson.Result) bool {
			required[name.String()] = true
			return true
		})
		properties.ForEach(func(name, _ gjson.Result) bool {
			if !required[name.String()] {
				rw.optional++
			}
			return true
		})
	}

	description := values["description"].String()
	if len(notes) > 0 {
		note := "{" + strings.Join(notes, ", ") + "}"
		if description != "" {
			description += "\n\n" + note
		} else {
			description = note
		}
	}
	if descriptionAt >= 0 {
		out.set(descriptionAt, jsonString(description))
	} else if description != "" {
		out.add("description", jsonString(description))
	}
	return out, true
}

func (rw *anthropicSchemaRewrite) rewriteEach(list gjson.Result, def string) (string, bool) {
	if !list.IsArray() {
		return "", false
	}
	var b strings.Builder
	b.WriteByte('[')
	ok, n := true, 0
	list.ForEach(func(_, branch gjson.Result) bool {
		rewritten, fine := rw.rewrite(branch, def, false)
		if !fine {
			ok = false
			return false
		}
		if n > 0 {
			b.WriteByte(',')
		}
		b.WriteString(rewritten.String())
		n++
		return true
	})
	b.WriteByte(']')
	return b.String(), ok && n > 0
}

func (rw *anthropicSchemaRewrite) rewriteProperties(properties gjson.Result, def string) (string, bool) {
	if !properties.IsObject() {
		return "", false
	}
	out := &jsonPairs{}
	ok := true
	properties.ForEach(func(name, schema gjson.Result) bool {
		rewritten, fine := rw.rewrite(schema, def, false)
		if !fine {
			ok = false
			return false
		}
		out.add(name.String(), rewritten.String())
		return true
	})
	return out.String(), ok
}

// refName returns the definition a reference points to. Only references to
// root definitions are followed: "#" is the root itself, so recursion.
func (rw *anthropicSchemaRewrite) refName(pointer string) (string, bool) {
	var name string
	switch {
	case strings.HasPrefix(pointer, "#/$defs/"):
		name = pointer[len("#/$defs/"):]
	case strings.HasPrefix(pointer, "#/definitions/"):
		name = pointer[len("#/definitions/"):]
	default:
		return "", false
	}
	if name == "" || strings.ContainsAny(name, "/~") {
		return "", false
	}
	if _, ok := rw.defs[name]; !ok {
		return "", false
	}
	return name, true
}

// recursive reports whether a definition refers back to itself, directly or
// through others.
func (rw *anthropicSchemaRewrite) recursive() bool {
	const visiting, done = 1, 2
	state := map[string]int{}
	var visit func(name string) bool
	visit = func(name string) bool {
		switch state[name] {
		case visiting:
			return true
		case done:
			return false
		}
		state[name] = visiting
		for _, next := range rw.refs[name] {
			if visit(next) {
				return true
			}
		}
		state[name] = done
		return false
	}
	for name := range rw.defs {
		if visit(name) {
			return true
		}
	}
	return false
}

func isObjectRoot(node gjson.Result) bool {
	kind := node.Get("type")
	if kind.Exists() {
		return kind.Type == gjson.String && kind.String() == "object"
	}
	return node.Get("properties").IsObject()
}

func typeIncludes(kind gjson.Result, name string) bool {
	if kind.Type == gjson.String {
		return kind.String() == name
	}
	found := false
	kind.ForEach(func(_, value gjson.Result) bool {
		found = value.String() == name
		return !found
	})
	return found
}

func isStringArray(value gjson.Result) bool {
	if !value.IsArray() {
		return false
	}
	ok := true
	value.ForEach(func(_, item gjson.Result) bool {
		ok = item.Type == gjson.String
		return ok
	})
	return ok
}

func countMembers(object gjson.Result) int {
	n := 0
	object.ForEach(func(_, _ gjson.Result) bool {
		n++
		return true
	})
	return n
}

func compactJSON(raw string) string {
	var b bytes.Buffer
	if err := json.Compact(&b, []byte(raw)); err != nil {
		return strings.TrimSpace(raw)
	}
	return b.String()
}

// Gemini's responseSchema is OpenAPI 3.0's Schema object rather than JSON
// Schema: type names are upper case, null is a nullable flag instead of a
// type, and propertyOrdering fixes the order of the keys in the reply.
// jsonSchemaFromGemini writes it as the JSON Schema the other protocols take.
// Google's JSON parser also takes these fields in snake_case.
var geminiSchemaKeys = map[string]string{
	"any_of":            "anyOf",
	"property_ordering": "propertyOrdering",
	"min_items":         "minItems",
	"max_items":         "maxItems",
	"min_length":        "minLength",
	"max_length":        "maxLength",
	"min_properties":    "minProperties",
	"max_properties":    "maxProperties",
}

func jsonSchemaFromGemini(node gjson.Result) string {
	if !node.IsObject() {
		return node.Raw
	}
	var keys []string
	fields := map[string]gjson.Result{}
	node.ForEach(func(key, value gjson.Result) bool {
		name := key.String()
		if renamed, ok := geminiSchemaKeys[name]; ok {
			name = renamed
		}
		if _, seen := fields[name]; !seen {
			keys = append(keys, name)
		}
		fields[name] = value
		return true
	})
	nullable := fields["nullable"].Bool()
	out := &jsonPairs{}
	for _, key := range keys {
		value := fields[key]
		switch key {
		case "type":
			name := strings.ToLower(value.String())
			if name == "" || name == "type_unspecified" {
				continue
			}
			if nullable && name != "null" {
				out.add(key, `[`+jsonString(name)+`,"null"]`)
			} else {
				out.add(key, jsonString(name))
			}
		case "nullable", "propertyOrdering", "example":
			// nullable folds into type, the order is applied to properties,
			// and an example does not constrain the reply.
		case "properties":
			out.add(key, geminiProperties(value, fields["propertyOrdering"]))
		case "items":
			out.add(key, jsonSchemaFromGemini(value))
		case "anyOf":
			out.add(key, geminiSchemaList(value))
		default:
			out.add(key, value.Raw)
		}
	}
	return out.String()
}

// geminiProperties writes properties in propertyOrdering's order, then the
// ones it does not name, so the reply keeps the key order Gemini would give.
func geminiProperties(properties, order gjson.Result) string {
	if !properties.IsObject() {
		return properties.Raw
	}
	var names []string
	schemas := map[string]gjson.Result{}
	properties.ForEach(func(name, schema gjson.Result) bool {
		names = append(names, name.String())
		schemas[name.String()] = schema
		return true
	})
	out := &jsonPairs{}
	written := map[string]bool{}
	order.ForEach(func(_, name gjson.Result) bool {
		if schema, ok := schemas[name.String()]; ok && !written[name.String()] {
			out.add(name.String(), jsonSchemaFromGemini(schema))
			written[name.String()] = true
		}
		return true
	})
	for _, name := range names {
		if !written[name] {
			out.add(name, jsonSchemaFromGemini(schemas[name]))
			written[name] = true
		}
	}
	return out.String()
}

func geminiSchemaList(list gjson.Result) string {
	if !list.IsArray() {
		return list.Raw
	}
	var b strings.Builder
	b.WriteByte('[')
	n := 0
	list.ForEach(func(_, schema gjson.Result) bool {
		if n > 0 {
			b.WriteByte(',')
		}
		b.WriteString(jsonSchemaFromGemini(schema))
		n++
		return true
	})
	b.WriteByte(']')
	return b.String()
}

// jsonPairs builds a JSON object that keeps its keys in the order they were
// added: the order of a schema's properties is the order a model writes them.
type jsonPairs struct {
	keys []string
	raws []string
}

func (p *jsonPairs) add(key, raw string) {
	p.keys = append(p.keys, key)
	p.raws = append(p.raws, raw)
}

func (p *jsonPairs) count() int { return len(p.keys) }

func (p *jsonPairs) set(i int, raw string) { p.raws[i] = raw }

func (p *jsonPairs) has(key string) bool {
	for _, k := range p.keys {
		if k == key {
			return true
		}
	}
	return false
}

func (p *jsonPairs) String() string {
	var b strings.Builder
	b.WriteByte('{')
	for i, key := range p.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(jsonString(key))
		b.WriteByte(':')
		b.WriteString(p.raws[i])
	}
	b.WriteByte('}')
	return b.String()
}
