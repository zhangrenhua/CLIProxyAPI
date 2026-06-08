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
//  2. function_call / function_call_output items with an empty call_id are
//     repaired by pairing them in order, and orphan outputs are dropped: an empty
//     output with no preceding call, or a non-empty output whose call_id matches no
//     function_call anywhere in the input (the upstream rejects both).
//  3. Chat Completions style message content parts (type "text"/"image_url"/"file")
//     are converted to the Responses part types (input_text/output_text per role,
//     input_image, input_file).
//  4. Chat Completions style function tool definitions ({"type":"function",
//     "function":{...}}) are flattened to the Responses shape ({"type":"function",
//     "name":..., "parameters":...}).
func normalizeCodexResponsesRequest(ctx context.Context, provider string, body []byte) []byte {
	body = normalizeCodexResponsesInputItems(ctx, provider, body)
	body = normalizeCodexResponsesToolDefinitions(ctx, provider, body)
	return body
}

// normalizeCodexResponsesInputItems fixes role "tool" message items, chat-style
// content parts, and empty/orphan call_id values in the Responses "input" array.
func normalizeCodexResponsesInputItems(ctx context.Context, provider string, body []byte) []byte {
	input := gjson.GetBytes(body, "input")
	if !input.Exists() || !input.IsArray() {
		return body
	}
	provider = codexNormalizeProviderLabel(provider)

	items := input.Array()

	// Pass 1: collect every non-empty call_id declared by a function_call item.
	// These are the only valid targets a function_call_output may reference.
	validCallIDs := make(map[string]struct{}, 4)
	for _, item := range items {
		if strings.TrimSpace(item.Get("type").String()) == "function_call" {
			if id := strings.TrimSpace(item.Get("call_id").String()); id != "" {
				validCallIDs[id] = struct{}{}
			}
		}
	}

	// Pass 2: rewrite items.
	rebuilt := make([][]byte, 0, len(items))
	pendingCallIDs := make([]string, 0, 4) // function_call ids awaiting their output, FIFO
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
		case "function_call":
			callID := strings.TrimSpace(gjson.GetBytes(raw, "call_id").String())
			if callID == "" {
				syntheticSeq++
				callID = fmt.Sprintf("call_normalized_%d", syntheticSeq)
				raw, _ = sjson.SetBytes(raw, "call_id", callID)
				changed = true
				helps.LogWithRequestID(ctx).Debugf("%s: assigned synthetic call_id %q to function_call at input[%d]", provider, callID, index)
			}
			pendingCallIDs = append(pendingCallIDs, callID)
		case "function_call_output":
			callID := strings.TrimSpace(gjson.GetBytes(raw, "call_id").String())
			switch {
			case callID == "":
				if len(pendingCallIDs) == 0 {
					changed = true
					helps.LogWithRequestID(ctx).Debugf("%s: dropped orphan function_call_output with empty call_id at input[%d]", provider, index)
					continue
				}
				callID = pendingCallIDs[0]
				pendingCallIDs = pendingCallIDs[1:]
				raw, _ = sjson.SetBytes(raw, "call_id", callID)
				changed = true
				helps.LogWithRequestID(ctx).Debugf("%s: paired function_call_output at input[%d] with call_id %q", provider, index, callID)
			default:
				if _, ok := validCallIDs[callID]; !ok {
					// Non-empty call_id that matches no function_call in the input.
					changed = true
					helps.LogWithRequestID(ctx).Debugf("%s: dropped orphan function_call_output at input[%d] (call_id %q has no matching function_call)", provider, index, callID)
					continue
				}
				pendingCallIDs = removeFirstString(pendingCallIDs, callID)
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
