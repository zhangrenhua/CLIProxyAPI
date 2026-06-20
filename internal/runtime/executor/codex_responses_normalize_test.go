package executor

import (
	"bytes"
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func TestNormalizeCodexResponsesFlattensFunctionCallName(t *testing.T) {
	// function_call with a nested chat-style function{name,arguments} but no
	// top-level name -> name/arguments flattened, function removed.
	body := []byte(`{"input":[{"type":"function_call","call_id":"c1","function":{"name":"get_weather","arguments":"{\"q\":1}"}},{"type":"function_call_output","call_id":"c1","output":"ok"}]}`)

	out := normalizeCodexResponsesRequest(context.Background(), "test", body)

	fc := gjson.GetBytes(out, "input.0")
	if got := fc.Get("name").String(); got != "get_weather" {
		t.Fatalf("expected flattened name get_weather, got %q", got)
	}
	if got := fc.Get("arguments").String(); got != `{"q":1}` {
		t.Fatalf("expected flattened arguments, got %q", got)
	}
	if fc.Get("function").Exists() {
		t.Fatalf("nested function should be removed: %s", fc.Raw)
	}
	// Its output is kept (call still valid after flatten).
	if n := len(gjson.GetBytes(out, "input").Array()); n != 2 {
		t.Fatalf("expected both items kept, got %d", n)
	}
}

func TestNormalizeCodexResponsesDropsNamelessFunctionCallAndOutput(t *testing.T) {
	// function_call with no recoverable name -> dropped together with its output.
	body := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},{"type":"function_call","call_id":"c1","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":"ok"}]}`)

	out := normalizeCodexResponsesRequest(context.Background(), "test", body)

	items := gjson.GetBytes(out, "input").Array()
	if len(items) != 1 {
		t.Fatalf("expected nameless function_call and its output dropped (len 1), got %d: %s", len(items), gjson.GetBytes(out, "input").Raw)
	}
	if got := items[0].Get("role").String(); got != "user" {
		t.Fatalf("expected the user message to survive, got role %q", got)
	}
}

func TestNormalizeCodexResponsesKeepsNamedFunctionCall(t *testing.T) {
	body := []byte(`{"input":[{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":"ok"}]}`)
	out := normalizeCodexResponsesRequest(context.Background(), "test", body)
	if string(out) != string(body) {
		t.Fatalf("expected a valid named function_call to be unchanged.\nwant: %s\ngot:  %s", body, out)
	}
}

func TestNormalizeCodexResponsesRewritesToolRole(t *testing.T) {
	body := []byte(`{"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{}"},
		{"type":"message","role":"tool","tool_call_id":"call_1","content":"sunny"}
	]}`)

	out := normalizeCodexResponsesRequest(context.Background(), "test", body)

	item := gjson.GetBytes(out, "input.2")
	if got := item.Get("type").String(); got != "function_call_output" {
		t.Fatalf("expected function_call_output, got %q", got)
	}
	if got := item.Get("call_id").String(); got != "call_1" {
		t.Fatalf("expected call_id call_1, got %q", got)
	}
	if got := item.Get("output").String(); got != "sunny" {
		t.Fatalf("expected output sunny, got %q", got)
	}
	if item.Get("role").Exists() {
		t.Fatalf("role field should be dropped after rewrite: %s", item.Raw)
	}
}

func TestNormalizeCodexResponsesToolRoleArrayContent(t *testing.T) {
	body := []byte(`{"input":[
		{"type":"function_call","call_id":"call_x","name":"f","arguments":"{}"},
		{"role":"tool","call_id":"call_x","content":[{"type":"output_text","text":"a"},{"type":"output_text","text":"b"}]}
	]}`)

	out := normalizeCodexResponsesRequest(context.Background(), "test", body)

	item := gjson.GetBytes(out, "input.1")
	if got := item.Get("type").String(); got != "function_call_output" {
		t.Fatalf("expected function_call_output, got %q", got)
	}
	if got := item.Get("output").String(); got != "ab" {
		t.Fatalf("expected concatenated output ab, got %q", got)
	}
}

func TestNormalizeCodexResponsesPairsEmptyCallID(t *testing.T) {
	body := []byte(`{"input":[
		{"type":"function_call","call_id":"call_42","name":"f","arguments":"{}"},
		{"type":"function_call_output","call_id":"","output":"done"}
	]}`)

	out := normalizeCodexResponsesRequest(context.Background(), "test", body)

	if got := gjson.GetBytes(out, "input.1.call_id").String(); got != "call_42" {
		t.Fatalf("expected empty call_id paired with call_42, got %q", got)
	}
}

func TestNormalizeCodexResponsesDropsOrphanOutput(t *testing.T) {
	body := []byte(`{"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"function_call_output","call_id":"","output":"orphan"}
	]}`)

	out := normalizeCodexResponsesRequest(context.Background(), "test", body)

	input := gjson.GetBytes(out, "input")
	if n := len(input.Array()); n != 1 {
		t.Fatalf("expected orphan output dropped (len 1), got len %d: %s", n, input.Raw)
	}
	if got := gjson.GetBytes(out, "input.0.role").String(); got != "user" {
		t.Fatalf("expected surviving user message, got %q", got)
	}
}

func TestNormalizeCodexResponsesSyntheticCallIDForFunctionCall(t *testing.T) {
	body := []byte(`{"input":[
		{"type":"function_call","call_id":"","name":"f","arguments":"{}"},
		{"type":"function_call_output","call_id":"","output":"done"}
	]}`)

	out := normalizeCodexResponsesRequest(context.Background(), "test", body)

	callID := gjson.GetBytes(out, "input.0.call_id").String()
	if callID == "" {
		t.Fatalf("expected synthetic call_id on function_call, got empty")
	}
	if got := gjson.GetBytes(out, "input.1.call_id").String(); got != callID {
		t.Fatalf("expected output paired with synthetic call_id %q, got %q", callID, got)
	}
}

func TestNormalizeCodexResponsesLeavesValidRequestUnchanged(t *testing.T) {
	body := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":"ok"}],"tools":[{"type":"function","name":"f","parameters":{"type":"object"}}]}`)

	out := normalizeCodexResponsesRequest(context.Background(), "test", body)

	if string(out) != string(body) {
		t.Fatalf("expected well-formed request to be returned unchanged.\nwant: %s\ngot:  %s", body, out)
	}
}

func TestNormalizeCodexResponsesFlattensChatStyleTool(t *testing.T) {
	body := []byte(`{"tools":[{"type":"function","function":{"name":"get_weather","description":"d","parameters":{"type":"object"},"strict":true}}]}`)

	out := normalizeCodexResponsesRequest(context.Background(), "test", body)

	tool := gjson.GetBytes(out, "tools.0")
	if got := tool.Get("name").String(); got != "get_weather" {
		t.Fatalf("expected flattened name get_weather, got %q", got)
	}
	if got := tool.Get("description").String(); got != "d" {
		t.Fatalf("expected flattened description d, got %q", got)
	}
	if got := tool.Get("parameters.type").String(); got != "object" {
		t.Fatalf("expected flattened parameters, got %q", got)
	}
	if !tool.Get("strict").Bool() {
		t.Fatalf("expected flattened strict=true")
	}
	if tool.Get("function").Exists() {
		t.Fatalf("nested function object should be removed: %s", tool.Raw)
	}
}

func TestNormalizeCodexResponsesPreservesSpecialCharsOnRebuild(t *testing.T) {
	// A surviving item containing <, >, & must keep its exact bytes even when the
	// array is rebuilt because another item triggered a change.
	body := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"if a < b && b > c"}]},{"type":"function_call_output","call_id":"","output":"x"}]}`)

	out := normalizeCodexResponsesRequest(context.Background(), "test", body)

	text := gjson.GetBytes(out, "input.0.content.0.text").String()
	if text != "if a < b && b > c" {
		t.Fatalf("special chars not preserved, got %q", text)
	}
	// Byte-faithful rebuild keeps literal <, >, & rather than HTML-escaping them.
	// If the bytes had been HTML-escaped (json.Marshal default), this literal
	// substring would not appear because < / > / & would become < etc.
	if !bytes.Contains(out, []byte(`if a < b && b > c`)) {
		t.Fatalf("expected literal special chars preserved on rebuild: %s", out)
	}
	// Orphan output (no preceding function_call) must be dropped.
	if n := len(gjson.GetBytes(out, "input").Array()); n != 1 {
		t.Fatalf("expected orphan dropped, len=%d", n)
	}
}

func TestNormalizeCodexResponsesConvertsUserTextPart(t *testing.T) {
	body := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	out := normalizeCodexResponsesRequest(context.Background(), "test", body)

	if got := gjson.GetBytes(out, "input.0.content.0.type").String(); got != "input_text" {
		t.Fatalf("expected input_text, got %q", got)
	}
	if got := gjson.GetBytes(out, "input.0.content.0.text").String(); got != "hi" {
		t.Fatalf("expected text preserved, got %q", got)
	}
}

func TestNormalizeCodexResponsesConvertsAssistantTextPart(t *testing.T) {
	body := []byte(`{"input":[{"type":"message","role":"assistant","content":[{"type":"text","text":"answer"}]}]}`)

	out := normalizeCodexResponsesRequest(context.Background(), "test", body)

	if got := gjson.GetBytes(out, "input.0.content.0.type").String(); got != "output_text" {
		t.Fatalf("expected output_text for assistant, got %q", got)
	}
}

func TestNormalizeCodexResponsesConvertsImageURLPart(t *testing.T) {
	body := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"image_url","image_url":{"url":"https://x/i.png","detail":"high"}}]}]}`)

	out := normalizeCodexResponsesRequest(context.Background(), "test", body)

	part := gjson.GetBytes(out, "input.0.content.0")
	if got := part.Get("type").String(); got != "input_image" {
		t.Fatalf("expected input_image, got %q", got)
	}
	if got := part.Get("image_url").String(); got != "https://x/i.png" {
		t.Fatalf("expected flattened image_url string, got %q", got)
	}
	if got := part.Get("detail").String(); got != "high" {
		t.Fatalf("expected detail high, got %q", got)
	}
	if part.Get("image_url").IsObject() {
		t.Fatalf("image_url should be a string, not object: %s", part.Raw)
	}
}

