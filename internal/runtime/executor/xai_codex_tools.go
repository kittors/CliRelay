package executor

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Codex sends MCP servers, Desktop plugins and some built-in tool groups as
// Responses namespace tools, {"type":"namespace","name":"mcp__exa","tools":[...]}.
// xAI's Responses API only knows flat tool variants and rejects the whole
// request with 422 "unknown variant `namespace`" (kittors/CliRelay#696).
// Codex dispatches a call on its name plus namespace, so flattening is only
// half the job: every name invented here has to map back on the way out.
const (
	xaiNamespaceToolType = "namespace"
	// xaiDefaultToolNamespace is Codex's DEFAULT_FUNCTION_NAMESPACE. Codex treats
	// functions::exec and a plain exec as the same tool, so children of this
	// namespace are plain tools and need neither a prefix nor a way back.
	xaiDefaultToolNamespace = "functions"
	xaiToolNameDelimiter    = "__"
	// xAI documents no limit on function names. 64 is the OpenAI-compatible
	// limit (^[a-zA-Z0-9_-]{1,64}$) and the tightest one a Grok endpoint can be
	// expected to enforce; Codex itself allows 128 for namespace plus name, so
	// a long MCP name can arrive that is valid for Codex and too long here.
	xaiMaxToolNameLength = 64
	xaiToolNameHashChars = 8
)

// xaiUnsupportedWebSearchOptions are web_search options Codex sends that xAI's
// Responses API does not take. Codex 0.166.0 puts external_web_access on every
// request; CLIProxyAPI's xAI executor drops both after xAI answered them with
// "Argument not supported". The web_search Claude Code requests translate to
// carries neither, so only Codex-shaped requests change.
var xaiUnsupportedWebSearchOptions = []string{"external_web_access", "search_context_size"}

// xaiNamespacedTool is the namespace and child name Codex declared a tool under.
type xaiNamespacedTool struct {
	namespace string
	name      string
}

// xaiFlatToolRef is one flattened tool, kept to expand an allowed_tools entry
// that names a whole namespace.
type xaiFlatToolRef struct {
	toolType string
	name     string
}

// xaiNamespaceToolNames maps the flat names sent to xAI in one request back to
// the namespaced tools they stand for.
type xaiNamespaceToolNames struct {
	byWire map[string]xaiNamespacedTool
	byTool map[xaiNamespacedTool]string
	// taken holds every name already used in the outbound tool list: plain
	// tools keep theirs, so a flattened name must never shadow one of them.
	taken map[string]struct{}
}

func newXAINamespaceToolNames(taken map[string]struct{}) *xaiNamespaceToolNames {
	return &xaiNamespaceToolNames{
		byWire: make(map[string]xaiNamespacedTool),
		byTool: make(map[xaiNamespacedTool]string),
		taken:  taken,
	}
}

func (n *xaiNamespaceToolNames) empty() bool {
	return n == nil || len(n.byWire) == 0
}

// wireName returns the flat name for a namespaced child, assigning one on
// first use. Names are unique within the request and fit the upstream limit;
// the mapping, not the name itself, is what makes them reversible.
func (n *xaiNamespaceToolNames) wireName(namespace, name string) string {
	key := xaiNamespacedTool{namespace: namespace, name: name}
	if wire, ok := n.byTool[key]; ok {
		return wire
	}
	base := xaiQualifiedToolName(namespace, name)
	wire := base
	for attempt := 0; len(wire) > xaiMaxToolNameLength || n.isTaken(wire); attempt++ {
		wire = xaiShortenToolName(base, key, attempt)
	}
	if wire != base {
		log.Debugf("xai: namespaced tool %s/%s sent as %s", namespace, name, wire)
	}
	n.byWire[wire] = key
	n.byTool[key] = wire
	n.taken[wire] = struct{}{}
	return wire
}

func (n *xaiNamespaceToolNames) isTaken(name string) bool {
	_, ok := n.taken[name]
	return ok
}

// xaiQualifiedToolName joins a namespace and child the way Codex named MCP
// tools before namespaces existed (mcp__server__tool), which models already
// know. A child that already carries the prefix is not prefixed twice. This is
// only a candidate: wireName still checks it against every other name, since
// two different tools can qualify to the same string.
func xaiQualifiedToolName(namespace, name string) string {
	prefix := namespace
	if !strings.HasSuffix(prefix, xaiToolNameDelimiter) {
		prefix += xaiToolNameDelimiter
	}
	if strings.HasPrefix(name, prefix) {
		return xaiSanitizeToolName(name)
	}
	return xaiSanitizeToolName(prefix + name)
}

