package loopguard

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/sdk/loopdetect"
)

func replyLimits(window, distinct int) *loopdetect.ReplyLimits {
	l := loopdetect.ReplyLimits{Window: window, Distinct: distinct}
	return &l
}

func toolLimits(repeats int) *loopdetect.ToolLimits {
	l := loopdetect.ToolLimits{Repeats: repeats}
	return &l
}

func TestEffective(t *testing.T) {
	defaultReply := replyLimits(loopdetect.DefaultReplyWindow, loopdetect.DefaultReplyDistinct)
	defaultTool := toolLimits(loopdetect.DefaultToolRepeats)

	for name, tc := range map[string]struct {
		settings    Settings
		keyID       string
		reply       *loopdetect.ReplyLimits
		replySource Source
		tool        *loopdetect.ToolLimits
		toolSource  Source
	}{
		"nothing stored is everything off": {
			settings: Settings{},
			keyID:    "key-1",
		},
		"tenant switches both on with the defaults": {
			settings: Settings{Scope: Scope{
				Reply: ReplyRule{Enabled: Bool(true)},
				Tool:  ToolRule{Enabled: Bool(true)},
			}},
			keyID: "key-1",
			reply: defaultReply, replySource: SourceTenant,
			tool: defaultTool, toolSource: SourceTenant,
		},
		"tenant switches only the reply check on": {
			settings: Settings{Scope: Scope{Reply: ReplyRule{Enabled: Bool(true), Window: 512, MaxDistinct: 40}}},
			keyID:    "key-1",
			reply:    replyLimits(512, 40), replySource: SourceTenant,
		},
		"tenant thresholds without the switch stay off": {
			settings: Settings{Scope: Scope{
				Reply: ReplyRule{Window: 512, MaxDistinct: 40},
				Tool:  ToolRule{Repeats: 5},
			}},
			keyID: "key-1",
		},
		"a key exempts itself from a tenant that has the checks on": {
			settings: Settings{
				Scope: Scope{Reply: ReplyRule{Enabled: Bool(true)}, Tool: ToolRule{Enabled: Bool(true)}},
				Keys:  map[string]Scope{"key-1": {Reply: ReplyRule{Enabled: Bool(false)}}},
			},
			keyID: "key-1",
			tool:  defaultTool, toolSource: SourceTenant,
		},
		"a key turns a check on under a tenant that has it off": {
			settings: Settings{Keys: map[string]Scope{"key-1": {Tool: ToolRule{Enabled: Bool(true), Repeats: 12}}}},
			keyID:    "key-1",
			tool:     toolLimits(12), toolSource: SourceAPIKey,
		},
		"a key that sets nothing follows the tenant": {
			settings: Settings{
				Scope: Scope{Reply: ReplyRule{Enabled: Bool(true), Window: 512}},
				Keys:  map[string]Scope{"key-1": {}},
			},
			keyID: "key-1",
			reply: replyLimits(512, loopdetect.DefaultReplyDistinct), replySource: SourceTenant,
		},
		"a key overrides one threshold and follows the tenant on the other": {
			settings: Settings{
				Scope: Scope{Reply: ReplyRule{Enabled: Bool(true), Window: 512, MaxDistinct: 40}},
				Keys:  map[string]Scope{"key-1": {Reply: ReplyRule{MaxDistinct: 60}}},
			},
			keyID: "key-1",
			reply: replyLimits(512, 60), replySource: SourceTenant,
		},
		"a key's own switch and thresholds": {
			settings: Settings{
				Scope: Scope{Reply: ReplyRule{Enabled: Bool(true), Window: 512, MaxDistinct: 40}},
				Keys:  map[string]Scope{"key-1": {Reply: ReplyRule{Enabled: Bool(true), Window: 128, MaxDistinct: 8}}},
			},
			keyID: "key-1",
			reply: replyLimits(128, 8), replySource: SourceAPIKey,
		},
		"another key's override does not apply": {
			settings: Settings{
				Scope: Scope{Tool: ToolRule{Enabled: Bool(true)}},
				Keys:  map[string]Scope{"key-2": {Tool: ToolRule{Enabled: Bool(false)}}},
			},
			keyID: "key-1",
			tool:  defaultTool, toolSource: SourceTenant,
		},
		"a request without a key id follows the tenant": {
			settings: Settings{
				Scope: Scope{Tool: ToolRule{Enabled: Bool(true)}},
				Keys:  map[string]Scope{"": {Tool: ToolRule{Enabled: Bool(false)}}},
			},
			keyID: "",
			tool:  defaultTool, toolSource: SourceTenant,
		},
		"thresholds out of their bounds are held to them": {
			settings: Settings{Scope: Scope{
				Reply: ReplyRule{Enabled: Bool(true), Window: 100000, MaxDistinct: 100000},
				Tool:  ToolRule{Enabled: Bool(true), Repeats: 1000},
			}},
			keyID: "key-1",
			reply: replyLimits(loopdetect.MaxReplyWindow, loopdetect.MaxReplyWindow/4), replySource: SourceTenant,
			tool: toolLimits(loopdetect.MaxToolRepeats), toolSource: SourceTenant,
		},
		"a distinct threshold wider than the window allows is held to a quarter of it": {
			settings: Settings{
				Scope: Scope{Reply: ReplyRule{Enabled: Bool(true), MaxDistinct: 60}},
				Keys:  map[string]Scope{"key-1": {Reply: ReplyRule{Window: 64}}},
			},
			keyID: "key-1",
			reply: replyLimits(64, 16), replySource: SourceTenant,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := tc.settings.Effective(tc.keyID)
			if !reflect.DeepEqual(got.Reply, tc.reply) {
				t.Fatalf("reply = %+v, want %+v", got.Reply, tc.reply)
			}
			if !reflect.DeepEqual(got.Tool, tc.tool) {
				t.Fatalf("tool = %+v, want %+v", got.Tool, tc.tool)
			}
			if got.Reply != nil && got.ReplySource != tc.replySource {
				t.Fatalf("reply source = %q, want %q", got.ReplySource, tc.replySource)
			}
			if got.Tool != nil && got.ToolSource != tc.toolSource {
				t.Fatalf("tool source = %q, want %q", got.ToolSource, tc.toolSource)
			}
			if want := tc.reply != nil || tc.tool != nil; got.Any() != want {
				t.Fatalf("Any() = %v, want %v", got.Any(), want)
			}
		})
	}
}