func TestNormalizeCodexResponsesDropsNonEmptyOrphanOutput(t *testing.T) {
	// call_id "ghost" matches no function_call anywhere in the input -> drop.
	body := []byte(`{"input":[
		{"type":"function_call","call_id":"real","name":"f","arguments":"{}"},
		{"type":"function_call_output","call_id":"real","output":"ok"},
		{"type":"function_call_output","call_id":"ghost","output":"orphan"}
	]}`)

	out := normalizeCodexResponsesRequest(context.Background(), "test", body)

	items := gjson.GetBytes(out, "input").Array()
	if len(items) != 2 {
		t.Fatalf("expected non-empty orphan dropped (len 2), got %d: %s", len(items), gjson.GetBytes(out, "input").Raw)
	}
	if got := gjson.GetBytes(out, "input.1.call_id").String(); got != "real" {
		t.Fatalf("expected matching output kept, got %q", got)
	}
}

func TestNormalizeCodexResponsesKeepsMatchingNonEmptyOutput(t *testing.T) {
	body := []byte(`{"input":[{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":"ok"}]}`)

	out := normalizeCodexResponsesRequest(context.Background(), "test", body)

	if string(out) != string(body) {
		t.Fatalf("expected matching pair unchanged.\nwant: %s\ngot:  %s", body, out)
	}
}