// xaiSanitizeToolName replaces every byte outside [A-Za-z0-9_-]. Codex already
// sanitizes namespace and name separately; other clients may not.
func xaiSanitizeToolName(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('_')
	}
	return b.String()
}

// xaiShortenToolName keeps as much of the readable name as fits and appends a
// hash of the original namespace and child, so two long names that share a
// prefix still differ. attempt only changes the hash when an earlier candidate
// was taken.
func xaiShortenToolName(base string, key xaiNamespacedTool, attempt int) string {
	seed := key.namespace + "\x00" + key.name
	if attempt > 0 {
		seed += "\x00" + strconv.Itoa(attempt)
	}
	sum := sha256.Sum256([]byte(seed))
	suffix := "_" + hex.EncodeToString(sum[:])[:xaiToolNameHashChars]
	if keep := xaiMaxToolNameLength - len(suffix); len(base) > keep {
		base = base[:keep]
	}
	return base + suffix
}

func xaiIsDefaultToolNamespace(namespace string) bool {
	namespace = strings.TrimSpace(namespace)
	return namespace == "" || namespace == xaiDefaultToolNamespace
}

func xaiIsNamespaceChildType(toolType string) bool {
	return toolType == "function" || toolType == "custom"
}

// flattenXAINamespaceTools rewrites the Codex tool shapes xAI rejects and
// returns the names it invented, or nil when it had nothing to rename.
// Requests without namespaces or Lite tools (Claude Code, Chat Completions,
// plain Responses) come back byte for byte.
//
//   - namespace children become top-level tools named <namespace>__<child>;
//   - Responses Lite tools move from input[].additional_tools to top-level
//     tools, because xAI has no such input item either;
//   - history calls and a tool_choice that carry name plus namespace are
//     renamed the same way, so the model sees one name per tool.
func flattenXAINamespaceTools(body []byte) ([]byte, *xaiNamespaceToolNames) {
	if !bytes.Contains(body, []byte(`"namespace"`)) && !bytes.Contains(body, []byte(`"additional_tools"`)) {
		return body, nil
	}

	var declared []gjson.Result
	if tools := gjson.GetBytes(body, "tools"); tools.IsArray() {
		declared = tools.Array()
	}
	topLevelCount := len(declared)
	var inputItems []gjson.Result
	if input := gjson.GetBytes(body, "input"); input.IsArray() {
		inputItems = input.Array()
	}
	liftedLite := false
	for _, item := range inputItems {
		if item.Get("type").String() == "additional_tools" {
			liftedLite = true
			declared = append(declared, item.Get("tools").Array()...)
		}
	}

	hasNamespace := false
	taken := make(map[string]struct{})
	for _, tool := range declared {
		if tool.Get("type").String() != xaiNamespaceToolType {
			if name := tool.Get("name").String(); name != "" {
				taken[name] = struct{}{}
			}
			continue
		}
		hasNamespace = true
		if xaiIsDefaultToolNamespace(tool.Get("name").String()) {
			for _, child := range tool.Get("tools").Array() {
				if name := child.Get("name").String(); name != "" {
					taken[name] = struct{}{}
				}
			}
		}
	}
	names := newXAINamespaceToolNames(taken)

	sets := make(map[string][]byte, 3)
	children := make(map[string][]xaiFlatToolRef)
	if hasNamespace || liftedLite {
		sets["tools"] = xaiFlattenToolList(declared, topLevelCount, names, children)
	}
	if input, changed := xaiFlattenInputItems(inputItems, names); changed {
		sets["input"] = input
	}
	if choice, changed := xaiFlattenToolChoice(gjson.GetBytes(body, "tool_choice"), names, children); changed {
		sets["tool_choice"] = choice
	}
	if len(sets) == 0 {
		return body, nil
	}
	body = util.MutateTopLevelObject(body, sets, nil)
	if names.empty() {
		return body, nil
	}
	log.Debugf("xai: flattened %d namespaced tool name(s) for xAI", len(names.byWire))
	return body, names
}

