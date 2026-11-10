package contentmoderation

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

// A body the guards cannot read the way the upstream will is refused by each
// of them, in every format (see extraction.run). The request stages refuse it
// first; these are the guards' own checks.

type unreadableBody struct {
	name   string
	format sdktranslator.Format
	body   string
}

// guardedBodies are, per format, a clean request with text in it, and the
// bodies the guards cannot read: the conversation repeated with the text only
// in the second copy (gjson reads the first, the upstream may read the last),
// a field repeated inside one turn, and the text with a NaN beside it (gjson
// reads nothing of it, Python's json all of it).
func guardedBodies(text string) (clean, unreadable []unreadableBody) {
	q := quoteJSON(text)
	for _, f := range []struct {
		format                      sdktranslator.Format
		wrap                        func(string) string
		turn, twice, nested, number string
	}{
		{sdktranslator.FormatOpenAI, func(c string) string { return `{"model":"m",` + c + `}` },
			`"messages":[{"role":"user","content":` + q + `}]`,
			`"messages":[{"role":"user","content":"hi"}],"messages":[{"role":"user","content":` + q + `}]`,
			`"messages":[{"role":"user","content":"hi","content":` + q + `}]`,
			`"temperature":NaN`},
		{sdktranslator.FormatOpenAIResponse, func(c string) string { return `{"model":"m",` + c + `}` },
			`"input":` + q,
			`"input":"hi","input":` + q,
			`"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi","text":` + q + `}]}]`,
			`"temperature":Infinity`},
		{sdktranslator.FormatClaude, func(c string) string { return `{"model":"m","max_tokens":8,` + c + `}` },
			`"messages":[{"role":"user","content":` + q + `}]`,
			`"messages":[{"role":"user","content":"hi"}],"messages":[{"role":"user","content":` + q + `}]`,
			`"system":"hi","system":` + q + `,"messages":[{"role":"user","content":"hi"}]`,
			`"temperature":NaN`},
		{sdktranslator.FormatGemini, func(c string) string { return `{` + c + `}` },
			`"contents":[{"role":"user","parts":[{"text":` + q + `}]}]`,
			`"contents":[{"role":"user","parts":[{"text":"hi"}]}],"contents":[{"role":"user","parts":[{"text":` + q + `}]}]`,
			`"contents":[{"role":"user","parts":[{"text":"hi","text":` + q + `}]}]`,
			`"generationConfig":{"temperature":NaN}`},
		{sdktranslator.FormatGeminiCLI, func(c string) string { return `{"model":"m",` + c + `}` },
			`"request":{"contents":[{"role":"user","parts":[{"text":` + q + `}]}]}`,
			`"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]},"request":{"contents":[{"role":"user","parts":[{"text":` + q + `}]}]}`,
			`"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"contents":[{"role":"user","parts":[{"text":` + q + `}]}]}`,
			`"request":{"generationConfig":{"temperature":NaN},"contents":[{"role":"user","parts":[{"text":` + q + `}]}]}`},
	} {
		name := f.format.String()
		clean = append(clean, unreadableBody{name, f.format, f.wrap(f.turn)})
		number := f.wrap(f.number + "," + f.turn)
		if f.format == sdktranslator.FormatGeminiCLI {
			number = f.wrap(f.number)
		}
		unreadable = append(unreadable,
			unreadableBody{name + " repeated conversation", f.format, f.wrap(f.twice)},
			unreadableBody{name + " repeated field in a turn", f.format, f.wrap(f.nested)},
			unreadableBody{name + " number gjson cannot parse", f.format, number},
		)
	}
	return clean, unreadable
}

func guardBody(format sdktranslator.Format, body string) (cliproxyexecutor.Request, cliproxyexecutor.Options) {
	req, opts := guardOptions("tenant-a", []byte(body))
	opts.SourceFormat = format
	return req, opts
}

