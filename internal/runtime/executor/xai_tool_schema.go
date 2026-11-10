package executor

import (
	"bytes"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Grok checks every function's parameters before it samples and rejects the
// whole request when the root is an anyOf/oneOf with a branch it cannot see is
// an object. grok-4.7 through cli-chat-proxy answered (2026-11-10,
// kittors/CliRelay#696):
//
//	400 invalid-argument: Failed to start sampling: [invalid_client_tool_schema]
//	codex_app__automation_update: tool parameter root must be an object type
//	(root schema is an anyOf/oneOf union with a non-object branch)
//
// A branch only counts when it says "type":"object" itself. Grok does not
// follow a $ref there, and an untyped branch or a nested union is a non-object
// branch too. The same session accepted an object root with object-only oneOf
// branches, an untyped root whose anyOf branches are all typed objects, and a
// $ref inside properties, so only the root union's branches are rewritten and
// $defs stays in place for the references inside them.
//
// zod v4's toJSONSchema emits the rejected shape (a oneOf of $refs into $defs),
// and Codex forwards MCP and Desktop dynamic tool schemas as they are, so a
// single such tool used to fail every turn of the session.

// xaiSchemaRewriteDepth bounds $ref chains and nested unions. A cycle ends as a
// plain object branch instead of recursing.
const xaiSchemaRewriteDepth = 8

var xaiSchemaUnionKeys = [...]string{"anyOf", "oneOf"}

// xaiObjectOnlyBranch is an object branch with nothing else to say. It stands
// in for branches that accept any value (true, {}) and for references that
// cannot be resolved; the tool still validates its own arguments.
var xaiObjectOnlyBranch = []byte(`{"type":"object"}`)

// xaiSchemaAnnotationKeys do not constrain a value, so a union that carries
// only these besides anyOf/oneOf can be flattened into its parent.
var xaiSchemaAnnotationKeys = map[string]struct{}{
	"$comment": {}, "$id": {}, "$schema": {}, "default": {}, "deprecated": {},
	"description": {}, "examples": {}, "readOnly": {}, "title": {}, "writeOnly": {},
}

type xaiSchemaObjectness int

const (
	xaiSchemaNeverObject xaiSchemaObjectness = iota
	xaiSchemaObjectOnly
	xaiSchemaObjectAmongOthers
)

// normalizeXAIToolParameterRoots rewrites the root anyOf/oneOf of each function
// tool's parameters so every branch is an explicit object schema. Requests
// whose roots Grok already accepts, which is nearly all of them, come back
// byte for byte.
func normalizeXAIToolParameterRoots(body []byte) []byte {
	if !bytes.Contains(body, []byte(`"anyOf"`)) && !bytes.Contains(body, []byte(`"oneOf"`)) {
		return body
	}
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body
	}
	for index, tool := range tools.Array() {
		if tool.Get("type").String() != "function" {
			continue
		}
		params := tool.Get("parameters")
		if !params.IsObject() {
			continue
		}
		fixed, changed := xaiNormalizeParametersRoot(params)
		if !changed {
			continue
		}
		updated, err := sjson.SetRawBytes(body, "tools."+strconv.Itoa(index)+".parameters", fixed)
		if err != nil {
			continue
		}
		log.Debugf("xai: rewrote the root union of tool %q into object branches", tool.Get("name").String())
		body = updated
	}
	return body
}

// xaiNormalizeParametersRoot returns the rewritten parameters, or false when
// every root union branch is already an explicit object.
func xaiNormalizeParametersRoot(params gjson.Result) ([]byte, bool) {
	raw := []byte(params.Raw)
	changed := false
	for _, key := range xaiSchemaUnionKeys {
		union := params.Get(key)
		if !union.IsArray() || xaiAllBranchesAreObjects(union) {
			continue
		}
		var branches [][]byte
		for _, branch := range union.Array() {
			branches = xaiAppendObjectBranches(branches, params, branch, 0)
		}
		var err error
		if len(branches) == 0 {
			raw, err = sjson.DeleteBytes(raw, key)
		} else {
			raw, err = sjson.SetRawBytes(raw, key, xaiJoinRawJSONArray(branches))
		}
		if err != nil {
			return nil, false
		}
		changed = true
	}
	if !changed {
		return nil, false
	}
	// Dropping every branch can leave a root that says nothing about its type,
	// which Grok rejects as well; function arguments are always an object.
	root := gjson.ParseBytes(raw)
	if !root.Get("type").Exists() && !root.Get("anyOf").Exists() && !root.Get("oneOf").Exists() {
		if updated, err := sjson.SetBytes(raw, "type", "object"); err == nil {
			raw = updated
		}
	}
	return raw, true
}

func xaiAllBranchesAreObjects(union gjson.Result) bool {
	for _, branch := range union.Array() {
		if !branch.IsObject() || xaiSchemaTypeObjectness(branch.Get("type")) != xaiSchemaObjectOnly {
			return false
		}
	}
	return true
}