// xaiFlattenToolList builds the outbound tool list. declared holds the
// top-level tools first and lifted Lite tools after them, so top-level
// declarations win when both carry the same tool.
func xaiFlattenToolList(declared []gjson.Result, topLevelCount int, names *xaiNamespaceToolNames, children map[string][]xaiFlatToolRef) []byte {
	out := make([][]byte, 0, len(declared))
	seenNames := make(map[string]struct{}, len(declared))
	seenHosted := make(map[string]struct{})
	for index, tool := range declared {
		toolType := tool.Get("type").String()
		if toolType != xaiNamespaceToolType {
			if name := tool.Get("name").String(); name != "" {
				if _, dup := seenNames[name]; dup {
					continue
				}
				seenNames[name] = struct{}{}
			} else {
				// A hosted tool (web_search, ...) has no name. Lite can repeat
				// its tool list, so a lifted copy of a declared type is dropped.
				if _, dup := seenHosted[toolType]; dup && index >= topLevelCount {
					continue
				}
				seenHosted[toolType] = struct{}{}
			}
			out = append(out, []byte(tool.Raw))
			continue
		}

		namespace := tool.Get("name").String()
		for _, child := range tool.Get("tools").Array() {
			childType := child.Get("type").String()
			childName := child.Get("name").String()
			// Codex namespaces only hold function and custom (freeform) tools.
			// A custom child stays custom: whether xAI takes custom tools is a
			// separate question from the namespace container this resolves.
			if !xaiIsNamespaceChildType(childType) || strings.TrimSpace(childName) == "" {
				log.Debugf("xai: dropped unsupported %q tool %q from namespace %q", childType, childName, namespace)
				continue
			}
			if xaiIsDefaultToolNamespace(namespace) {
				if _, dup := seenNames[childName]; dup {
					continue
				}
				seenNames[childName] = struct{}{}
				children[namespace] = append(children[namespace], xaiFlatToolRef{toolType: childType, name: childName})
				out = append(out, []byte(child.Raw))
				continue
			}
			key := xaiNamespacedTool{namespace: namespace, name: childName}
			if _, dup := names.byTool[key]; dup {
				continue
			}
			wire := names.wireName(namespace, childName)
			raw, err := sjson.SetBytes([]byte(child.Raw), "name", wire)
			if err != nil {
				continue
			}
			seenNames[wire] = struct{}{}
			children[namespace] = append(children[namespace], xaiFlatToolRef{toolType: childType, name: wire})
			out = append(out, raw)
		}
	}
	return xaiJoinRawJSONArray(out)
}

// xaiFlattenInputItems drops the Lite additional_tools items (their tools are
// already in the outbound list) and renames history calls that carry a
// namespace, which xAI does not accept on a call either.
func xaiFlattenInputItems(items []gjson.Result, names *xaiNamespaceToolNames) ([]byte, bool) {
	out := make([][]byte, 0, len(items))
	changed := false
	for _, item := range items {
		switch item.Get("type").String() {
		case "additional_tools":
			changed = true
			continue
		case "function_call", "custom_tool_call":
			if raw, renamed := xaiFlattenToolReference(item, names); renamed {
				out = append(out, raw)
				changed = true
				continue
			}
		}
		out = append(out, []byte(item.Raw))
	}
	if !changed {
		return nil, false
	}
	return xaiJoinRawJSONArray(out), true
}

// xaiFlattenToolChoice renames a forced namespaced tool, and expands an
// allowed_tools entry that names a whole namespace into its flattened tools.
func xaiFlattenToolChoice(choice gjson.Result, names *xaiNamespaceToolNames, children map[string][]xaiFlatToolRef) ([]byte, bool) {
	if !choice.IsObject() {
		return nil, false
	}
	switch choice.Get("type").String() {
	case "function", "custom":
		return xaiFlattenToolReference(choice, names)
	case "allowed_tools":
		entries := choice.Get("tools")
		if !entries.IsArray() {
			return nil, false
		}
		out := make([][]byte, 0, len(entries.Array()))
		changed := false
		for _, entry := range entries.Array() {
			if entry.Get("type").String() == xaiNamespaceToolType {
				changed = true
				for _, child := range children[entry.Get("name").String()] {
					ref, _ := sjson.SetBytes([]byte(`{}`), "type", child.toolType)
					ref, _ = sjson.SetBytes(ref, "name", child.name)
					out = append(out, ref)
				}
				continue
			}
			if raw, renamed := xaiFlattenToolReference(entry, names); renamed {
				out = append(out, raw)
				changed = true
				continue
			}
			out = append(out, []byte(entry.Raw))
		}
		if !changed {
			return nil, false
		}
		updated, err := sjson.SetRawBytes([]byte(choice.Raw), "tools", xaiJoinRawJSONArray(out))
		if err != nil {
			return nil, false
		}
		return updated, true
	}
	return nil, false
}