func TestNormalizeCodexResponsesFlattensChatStyleToolChoice(t *testing.T) {
	body := []byte(`{"tool_choice":{"type":"function","function":{"name":"get_weather"}}}`)

	out := normalizeCodexResponsesRequest(context.Background(), "test", body)

	tc := gjson.GetBytes(out, "tool_choice")
	if got := tc.Get("name").String(); got != "get_weather" {
		t.Fatalf("expected flattened tool_choice.name get_weather, got %q", got)
	}
	if tc.Get("function").Exists() {
		t.Fatalf("nested tool_choice.function should be removed: %s", tc.Raw)
	}
	if got := tc.Get("type").String(); got != "function" {
		t.Fatalf("expected type function preserved, got %q", got)
	}
}

func TestNormalizeCodexResponsesLeavesValidToolChoiceUnchanged(t *testing.T) {
	cases := []string{
		`{"tool_choice":"auto"}`,
		`{"tool_choice":"required"}`,
		`{"tool_choice":{"type":"function","name":"get_weather"}}`,
		`{"tool_choice":{"type":"web_search"}}`,
	}
	for _, body := range cases {
		out := normalizeCodexResponsesRequest(context.Background(), "test", []byte(body))
		if string(out) != body {
			t.Fatalf("expected tool_choice unchanged.\nwant: %s\ngot:  %s", body, out)
		}
	}
}

func TestNormalizeCodexResponsesStripsUnsupportedParams(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"max_tokens":100,"messages":[{"role":"user","content":"x"}],"frequency_penalty":0.5,"presence_penalty":0.1,"logit_bias":{"a":1},"logprobs":true,"top_logprobs":3,"n":2,"stop":["x"],"seed":7}`)

	out := normalizeCodexResponsesRequest(context.Background(), "test", body)

	for _, key := range []string{"max_tokens", "messages", "frequency_penalty", "presence_penalty", "logit_bias", "logprobs", "top_logprobs", "n", "stop", "seed"} {
		if gjson.GetBytes(out, key).Exists() {
			t.Fatalf("expected %q stripped, still present: %s", key, out)
		}
	}
	// Supported fields must survive.
	if gjson.GetBytes(out, "model").String() != "gpt-5.5" {
		t.Fatalf("model should be preserved")
	}
	if gjson.GetBytes(out, "input.0.content.0.text").String() != "hi" {
		t.Fatalf("input should be preserved")
	}
}

func TestNormalizeCodexResponsesKeepsSupportedTokenField(t *testing.T) {
	// Only the unsupported names are stripped; supported fields are untouched.
	body := []byte(`{"model":"gpt-5.5","reasoning":{"effort":"medium"},"store":false}`)
	out := normalizeCodexResponsesRequest(context.Background(), "test", body)
	if string(out) != string(body) {
		t.Fatalf("expected request with only supported fields unchanged.\nwant: %s\ngot:  %s", body, out)
	}
}

func TestNormalizeCodexResponsesIgnoresNonArrayInput(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","input":"hello"}`)
	out := normalizeCodexResponsesRequest(context.Background(), "test", body)
	if string(out) != string(body) {
		t.Fatalf("expected non-array input unchanged, got %s", out)
	}
}