// xaiAppendObjectBranches appends what branch becomes once every alternative in
// it is an explicit object schema; it can contribute none, one or several.
func xaiAppendObjectBranches(out [][]byte, root, branch gjson.Result, depth int) [][]byte {
	if depth > xaiSchemaRewriteDepth {
		return append(out, xaiObjectOnlyBranch)
	}
	switch {
	case branch.Type == gjson.True:
		return append(out, xaiObjectOnlyBranch)
	case !branch.IsObject():
		// false, or not a schema at all: matches no arguments.
		return out
	case len(branch.Map()) == 0:
		return append(out, xaiObjectOnlyBranch)
	}

	if schemaType := branch.Get("type"); schemaType.Exists() {
		switch xaiSchemaTypeObjectness(schemaType) {
		case xaiSchemaObjectOnly:
			return append(out, []byte(branch.Raw))
		case xaiSchemaObjectAmongOthers:
			// ["object","null"] and the like: arguments are never null.
			return append(out, xaiSetSchemaType(branch))
		default:
			// A branch that is never an object never matches function
			// arguments, which are always a JSON object.
			return out
		}
	}
	if ref := branch.Get("$ref"); ref.Exists() {
		// The referenced schema replaces the branch. Generators that emit
		// these unions (zod, pydantic) put nothing but the $ref there.
		target, ok := xaiLookupLocalSchemaRef(root, ref.String())
		if !ok {
			return append(out, xaiObjectOnlyBranch)
		}
		return xaiAppendObjectBranches(out, root, target, depth+1)
	}
	if key, ok := xaiBareUnionKey(branch); ok {
		// Nested unions flatten into the root one. For oneOf this only differs
		// when several alternatives match at once, which discriminated unions
		// (the source of these schemas) rule out.
		for _, child := range branch.Get(key).Array() {
			out = xaiAppendObjectBranches(out, root, child, depth+1)
		}
		return out
	}
	// Untyped, but constrains an object or nothing at all. Saying so changes
	// nothing for function arguments.
	return append(out, xaiSetSchemaType(branch))
}

func xaiSchemaTypeObjectness(schemaType gjson.Result) xaiSchemaObjectness {
	if schemaType.Type == gjson.String {
		if schemaType.String() == "object" {
			return xaiSchemaObjectOnly
		}
		return xaiSchemaNeverObject
	}
	if !schemaType.IsArray() {
		return xaiSchemaNeverObject
	}
	objects, others := 0, 0
	for _, item := range schemaType.Array() {
		if item.Type == gjson.String && item.String() == "object" {
			objects++
		} else {
			others++
		}
	}
	switch {
	case objects == 0:
		return xaiSchemaNeverObject
	case others == 0:
		return xaiSchemaObjectOnly
	default:
		return xaiSchemaObjectAmongOthers
	}
}

func xaiSetSchemaType(branch gjson.Result) []byte {
	fixed, err := sjson.SetBytes([]byte(branch.Raw), "type", "object")
	if err != nil {
		return xaiObjectOnlyBranch
	}
	return fixed
}

// xaiBareUnionKey reports the union keyword of a schema that is only a union:
// no type, no $ref and nothing but annotations next to exactly one of
// anyOf/oneOf. Anything else in it would be lost by flattening.
func xaiBareUnionKey(schema gjson.Result) (string, bool) {
	unionKey := ""
	bare := true
	schema.ForEach(func(key, _ gjson.Result) bool {
		name := key.String()
		if name == "anyOf" || name == "oneOf" {
			if unionKey != "" {
				bare = false
				return false
			}
			unionKey = name
			return true
		}
		if _, annotation := xaiSchemaAnnotationKeys[name]; !annotation {
			bare = false
			return false
		}
		return true
	})
	if !bare || unionKey == "" || !schema.Get(unionKey).IsArray() {
		return "", false
	}
	return unionKey, true
}

// xaiLookupLocalSchemaRef resolves a same-document JSON Pointer reference such
// as #/$defs/__schema0 against the parameters root. Remote references and
// pointers into nothing report false.
func xaiLookupLocalSchemaRef(root gjson.Result, ref string) (gjson.Result, bool) {
	if ref == "#" {
		return root, true
	}
	if !strings.HasPrefix(ref, "#/") {
		return gjson.Result{}, false
	}
	node := root
	for _, token := range strings.Split(ref[2:], "/") {
		token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
		next, ok := xaiSchemaChild(node, token)
		if !ok {
			return gjson.Result{}, false
		}
		node = next
	}
	return node, true
}

// xaiSchemaChild steps one pointer token into node. Keys are compared as
// written, so $defs names need no gjson path escaping.
func xaiSchemaChild(node gjson.Result, token string) (gjson.Result, bool) {
	if node.IsArray() {
		index, err := strconv.Atoi(token)
		items := node.Array()
		if err != nil || index < 0 || index >= len(items) {
			return gjson.Result{}, false
		}
		return items[index], true
	}
	if !node.IsObject() {
		return gjson.Result{}, false
	}
	var child gjson.Result
	found := false
	node.ForEach(func(key, value gjson.Result) bool {
		if key.String() == token {
			child, found = value, true
			return false
		}
		return true
	})
	return child, found
}