// xaiFlattenToolReference turns a {"name","namespace"} reference (a history
// call or a tool_choice entry) into the flat name sent in tools.
func xaiFlattenToolReference(ref gjson.Result, names *xaiNamespaceToolNames) ([]byte, bool) {
	namespace := ref.Get("namespace")
	if !namespace.Exists() {
		return nil, false
	}
	raw := []byte(ref.Raw)
	if name := ref.Get("name").String(); !xaiIsDefaultToolNamespace(namespace.String()) && strings.TrimSpace(name) != "" {
		updated, err := sjson.SetBytes(raw, "name", names.wireName(namespace.String(), name))
		if err != nil {
			return nil, false
		}
		raw = updated
	}
	updated, err := sjson.DeleteBytes(raw, "namespace")
	if err != nil {
		return nil, false
	}
	return updated, true
}

func xaiJoinRawJSONArray(items [][]byte) []byte {
	size := 2
	for _, item := range items {
		size += len(item) + 1
	}
	var b bytes.Buffer
	b.Grow(size)
	b.WriteByte('[')
	for index, item := range items {
		if index > 0 {
			b.WriteByte(',')
		}
		b.Write(item)
	}
	b.WriteByte(']')
	return b.Bytes()
}

// restoreEvent hands calls to flattened tools back under the namespace and
// child name Codex declared. Calls to any other tool, and every event without
// one, pass through unchanged.
func (n *xaiNamespaceToolNames) restoreEvent(payload []byte) ([]byte, bool) {
	if n.empty() || len(payload) == 0 {
		return payload, false
	}
	switch gjson.GetBytes(payload, "type").String() {
	case "response.output_item.added", "response.output_item.done":
		return n.restoreCallAt(payload, "item")
	case "response.function_call_arguments.done":
		// The event repeats the call's name; keep it in step with the item.
		tool, ok := n.byWire[gjson.GetBytes(payload, "name").String()]
		if !ok {
			return payload, false
		}
		return xaiSetToolCallName(payload, "", tool)
	case "response.created", "response.in_progress", "response.completed", "response.incomplete", "response.failed":
		output := gjson.GetBytes(payload, "response.output")
		if !output.IsArray() {
			return payload, false
		}
		changed := false
		for index, item := range output.Array() {
			tool, ok := n.restorableCall(item)
			if !ok {
				continue
			}
			updated, restored := xaiSetToolCallName(payload, "response.output."+strconv.Itoa(index), tool)
			payload = updated
			changed = changed || restored
		}
		return payload, changed
	}
	return payload, false
}

func (n *xaiNamespaceToolNames) restoreCallAt(payload []byte, path string) ([]byte, bool) {
	tool, ok := n.restorableCall(gjson.GetBytes(payload, path))
	if !ok {
		return payload, false
	}
	return xaiSetToolCallName(payload, path, tool)
}

func (n *xaiNamespaceToolNames) restorableCall(item gjson.Result) (xaiNamespacedTool, bool) {
	switch item.Get("type").String() {
	case "function_call", "custom_tool_call":
		tool, ok := n.byWire[item.Get("name").String()]
		return tool, ok
	}
	return xaiNamespacedTool{}, false
}

func xaiSetToolCallName(payload []byte, path string, tool xaiNamespacedTool) ([]byte, bool) {
	prefix := ""
	if path != "" {
		prefix = path + "."
	}
	updated, err := sjson.SetBytes(payload, prefix+"name", tool.name)
	if err != nil {
		return payload, false
	}
	updated, err = sjson.SetBytes(updated, prefix+"namespace", tool.namespace)
	if err != nil {
		return payload, false
	}
	return updated, true
}

// restoreSSELine applies restoreEvent to one data line of the upstream stream.
func (n *xaiNamespaceToolNames) restoreSSELine(line []byte) []byte {
	if n.empty() || !bytes.HasPrefix(line, dataTag) {
		return line
	}
	payload, changed := n.restoreEvent(bytes.TrimSpace(line[len(dataTag):]))
	if !changed {
		return line
	}
	out := make([]byte, 0, len(dataTag)+1+len(payload))
	out = append(out, dataTag...)
	out = append(out, ' ')
	return append(out, payload...)
}

// stripXAIUnsupportedWebSearchOptions removes the web_search options in
// xaiUnsupportedWebSearchOptions and leaves the tool itself in place.
func stripXAIUnsupportedWebSearchOptions(body []byte) []byte {
	if !bytes.Contains(body, []byte("external_web_access")) && !bytes.Contains(body, []byte("search_context_size")) {
		return body
	}
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body
	}
	for index, tool := range tools.Array() {
		if tool.Get("type").String() != "web_search" {
			continue
		}
		for _, option := range xaiUnsupportedWebSearchOptions {
			if !tool.Get(option).Exists() {
				continue
			}
			if updated, err := sjson.DeleteBytes(body, "tools."+strconv.Itoa(index)+"."+option); err == nil {
				body = updated
			}
		}
	}
	return body
}