func TestSanitized(t *testing.T) {
	in := Settings{
		Scope: Scope{Reply: ReplyRule{Window: -4, MaxDistinct: -1}, Tool: ToolRule{Repeats: -9}},
		Keys: map[string]Scope{
			"empty":    {},
			"negative": {Reply: ReplyRule{Window: -1}},
			"":         {Tool: ToolRule{Enabled: Bool(true)}},
			"kept":     {Tool: ToolRule{Enabled: Bool(false), Repeats: 5}},
		},
	}
	want := Settings{Keys: map[string]Scope{"kept": {Tool: ToolRule{Enabled: Bool(false), Repeats: 5}}}}
	got := in.Sanitized()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Sanitized() = %+v, want %+v", got, want)
	}
	// The input is not edited in place: a cached Settings is shared.
	if len(in.Keys) != 4 || in.Reply.Window != -4 {
		t.Fatalf("Sanitized edited its receiver: %+v", in)
	}
	// Nothing left in the map is stored as a missing map, not an empty one.
	if got := (Settings{Keys: map[string]Scope{"x": {}}}).Sanitized(); got.Keys != nil {
		t.Fatalf("empty overrides left a map behind: %+v", got.Keys)
	}
}

func TestCloneSharesNoMap(t *testing.T) {
	a := Settings{Keys: map[string]Scope{"k": {Tool: ToolRule{Repeats: 4}}}}
	b := a.Clone()
	b.Keys["k"] = Scope{Tool: ToolRule{Repeats: 9}}
	b.Keys["other"] = Scope{}
	if a.Keys["k"].Tool.Repeats != 4 || len(a.Keys) != 1 {
		t.Fatalf("Clone shares its map: %+v", a.Keys)
	}
}

func TestScopeValidate(t *testing.T) {
	for name, tc := range map[string]struct {
		scope   Scope
		invalid bool
	}{
		"empty":                {Scope{}, false},
		"valid reply":          {Scope{Reply: ReplyRule{Window: 512, MaxDistinct: 100}}, false},
		"valid tool":           {Scope{Tool: ToolRule{Repeats: 3}}, false},
		"window out of bounds": {Scope{Reply: ReplyRule{Window: 10}}, true},
		"distinct too wide":    {Scope{Reply: ReplyRule{Window: 64, MaxDistinct: 17}}, true},
		"repeats out of bound": {Scope{Tool: ToolRule{Repeats: 2}}, true},
		"a switch alone":       {Scope{Reply: ReplyRule{Enabled: Bool(true)}, Tool: ToolRule{Enabled: Bool(false)}}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if err := tc.scope.Validate(); (err != nil) != tc.invalid {
				t.Fatalf("Validate() = %v, invalid = %v", err, tc.invalid)
			}
		})
	}
}

// The stored form is the API form: the tenant's own rules sit at the top level
// and the key overrides under "keys", and a setting unset is absent.
func TestSettingsJSONShape(t *testing.T) {
	in := Settings{
		Scope: Scope{Reply: ReplyRule{Enabled: Bool(true), Window: 512}},
		Keys:  map[string]Scope{"key-1": {Tool: ToolRule{Enabled: Bool(false)}}},
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"reply":{"enabled":true,"window":512},"tool":{},"keys":{"key-1":{"reply":{},"tool":{"enabled":false}}}}`
	if string(data) != want {
		t.Fatalf("json = %s\nwant   %s", data, want)
	}
	var out Settings
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round trip = %+v, want %+v", out, in)
	}
	// A row written by a later build with fields this one does not know is read,
	// not refused.
	var future Settings
	if err := json.Unmarshal([]byte(`{"reply":{"enabled":true,"mode":"strict"},"tool":{},"extra":1}`), &future); err != nil {
		t.Fatalf("unknown fields refused: %v", err)
	}
	if future.Reply.Enabled == nil || !*future.Reply.Enabled {
		t.Fatalf("known fields lost beside unknown ones: %+v", future)
	}
}
