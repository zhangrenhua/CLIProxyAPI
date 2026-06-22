package executor

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// normalizeCodexResponsesRequest brings an OpenAI Responses request body into
// compliance with what the Codex upstream accepts. It is a defensive, idempotent
// gate applied right before the request is forwarded, independent of the source
// protocol the request was translated from. Well-formed requests are returned
// untouched.
//
// It repairs client/translation mistakes the Codex upstream rejects with
// invalid_request_error:
//
//  1. Conversation items carrying role "tool" (a Chat Completions concept) are
//     rewritten into Responses "function_call_output" items. The Responses input
//     only accepts roles assistant/system/developer/user on message items.
//  2. Tool-call items with an empty call_id (function_call / custom_tool_call and
//     their *_output) are repaired by pairing them in order within their family,
//     and orphan outputs are dropped: an empty output with no preceding call, or a
//     non-empty output whose call_id matches no tool call anywhere in the input
//     (the upstream rejects both).
//  3. Chat Completions style message content parts (type "text"/"image_url"/"file")
//     are converted to the Responses part types (input_text/output_text per role,
//     input_image, input_file).
//  4. Chat Completions style function tool definitions ({"type":"function",
//     "function":{...}}) are flattened to the Responses shape ({"type":"function",
//     "name":..., "parameters":...}).
//  5. Chat Completions style forced tool_choice ({"type":"function",
//     "function":{"name":...}}) is flattened to the Responses shape
//     ({"type":"function","name":...}).
//  6. Chat Completions only parameters the Codex /responses upstream rejects with
//     "Unsupported parameter: <name>" (e.g. max_tokens, messages) are stripped.
func normalizeCodexResponsesRequest(ctx context.Context, provider string, body []byte) []byte {
	body = normalizeCodexResponsesInputItems(ctx, provider, body)
	body = normalizeCodexResponsesToolDefinitions(ctx, provider, body)
	body = normalizeCodexResponsesToolChoice(ctx, provider, body)
	body = stripCodexResponsesUnsupportedParams(ctx, provider, body)
	return body
}

// codexResponsesUnsupportedParams lists Chat Completions parameters that the Codex
// /responses upstream rejects with "Unsupported parameter: <name>". They either
// have no Responses equivalent or use a different field, so they are stripped
// before forwarding. A well-formed Responses request never carries these, so
// stripping them cannot break a request that was already valid.
var codexResponsesUnsupportedParams = []string{
	"max_tokens",
	"messages",
	"frequency_penalty",
	"presence_penalty",
	"logit_bias",
	"logprobs",
	"top_logprobs",
	"n",
	"stop",
	"seed",
}

// stripCodexResponsesUnsupportedParams removes top-level parameters the Codex
// /responses upstream does not accept. It is idempotent: absent keys are skipped.
func stripCodexResponsesUnsupportedParams(ctx context.Context, provider string, body []byte) []byte {
	provider = codexNormalizeProviderLabel(provider)
	updated := body
	for _, key := range codexResponsesUnsupportedParams {
		if !gjson.GetBytes(updated, key).Exists() {
			continue
		}
		next, err := sjson.DeleteBytes(updated, key)
		if err != nil {
			helps.LogWithRequestID(ctx).Debugf("%s: failed to strip unsupported parameter %q: %v", provider, key, err)
			continue
		}
		updated = next
		helps.LogWithRequestID(ctx).Debugf("%s: stripped unsupported Responses parameter %q", provider, key)
	}
	return updated
}

// normalizeCodexResponsesToolChoice flattens a Chat Completions style forced
// function tool_choice into the shape the Responses API expects. Chat puts the
// name under "function" ({"type":"function","function":{"name":...}}); Responses
// requires it at the top level ({"type":"function","name":...}) and otherwise
// rejects with "Missing required parameter: 'tool_choice.name'". It only fires
// when the choice nests a function name and lacks a top-level name, so string
// choices ("auto"/"none"/"required"), built-in choices ({"type":"web_search"}),
// and already-compliant function choices are left untouched.
func normalizeCodexResponsesToolChoice(ctx context.Context, provider string, body []byte) []byte {
	tc := gjson.GetBytes(body, "tool_choice")
	if !tc.IsObject() {
		return body
	}
	if strings.TrimSpace(tc.Get("type").String()) != "function" {
		return body
	}
	if strings.TrimSpace(tc.Get("name").String()) != "" {
		return body // already Responses-style
	}
	fn := tc.Get("function")
	if !fn.IsObject() {
		return body
	}
	name := strings.TrimSpace(fn.Get("name").String())
	if name == "" {
		return body // no name to flatten
	}

	provider = codexNormalizeProviderLabel(provider)
	updated := body
	updated, _ = sjson.SetBytes(updated, "tool_choice.name", name)
	updated, _ = sjson.DeleteBytes(updated, "tool_choice.function")
	helps.LogWithRequestID(ctx).Debugf("%s: flattened chat-style tool_choice function name", provider)
	return updated
}

