package util

import (
	"sort"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	parametersJSONSchemaKey = "parametersJsonSchema"
	parametersKey           = "parameters"
)

// Where an Antigravity request holds JSON schemas, all under "request". The
// upstream reads the request as protobuf JSON, which takes a field by its JSON
// name or by its proto name, so both spellings are listed.
var (
	requestKeys = keySet("request")
	toolsKeys   = keySet("tools")
	// The members of a tool that list its function declarations.
	functionDeclarationListKeys = keySet("functionDeclarations", "function_declarations")
	// The members of a function declaration that hold a schema. Its name,
	// description and behavior are not schemas.
	functionDeclarationSchemaKeys = keySet(
		"parameters", "parametersJsonSchema", "parameters_json_schema",
		"response", "responseJsonSchema", "response_json_schema")
	generationConfigKeys = keySet("generationConfig", "generation_config")
	// The members of generationConfig that hold a schema. _responseJsonSchema is
	// the internal twin of responseJsonSchema that the Gemini API still lists.
	responseSchemaKeys = keySet(
		"responseSchema", "response_schema",
		"responseJsonSchema", "response_json_schema", "_responseJsonSchema")
	// generationConfig.responseFormat.text.schema holds one as well.
	responseFormatKeys     = keySet("responseFormat", "response_format")
	responseFormatTextKeys = keySet("text")
	responseFormatSchema   = keySet("schema")
)

// CleanAntigravityRequestSchemas readies the JSON schemas an Antigravity request
// declares: it renames each function declaration's "parametersJsonSchema" to
// "parameters", the only form Antigravity takes, and cleans every schema, with the
// Antigravity cleanup when addPlaceholder is set (the Claude family and
// gemini-3-pro-high) and the Gemini one otherwise.
//
// Schemas sit in known places under "request": the function declarations in tools
// and the response schema in generationConfig. Nothing else is touched. The cleanup
// used to run over the whole payload and edit every key it knew by name, so the
// function calls and responses replayed in contents were rewritten as if they were
// schemas: a Grep call's "pattern" turned into a "description", arguments holding a
// "$ref" were replaced outright, and the model was shown tool calls it never made.
// It also renamed every property called parametersJsonSchema, and emptied the whole
// payload when one sat inside another.
//
// Each schema is cleaned as if it sat alone at its real path, so the cleanup treats
// it exactly as it did inside the whole payload. That matters: cleaned bare, as the
// Claude translator cleans input_schema, the root schema of a tool would not get the
// "_" placeholder it gets at its real path. The payload is copied once, at the end.
func CleanAntigravityRequestSchemas(payload string, addPlaceholder bool) string {
	return cleanRequestSchemas(payload, addPlaceholder, cleanSchemaDoc)
}

// schemaCleaner cleans the schemas in a document holding nothing but them and the
// skeleton leading to them. CleanAntigravityRequestSchemas uses cleanSchemaDoc; the
// tests hold it to cleanJSONSchema, which cleanSchemaDoc must match.
type schemaCleaner func(doc string, addPlaceholder bool) string

func cleanRequestSchemas(payload string, addPlaceholder bool, clean schemaCleaner) string {
	// The entrypoints refuse a body that is not valid JSON (#1267), and the offsets
	// the edits are spliced at are only exact on valid JSON.
	if !gjson.Valid(payload) {
		return payload
	}
	var edits []schemaEdit
	cleanAt := func(schema gjson.Result, path ...string) {
		if cleaned := cleanSchemaAt(path, schema.Raw, addPlaceholder, clean); cleaned != schema.Raw {
			edits = append(edits, newSchemaEdit(schema, cleaned, strings.Join(path, ".")))
		}
	}
	forEachMember(gjson.Parse(payload), requestKeys, func(_ string, request gjson.Result) {
		forEachMember(request, toolsKeys, func(_ string, tools gjson.Result) {
			forEachElement(tools, func(i int, tool gjson.Result) {
				forEachMember(tool, functionDeclarationListKeys, func(listKey string, decls gjson.Result) {
					forEachElement(decls, func(j int, decl gjson.Result) {
						path := []string{"request", "tools", strconv.Itoa(i), listKey, strconv.Itoa(j)}
						if cleaned := cleanFunctionDeclaration(path, decl, addPlaceholder, clean); cleaned != decl.Raw {
							edits = append(edits, newSchemaEdit(decl, cleaned, strings.Join(path, ".")))
						}
					})
				})
			})
		})
		forEachMember(request, generationConfigKeys, func(configKey string, config gjson.Result) {
			forEachMember(config, responseSchemaKeys, func(key string, schema gjson.Result) {
				cleanAt(schema, "request", configKey, key)
			})
			forEachMember(config, responseFormatKeys, func(formatKey string, format gjson.Result) {
				forEachMember(format, responseFormatTextKeys, func(_ string, text gjson.Result) {
					forEachMember(text, responseFormatSchema, func(_ string, schema gjson.Result) {
						cleanAt(schema, "request", configKey, formatKey, "text", "schema")
					})
				})
			})
		})
	})
	return applySchemaEdits(payload, edits)
}

