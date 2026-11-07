package claude

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Anthropic streams a message as a series of content blocks: each is opened
// with content_block_start, filled with content_block_delta events and closed
// with content_block_stop before the next one opens, and its index is its
// place in the finished message. A Chat Completions stream has no blocks:
// reasoning, text and the arguments of several tool calls can arrive
// interleaved, each tool call keyed by its index.
//
// The translator keeps exactly one block open, streams into it as the
// upstream writes, and holds back whatever belongs to another block until
// that block's turn comes. Tool arguments go out as input_json_delta as they
// arrive, so a client watching a large tool call (a file being written) sees
// it grow instead of waiting for all of it at the end.
//
// A tool call's block can only close once its arguments are a complete JSON
// value: no event says a call is finished, and a closed block cannot be
// reopened. So text or another call that interleaves with an unfinished call
// waits, and goes out as soon as the call's arguments close, or when the
// upstream finishes.

type blockKind int

const (
	blockNone blockKind = iota
	blockThinking
	blockText
	blockTool
)

// heldBlock is a content block waiting for its turn: reasoning or text held
// back while another block was open, or a tool call by its OpenAI index,
// whose arguments wait in its ToolCallAccumulator.
type heldBlock struct {
	kind blockKind
	text strings.Builder
	tool int
}

// contentBlocks is the one open block and the blocks waiting behind it, in
// the order they arrived.
type contentBlocks struct {
	open  blockKind
	index int // the open block's Anthropic index
	tool  int // the open tool call's OpenAI index
	held  []*heldBlock
}

// toolArgsScanner follows a call's arguments as they stream, to tell when
// they form a complete JSON value and whether util.FixJSON has to rewrite
// them. It tracks single-quoted strings the way FixJSON reads them, so the
// brackets inside them are not counted.
type toolArgsScanner struct {
	depth    int
	started  bool
	inDouble bool
	inSingle bool
	escaped  bool
	// needsFix is set once a single-quoted string appears outside a
	// double-quoted one: FixJSON changes the text from there on, so the
	// rest of the arguments wait for the block to close.
	needsFix bool
}

func (s *toolArgsScanner) feed(chunk string) {
	for i := 0; i < len(chunk); i++ {
		c := chunk[i]
		switch {
		case s.inDouble || s.inSingle:
			quote := byte('"')
			if s.inSingle {
				quote = '\''
			}
			switch {
			case s.escaped:
				s.escaped = false
			case c == '\\':
				s.escaped = true
			case c == quote:
				s.inDouble, s.inSingle = false, false
			}
		case c == '"':
			s.inDouble = true
		case c == '\'':
			s.inSingle, s.needsFix = true, true
		case c == '{' || c == '[':
			s.depth++
			s.started = true
		case c == '}' || c == ']':
			if s.depth > 0 {
				s.depth--
			}
		}
	}
}

// complete reports whether the arguments so far form a whole object or
// array. Nothing can follow one in valid JSON.
func (s *toolArgsScanner) complete() bool {
	return s.started && s.depth == 0 && !s.inDouble && !s.inSingle
}

// unsentArguments returns the arguments not yet sent to the client, through
// util.FixJSON when they need it, and marks them sent.
func (a *ToolCallAccumulator) unsentArguments() string {
	full := a.Arguments.String()
	sent, rest := full[:a.emitted], full[a.emitted:]
	if a.args.needsFix {
		// FixJSON leaves standard JSON as it is, so what already went out is
		// the start of the fixed text.
		if fixed := util.FixJSON(full); strings.HasPrefix(fixed, sent) {
			rest = fixed[len(sent):]
		}
	}
	a.emitted = len(full)
	return rest
}

func (p *ConvertOpenAIResponseToAnthropicParams) deliverText(kind blockKind, text string, results *[]string) {
	if p.blocks.open == kind {
		*results = append(*results, textDeltaEvent(kind, p.blocks.index, text))
		return
	}
	var held *heldBlock
	if n := len(p.blocks.held); n > 0 && p.blocks.held[n-1].kind == kind {
		held = p.blocks.held[n-1]
	} else {
		held = &heldBlock{kind: kind}
		p.blocks.held = append(p.blocks.held, held)
	}
	held.text.WriteString(text)
	p.makeWay(results)
}

// deliverToolCall takes one tool_calls delta: it records the call's id, name
// and arguments, and streams the arguments if the call's block is open.
func (p *ConvertOpenAIResponseToAnthropicParams) deliverToolCall(toolCall gjson.Result, results *[]string) {
	if p.ToolCallsAccumulator == nil {
		p.ToolCallsAccumulator = make(map[int]*ToolCallAccumulator)
	}
	index := int(toolCall.Get("index").Int())
	acc, ok := p.ToolCallsAccumulator[index]
	if !ok {
		acc = &ToolCallAccumulator{BlockIndex: -1}
		p.ToolCallsAccumulator[index] = acc
	}
	if id := toolCall.Get("id"); id.Exists() {
		acc.ID = id.String()
	}
	function := toolCall.Get("function")
	if name := strings.TrimSpace(function.Get("name").String()); name != "" {
		acc.Name = name
	}
	if args := function.Get("arguments").String(); args != "" {
		acc.Arguments.WriteString(args)
		acc.args.feed(args)
	}

	switch {
	case acc.Name == "":
		// Not a call until it has a name: its arguments wait, and a call
		// that never gets one is dropped.
	case p.blocks.open == blockTool && p.blocks.tool == index:
		p.flushToolArguments(acc, results)
		if len(p.blocks.held) > 0 && acc.args.complete() {
			p.closeOpenBlock(results)
			p.openHeldBlocks(results)
		}
	case acc.Started || acc.held:
		// Closed already, or waiting its turn with its arguments kept.
	default:
		acc.held = true
		p.blocks.held = append(p.blocks.held, &heldBlock{kind: blockTool, tool: index})
		p.makeWay(results)
	}
}