// normalizeCodexResponsesInputItems fixes role "tool" message items, chat-style
// content parts, empty/orphan call_id values on tool-call items (function_call and
// custom_tool_call families), and function_call items missing a name (recovered
// from a nested "function" object, or dropped with their paired output) in the
// Responses "input" array.
func normalizeCodexResponsesInputItems(ctx context.Context, provider string, body []byte) []byte {
	input := gjson.GetBytes(body, "input")
	if !input.Exists() || !input.IsArray() {
		return body
	}
	provider = codexNormalizeProviderLabel(provider)

	items := input.Array()

	// Pass 1: collect every non-empty call_id declared by a tool-call item
	// (function_call or custom_tool_call). These are the only valid targets a
	// matching *_output item may reference.
	validCallIDs := make(map[string]struct{}, 4)
	for _, item := range items {
		switch strings.TrimSpace(item.Get("type").String()) {
		case "function_call", "custom_tool_call":
			if id := strings.TrimSpace(item.Get("call_id").String()); id != "" {
				validCallIDs[id] = struct{}{}
			}
		}
	}

	// Pass 2: rewrite items.
	rebuilt := make([][]byte, 0, len(items))
	// Tool-call ids awaiting their output, FIFO, kept per family ("function" vs
	// "custom") so a *_output only pairs with a call of the same family.
	pendingCallIDs := map[string][]string{}
	droppedCallIDs := make(map[string]struct{}, 2) // function_call ids removed (e.g. missing name)
	syntheticSeq := 0
	changed := false

	for index, item := range items {
		raw := []byte(item.Raw)
		itemType := strings.TrimSpace(gjson.GetBytes(raw, "type").String())
		role := strings.TrimSpace(gjson.GetBytes(raw, "role").String())

		switch {
		case role == "tool" && (itemType == "" || itemType == "message"):
			// role:"tool" message items are invalid in the Responses input.
			callID := strings.TrimSpace(gjson.GetBytes(raw, "call_id").String())
			if callID == "" {
				callID = strings.TrimSpace(gjson.GetBytes(raw, "tool_call_id").String())
			}
			converted := []byte(`{"type":"function_call_output"}`)
			converted, _ = sjson.SetBytes(converted, "call_id", callID)
			converted = setCodexToolOutputContent(converted, gjson.GetBytes(raw, "content"))
			raw = converted
			itemType = "function_call_output"
			changed = true
			helps.LogWithRequestID(ctx).Debugf("%s: rewrote input[%d] role=tool message as function_call_output", provider, index)
		case itemType == "message" || (itemType == "" && role != ""):
			// Convert chat-style content parts on message items.
			if updatedRaw, c := normalizeCodexResponsesMessageContent(raw, role); c {
				raw = updatedRaw
				changed = true
				helps.LogWithRequestID(ctx).Debugf("%s: normalized chat-style content parts at input[%d]", provider, index)
			}
		}

		switch itemType {
		case "function_call", "custom_tool_call":
			family := codexToolCallFamily(itemType)
			callID := strings.TrimSpace(gjson.GetBytes(raw, "call_id").String())
			// function_call requires a top-level "name". Recover it from a chat-style
			// nested "function" object when missing; carry arguments along too. A
			// function_call with no recoverable name is invalid, so it is dropped (its
			// paired output is dropped below). custom_tool_call carries no nested
			// "function" form, so only its call_id is repaired here.
			if itemType == "function_call" && strings.TrimSpace(gjson.GetBytes(raw, "name").String()) == "" {
				if fn := gjson.GetBytes(raw, "function"); fn.IsObject() {
					if fnName := strings.TrimSpace(fn.Get("name").String()); fnName != "" {
						raw, _ = sjson.SetBytes(raw, "name", fnName)
						if args := fn.Get("arguments"); args.Exists() && !gjson.GetBytes(raw, "arguments").Exists() {
							raw, _ = sjson.SetBytes(raw, "arguments", args.String())
						}
						raw, _ = sjson.DeleteBytes(raw, "function")
						changed = true
						helps.LogWithRequestID(ctx).Debugf("%s: flattened chat-style function_call name at input[%d]", provider, index)
					}
				}
				if strings.TrimSpace(gjson.GetBytes(raw, "name").String()) == "" {
					if callID != "" {
						droppedCallIDs[callID] = struct{}{}
					}
					changed = true
					helps.LogWithRequestID(ctx).Debugf("%s: dropped function_call without a name at input[%d]", provider, index)
					continue
				}
			}
			if callID == "" {
				syntheticSeq++
				callID = fmt.Sprintf("call_normalized_%d", syntheticSeq)
				raw, _ = sjson.SetBytes(raw, "call_id", callID)
				changed = true
				helps.LogWithRequestID(ctx).Debugf("%s: assigned synthetic call_id %q to %s at input[%d]", provider, callID, itemType, index)
			}
			pendingCallIDs[family] = append(pendingCallIDs[family], callID)
		case "function_call_output", "custom_tool_call_output":
			family := codexToolCallFamily(itemType)
			callID := strings.TrimSpace(gjson.GetBytes(raw, "call_id").String())
			switch {
			case callID == "":
				if len(pendingCallIDs[family]) == 0 {
					changed = true
					helps.LogWithRequestID(ctx).Debugf("%s: dropped orphan %s with empty call_id at input[%d]", provider, itemType, index)
					continue
				}
				callID = pendingCallIDs[family][0]
				pendingCallIDs[family] = pendingCallIDs[family][1:]
				raw, _ = sjson.SetBytes(raw, "call_id", callID)
				changed = true
				helps.LogWithRequestID(ctx).Debugf("%s: paired %s at input[%d] with call_id %q", provider, itemType, index, callID)
			default:
				if _, dropped := droppedCallIDs[callID]; dropped {
					// Its tool call was removed (e.g. function_call missing name); drop the output too.
					changed = true
					helps.LogWithRequestID(ctx).Debugf("%s: dropped %s at input[%d] (its tool call %q was removed)", provider, itemType, index, callID)
					continue
				}
				if _, ok := validCallIDs[callID]; !ok {
					// Non-empty call_id that matches no tool call in the input.
					changed = true
					helps.LogWithRequestID(ctx).Debugf("%s: dropped orphan %s at input[%d] (call_id %q has no matching tool call)", provider, itemType, index, callID)
					continue
				}
				pendingCallIDs[family] = removeFirstString(pendingCallIDs[family], callID)
			}
		}

		rebuilt = append(rebuilt, raw)
	}

	if !changed {
		return body
	}

	// Rebuild the array by byte concatenation so unmodified items keep their exact
	// original bytes (json.Marshal would HTML-escape <, >, & inside string values).
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, raw := range rebuilt {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(raw)
	}
	buf.WriteByte(']')

	updated, err := sjson.SetRawBytes(body, "input", buf.Bytes())
	if err != nil {
		helps.LogWithRequestID(ctx).Debugf("%s: failed to write normalized input array: %v", provider, err)
		return body
	}
	return updated
}