func TestWordGuardRefusesABodyItCannotRead(t *testing.T) {
	moderator := guardModerator(&wordResolverStub{policies: WordPolicies{Tenant: tenantOf(t, WordActionBlock, kw("forbidden"))}})
	clean, unreadable := guardedBodies("say forbidden things")
	for _, tc := range clean {
		req, opts := guardBody(tc.format, tc.body)
		if result := moderator.GuardRequest(context.Background(), req, opts); !result.Blocked || result.HTTPStatus != DefaultBlockStatus {
			t.Fatalf("%s: the policy did not apply to a body it can read: %+v", tc.name, result)
		}
	}
	for _, tc := range unreadable {
		req, opts := guardBody(tc.format, tc.body)
		ctx, ginCtx := snapshotContext(t)
		result := moderator.GuardRequest(ctx, req, opts)
		if !result.Blocked || result.HTTPStatus != http.StatusBadRequest || result.Message != UnreadableBodyMessage {
			t.Errorf("%s: %+v", tc.name, result)
		}
		snapshot, _ := WordGuardSnapshotFromGin(ginCtx)
		if raw, _ := json.Marshal(snapshot); gjson.GetBytes(raw, "reason").String() != WordReasonUnreadable {
			t.Errorf("%s: snapshot = %s", tc.name, raw)
		}
	}
}

func TestMaskingRefusesABodyItCannotRead(t *testing.T) {
	moderator := maskingModerator(t, "tenant-a")
	clean, unreadable := guardedBodies("deploy with " + testOpenAIKey)
	for _, tc := range clean {
		req, opts := guardBody(tc.format, tc.body)
		result := moderator.GuardRequest(context.Background(), req, opts)
		if result.Blocked || result.Payload == nil || bytes.Contains(result.Payload, []byte(testOpenAIKey)) {
			t.Fatalf("%s: a body masking can read was not masked: %+v", tc.name, result)
		}
	}
	for _, tc := range unreadable {
		req, opts := guardBody(tc.format, tc.body)
		ctx, ginCtx := snapshotContext(t)
		result := moderator.GuardRequest(ctx, req, opts)
		if !result.Blocked || result.HTTPStatus != http.StatusBadRequest || result.Message != UnreadableBodyMessage || result.Payload != nil {
			t.Errorf("%s: %+v", tc.name, result)
		}
		if snapshot, ok := SecretMaskingSnapshotFromGin(ginCtx); !ok || snapshot.Refused != maskFailureUnreadable {
			t.Errorf("%s: snapshot = %+v", tc.name, snapshot)
		}
	}
}

func TestModerationRefusesABodyItCannotRead(t *testing.T) {
	const tenantID = "tenant-unreadable"
	store := newTestStore(t)
	profile := createRuntimeProfile(t, store, tenantID, "profile-unreadable", CreateProfileInput{
		Name: "keywords", Mode: ModePreBlock, KeywordMode: KeywordModeKeywordOnly, BlockedKeywords: []string{"forbidden"},
	})
	bindRuntimeChannel(t, store, tenantID, ChannelTypeProviderKey, "unreadable-key", profile.ID)
	moderator := NewRequestModerator(store, NewEvaluator(nil))
	auth := &coreauth.Auth{ID: "unreadable-auth", TenantID: tenantID, Provider: "moderation-runtime", Attributes: map[string]string{"provider_key_id": "unreadable-key"}}
	options := func(format sdktranslator.Format, body string) cliproxyexecutor.Options {
		return cliproxyexecutor.Options{OriginalRequest: []byte(body), SourceFormat: format, Metadata: map[string]any{cliproxyexecutor.TenantMetadataKey: tenantID}}
	}
	clean, unreadable := guardedBodies("say forbidden things")
	for _, tc := range clean {
		if result := moderator.Moderate(context.Background(), auth, options(tc.format, tc.body)); !result.Blocked {
			t.Fatalf("%s: the profile did not apply to a body it can read", tc.name)
		}
	}
	for _, tc := range unreadable {
		result := moderator.Moderate(context.Background(), auth, options(tc.format, tc.body))
		if !result.Blocked || result.HTTPStatus != http.StatusBadRequest || result.Message != UnreadableBodyMessage {
			t.Errorf("%s: %+v", tc.name, result)
		}
	}
	// An empty body has nothing in it to read, as before.
	if result := moderator.Moderate(context.Background(), auth, options(sdktranslator.FormatOpenAI, "")); result.Blocked {
		t.Fatalf("an empty body was refused: %+v", result)
	}
}