// makeWay closes the open block if it is finished and opens the blocks
// waiting behind it.
func (p *ConvertOpenAIResponseToAnthropicParams) makeWay(results *[]string) {
	if p.blocks.open != blockNone && p.openBlockComplete() {
		p.closeOpenBlock(results)
	}
	p.openHeldBlocks(results)
}

func (p *ConvertOpenAIResponseToAnthropicParams) openHeldBlocks(results *[]string) {
	for p.blocks.open == blockNone && len(p.blocks.held) > 0 {
		next := p.blocks.held[0]
		p.blocks.held = p.blocks.held[1:]
		p.openBlock(next, results)
		if len(p.blocks.held) > 0 && p.openBlockComplete() {
			p.closeOpenBlock(results)
		}
	}
}

// closeAllBlocks closes the open block and sends every block still waiting,
// in order, when the upstream finishes.
func (p *ConvertOpenAIResponseToAnthropicParams) closeAllBlocks(results *[]string) {
	for {
		if p.blocks.open != blockNone {
			p.closeOpenBlock(results)
		}
		if len(p.blocks.held) == 0 {
			return
		}
		next := p.blocks.held[0]
		p.blocks.held = p.blocks.held[1:]
		p.openBlock(next, results)
	}
}

// openBlockComplete reports whether the open block can close without losing
// anything: reasoning and text can continue in a new block, a tool call
// cannot.
func (p *ConvertOpenAIResponseToAnthropicParams) openBlockComplete() bool {
	if p.blocks.open != blockTool {
		return true
	}
	return p.ToolCallsAccumulator[p.blocks.tool].args.complete()
}

func (p *ConvertOpenAIResponseToAnthropicParams) openBlock(held *heldBlock, results *[]string) {
	index := p.NextContentBlockIndex
	p.NextContentBlockIndex++
	p.blocks.open, p.blocks.index = held.kind, index
	if held.kind != blockTool {
		block := `{"type":"text","text":""}`
		if held.kind == blockThinking {
			block = `{"type":"thinking","thinking":""}`
		}
		*results = append(*results, contentBlockStartEvent(index, block))
		if held.text.Len() > 0 {
			*results = append(*results, textDeltaEvent(held.kind, index, held.text.String()))
		}
		return
	}
	acc := p.ToolCallsAccumulator[held.tool]
	p.blocks.tool = held.tool
	acc.Started, acc.held, acc.BlockIndex = true, false, index
	p.HasToolUse = true
	block := `{"type":"tool_use","id":"","name":"","input":{}}`
	block, _ = sjson.Set(block, "id", acc.ID)
	block, _ = sjson.Set(block, "name", acc.Name)
	*results = append(*results, contentBlockStartEvent(index, block))
	p.flushToolArguments(acc, results)
}

// flushToolArguments sends the open call's arguments that have not gone out
// yet, unless they need FixJSON, which can only run on all of them.
func (p *ConvertOpenAIResponseToAnthropicParams) flushToolArguments(acc *ToolCallAccumulator, results *[]string) {
	if acc.args.needsFix || acc.emitted == acc.Arguments.Len() {
		return
	}
	*results = append(*results, inputJSONDeltaEvent(p.blocks.index, acc.unsentArguments()))
}

func (p *ConvertOpenAIResponseToAnthropicParams) closeOpenBlock(results *[]string) {
	if p.blocks.open == blockTool {
		if rest := p.ToolCallsAccumulator[p.blocks.tool].unsentArguments(); rest != "" {
			*results = append(*results, inputJSONDeltaEvent(p.blocks.index, rest))
		}
	}
	stop := `{"type":"content_block_stop","index":0}`
	stop, _ = sjson.Set(stop, "index", p.blocks.index)
	*results = append(*results, "event: content_block_stop\ndata: "+stop+"\n\n")
	p.blocks.open = blockNone
}

func contentBlockStartEvent(index int, block string) string {
	start := `{"type":"content_block_start","index":0,"content_block":{}}`
	start, _ = sjson.Set(start, "index", index)
	start, _ = sjson.SetRaw(start, "content_block", block)
	return "event: content_block_start\ndata: " + start + "\n\n"
}

func textDeltaEvent(kind blockKind, index int, text string) string {
	delta := `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":""}}`
	field := "delta.text"
	if kind == blockThinking {
		delta = `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":""}}`
		field = "delta.thinking"
	}
	delta, _ = sjson.Set(delta, "index", index)
	delta, _ = sjson.Set(delta, field, text)
	return "event: content_block_delta\ndata: " + delta + "\n\n"
}

func inputJSONDeltaEvent(index int, partial string) string {
	delta := `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":""}}`
	delta, _ = sjson.Set(delta, "index", index)
	delta, _ = sjson.Set(delta, "delta.partial_json", partial)
	return "event: content_block_delta\ndata: " + delta + "\n\n"
}