// normalizeCodexResponsesMessageContent converts Chat Completions style content
// parts on a message item into the Responses part types. It mirrors the
// chat-completions -> codex translator: "text" becomes input_text (or output_text
// for assistant messages), and "image_url"/"file" become input_image/input_file
// for user messages. Already-compliant parts are left untouched.
func normalizeCodexResponsesMessageContent(raw []byte, role string) ([]byte, bool) {
	content := gjson.GetBytes(raw, "content")
	if !content.IsArray() {
		return raw, false
	}

	textType := "input_text"
	if role == "assistant" {
		textType = "output_text"
	}

	changed := false
	for i, part := range content.Array() {
		base := fmt.Sprintf("content.%d", i)
		switch strings.TrimSpace(part.Get("type").String()) {
		case "text":
			raw, _ = sjson.SetBytes(raw, base+".type", textType)
			changed = true
		case "image_url":
			if role != "user" {
				continue
			}
			url := part.Get("image_url.url").String()
			if url == "" && part.Get("image_url").Type == gjson.String {
				url = part.Get("image_url").String()
			}
			detail := part.Get("image_url.detail").String()
			fileID := part.Get("image_url.file_id").String()
			raw, _ = sjson.SetBytes(raw, base+".type", "input_image")
			if url != "" {
				raw, _ = sjson.SetBytes(raw, base+".image_url", url)
			} else {
				raw, _ = sjson.DeleteBytes(raw, base+".image_url")
			}
			if detail != "" {
				raw, _ = sjson.SetBytes(raw, base+".detail", detail)
			}
			if fileID != "" {
				raw, _ = sjson.SetBytes(raw, base+".file_id", fileID)
			}
			changed = true
		case "file":
			if role != "user" {
				continue
			}
			fileData := part.Get("file.file_data").String()
			if fileData == "" {
				continue
			}
			filename := part.Get("file.filename").String()
			raw, _ = sjson.SetBytes(raw, base+".type", "input_file")
			raw, _ = sjson.SetBytes(raw, base+".file_data", fileData)
			if filename != "" {
				raw, _ = sjson.SetBytes(raw, base+".filename", filename)
			}
			raw, _ = sjson.DeleteBytes(raw, base+".file")
			changed = true
		}
	}
	return raw, changed
}