// cleanFunctionDeclaration returns the declaration at path with its
// parametersJsonSchema renamed to parameters and each schema it holds cleaned.
func cleanFunctionDeclaration(path []string, decl gjson.Result, addPlaceholder bool, clean schemaCleaner) string {
	if !decl.IsObject() {
		return decl.Raw
	}
	raw := decl.Raw
	renames := 0
	decl.ForEach(func(key, _ gjson.Result) bool {
		if key.String() == parametersJSONSchemaKey {
			renames++
		}
		return true
	})
	if renames > 0 {
		raw = renameParametersJSONSchema(path, raw, renames)
	}
	var edits []schemaEdit
	forEachMember(gjson.Parse(raw), functionDeclarationSchemaKeys, func(key string, schema gjson.Result) {
		schemaPath := append(path[:len(path):len(path)], key)
		if cleaned := cleanSchemaAt(schemaPath, schema.Raw, addPlaceholder, clean); cleaned != schema.Raw {
			edits = append(edits, newSchemaEdit(schema, cleaned, key))
		}
	})
	return applySchemaEdits(raw, edits)
}

// renameParametersJSONSchema renames the declaration's parametersJsonSchema to
// parameters, once per occurrence, the way the request-wide rename always did:
// RenameKey moves the value onto an existing parameters or to the end of the
// declaration. It runs on a skeleton of the declaration's real path, as the cleanup
// does, so the bytes come out as they did in the whole payload.
func renameParametersJSONSchema(path []string, decl string, occurrences int) string {
	head, tail := schemaSkeleton(path)
	doc := head + decl + tail
	at := strings.Join(path, ".") + "."
	for ; occurrences > 0; occurrences-- {
		renamed, err := RenameKey(doc, at+parametersJSONSchemaKey, at+parametersKey)
		if err != nil {
			// Not reached: the key is there and every step of the path is plain.
			return decl
		}
		doc = renamed
	}
	if renamed, ok := unwrapSkeleton(doc, head, tail); ok {
		return renamed
	}
	return decl
}

// cleanSchemaAt cleans schema as if it sat alone at path in the request: wrapped in
// a skeleton of that path, every key the cleanup finds in it has the path it has in
// the request, so the cleanup makes the edits it made over the whole payload.
func cleanSchemaAt(path []string, schema string, addPlaceholder bool, clean schemaCleaner) string {
	head, tail := schemaSkeleton(path)
	if cleaned, ok := unwrapSkeleton(clean(head+schema+tail, addPlaceholder), head, tail); ok {
		return cleaned
	}
	// The skeleton came back changed, which no keyword in the schema can do; keep
	// the schema as the client sent it rather than guess where it went.
	return schema
}

// schemaSkeleton returns the JSON around a value at path, every step an object key:
// gjson and sjson paths resolve an index on an object as that key, so the cleanup's
// paths into the value read and write as they would in the request. Every step is
// an index or an allow-listed key, and needs no escaping.
func schemaSkeleton(path []string) (head, tail string) {
	var b strings.Builder
	for _, step := range path {
		b.WriteString(`{"`)
		b.WriteString(step)
		b.WriteString(`":`)
	}
	return b.String(), strings.Repeat("}", len(path))
}

// unwrapSkeleton returns what doc holds inside the skeleton head…tail, and false
// when the skeleton did not come back intact or what it holds is not valid JSON.
func unwrapSkeleton(doc, head, tail string) (string, bool) {
	if len(doc) < len(head)+len(tail) || !strings.HasPrefix(doc, head) || !strings.HasSuffix(doc, tail) {
		return "", false
	}
	inner := doc[len(head) : len(doc)-len(tail)]
	if !gjson.Valid(inner) {
		return "", false
	}
	return inner, true
}

// schemaEdit replaces doc[start:end], which must read orig, with repl. path is the
// value's gjson path in doc, for making the edit by path instead.
type schemaEdit struct {
	start, end int
	orig, repl string
	path       string
}

func newSchemaEdit(value gjson.Result, repl, path string) schemaEdit {
	return schemaEdit{start: value.Index, end: value.Index + len(value.Raw), orig: value.Raw, repl: repl, path: path}
}

// applySchemaEdits splices the edits, which must not overlap, into doc in one copy.
// gjson places every value it parses from valid JSON exactly; should an edit still
// not find its original bytes there, the edits are made by path instead.
func applySchemaEdits(doc string, edits []schemaEdit) string {
	if len(edits) == 0 {
		return doc
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	size, prev := len(doc), 0
	for _, e := range edits {
		if e.start < prev || e.end > len(doc) || doc[e.start:e.end] != e.orig {
			return applySchemaEditsByPath(doc, edits)
		}
		size += len(e.repl) - len(e.orig)
		prev = e.end
	}
	var out strings.Builder
	out.Grow(size)
	prev = 0
	for _, e := range edits {
		out.WriteString(doc[prev:e.start])
		out.WriteString(e.repl)
		prev = e.end
	}
	out.WriteString(doc[prev:])
	return out.String()
}

func applySchemaEditsByPath(doc string, edits []schemaEdit) string {
	for _, e := range edits {
		if out, err := sjson.SetRaw(doc, e.path, e.repl); err == nil {
			doc = out
		}
	}
	return doc
}

// forEachMember calls fn with each member of object whose key is in keys,
// duplicates included.
func forEachMember(object gjson.Result, keys map[string]struct{}, fn func(key string, value gjson.Result)) {
	if !object.IsObject() {
		return
	}
	object.ForEach(func(key, value gjson.Result) bool {
		if name := key.String(); keyIn(keys, name) {
			fn(name, value)
		}
		return true
	})
}

// forEachElement calls fn with each element of array and its index.
func forEachElement(array gjson.Result, fn func(index int, value gjson.Result)) {
	if !array.IsArray() {
		return
	}
	index := 0
	array.ForEach(func(_, value gjson.Result) bool {
		fn(index, value)
		index++
		return true
	})
}

func keySet(keys ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		set[key] = struct{}{}
	}
	return set
}

func keyIn(set map[string]struct{}, key string) bool {
	_, ok := set[key]
	return ok
}