// normalizeCodexResponsesToolDefinitions flattens Chat Completions style function
// tool definitions into the shape expected by the Responses API. It only fires
// when a tool nests its descriptor under "function" and lacks a top-level name,
// so already-compliant tool definitions are left untouched.
func normalizeCodexResponsesToolDefinitions(ctx context.Context, provider string, body []byte) []byte {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body
	}
	provider = codexNormalizeProviderLabel(provider)

	updated := body
	for i, tool := range tools.Array() {
		if strings.TrimSpace(tool.Get("type").String()) != "function" {
			continue
		}
		fn := tool.Get("function")
		if !fn.IsObject() {
			continue
		}
		if strings.TrimSpace(tool.Get("name").String()) != "" {
			continue // already Responses-style
		}

		base := fmt.Sprintf("tools.%d", i)
		if name := fn.Get("name"); name.Exists() {
			updated, _ = sjson.SetBytes(updated, base+".name", name.String())
		}
		if desc := fn.Get("description"); desc.Exists() {
			updated, _ = sjson.SetBytes(updated, base+".description", desc.String())
		}
		if params := fn.Get("parameters"); params.Exists() {
			updated, _ = sjson.SetRawBytes(updated, base+".parameters", []byte(params.Raw))
		}
		if strict := fn.Get("strict"); strict.Exists() {
			updated, _ = sjson.SetRawBytes(updated, base+".strict", []byte(strict.Raw))
		}
		updated, _ = sjson.DeleteBytes(updated, base+".function")
		helps.LogWithRequestID(ctx).Debugf("%s: flattened chat-style function tool at tools[%d]", provider, i)
	}
	return updated
}

// setCodexToolOutputContent writes a tool message content into the function_call_output
// "output" field as a string. Responses always accepts a string output, so array and
// object content parts are reduced to their text.
func setCodexToolOutputContent(funcOutput []byte, content gjson.Result) []byte {
	switch {
	case !content.Exists():
		funcOutput, _ = sjson.SetBytes(funcOutput, "output", "")
	case content.Type == gjson.String:
		funcOutput, _ = sjson.SetBytes(funcOutput, "output", content.String())
	case content.IsArray():
		var b strings.Builder
		for _, part := range content.Array() {
			if part.Type == gjson.String {
				b.WriteString(part.String())
				continue
			}
			if t := part.Get("text"); t.Exists() {
				b.WriteString(t.String())
			}
		}
		funcOutput, _ = sjson.SetBytes(funcOutput, "output", b.String())
	default:
		fallback := content.Raw
		if fallback == "" {
			fallback = content.String()
		}
		funcOutput, _ = sjson.SetBytes(funcOutput, "output", fallback)
	}
	return funcOutput
}

// codexToolCallFamily groups the paired tool-call item types so a *_output only
// pairs with a *_call of the same family. "custom_tool_call"/"custom_tool_call_output"
// belong to the "custom" family; "function_call"/"function_call_output" to "function".
func codexToolCallFamily(itemType string) string {
	if strings.HasPrefix(itemType, "custom_tool_call") {
		return "custom"
	}
	return "function"
}

// removeFirstString removes the first occurrence of target from s and returns the result.
func removeFirstString(s []string, target string) []string {
	for i, v := range s {
		if v == target {
			return append(s[:i], s[i+1:]...)
		}
	}
	return s
}

func codexNormalizeProviderLabel(provider string) string {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return "codex executor"
	}
	return provider
}
