package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	// codexContentCharCap caps the size of a single oversized text part or tool output.
	codexContentCharCap = 8000
	// codexToolDescCharCap caps the size of a tool description.
	codexToolDescCharCap = 600
	// codexHistoryKeepRecentItems is how many trailing input items are always
	// preserved when trimming history.
	codexHistoryKeepRecentItems = 6
)

// Execute runs the Codex request. When the upstream rejects it with
// context_too_large and codex.context-overflow-retry is enabled, the request is
// shrunk once (truncate oversized content, drop oldest history, shorten tool
// descriptions) and retried a single time; if that retry still fails the error is
// returned unchanged. When the feature is disabled (the default) the request is
// sent exactly once, preserving the previous behavior.
func (e *CodexExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	resp, err = e.executeOnce(ctx, auth, req, opts)
	if err == nil || !e.codexContextOverflowRetryEnabled() || !codexErrIsContextOverflowErr(err) {
		return resp, err
	}
	reduced, ok := reduceCodexResponsesBodyForOverflow(ctx, "codex executor", req.Payload)
	if !ok {
		return resp, err
	}
	helps.LogWithRequestID(ctx).Debugf("codex executor: context_too_large, retrying once with reduced input")
	req.Payload = reduced
	return e.executeOnce(ctx, auth, req, opts)
}

// ExecuteStream runs the streaming Codex request with the same single context-
// overflow retry. Retry only applies to pre-stream rejections; once streaming has
// started, an error is surfaced to the client unchanged.
func (e *CodexExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (res *cliproxyexecutor.StreamResult, err error) {
	res, err = e.executeStreamOnce(ctx, auth, req, opts)
	if err == nil || !e.codexContextOverflowRetryEnabled() || !codexErrIsContextOverflowErr(err) {
		return res, err
	}
	reduced, ok := reduceCodexResponsesBodyForOverflow(ctx, "codex executor", req.Payload)
	if !ok {
		return res, err
	}
	helps.LogWithRequestID(ctx).Debugf("codex executor: context_too_large, retrying stream once with reduced input")
	req.Payload = reduced
	return e.executeStreamOnce(ctx, auth, req, opts)
}

// codexContextOverflowRetryEnabled reports whether the single reduce-and-retry on
// context_too_large is enabled by configuration (default off).
func (e *CodexExecutor) codexContextOverflowRetryEnabled() bool {
	return e != nil && e.cfg != nil && e.cfg.Codex.ContextOverflowRetry
}

// codexErrIsContextOverflow reports whether an upstream status/body represents a
// context-length / context_too_large rejection.
func codexErrIsContextOverflow(statusCode int, body []byte) bool {
	if codexTerminalErrorIsContextLength(body) {
		return true
	}
	code, _, ok := codexStatusErrorClassification(statusCode, body)
	return ok && code == "context_too_large"
}

// codexErrIsContextOverflowErr reports whether err carries a context_too_large
// status error returned by the Codex executor.
func codexErrIsContextOverflowErr(err error) bool {
	if err == nil {
		return false
	}
	var se statusErr
	if errors.As(err, &se) {
		return codexErrIsContextOverflow(se.StatusCode(), []byte(se.Error()))
	}
	return false
}

// reduceCodexResponsesBodyForOverflow applies every reduction step once to a
// Responses-format payload: truncate oversized content, drop the oldest history
// (instructions and the most recent items are preserved), and shorten oversized
// tool descriptions (tools are never removed). It returns (reduced, true) when any
// step changed something, or (payload, false) when there is nothing to reduce.
func reduceCodexResponsesBodyForOverflow(ctx context.Context, provider string, payload []byte) ([]byte, bool) {
	if !gjson.ValidBytes(payload) {
		return payload, false
	}
	out := payload
	changed := false
	if r, ok := shrinkCodexLargeContent(ctx, provider, out, codexContentCharCap); ok {
		out, changed = r, true
	}
	if r, ok := trimCodexOldestHistory(ctx, provider, out); ok {
		out, changed = r, true
	}
	if r, ok := shrinkCodexToolDescriptions(ctx, provider, out, codexToolDescCharCap); ok {
		out, changed = r, true
	}
	if changed {
		// Trimming can orphan function_call_output items; the normalization gate
		// drops those and keeps the input array valid.
		out = normalizeCodexResponsesRequest(ctx, provider, out)
	}
	return out, changed
}

// shrinkCodexLargeContent truncates oversized text content parts and tool outputs.
func shrinkCodexLargeContent(ctx context.Context, provider string, body []byte, limit int) ([]byte, bool) {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, false
	}
	provider = codexNormalizeProviderLabel(provider)
	updated := body
	changed := false
	for idx, item := range input.Array() {
		if strings.TrimSpace(item.Get("type").String()) == "function_call_output" {
			out := item.Get("output")
			if out.Type == gjson.String && len(out.String()) > limit {
				if truncated, did := codexTruncateText(out.String(), limit); did {
					updated, _ = sjson.SetBytes(updated, fmt.Sprintf("input.%d.output", idx), truncated)
					changed = true
				}
			}
			continue
		}
		content := item.Get("content")
		if !content.IsArray() {
			continue
		}
		for j, part := range content.Array() {
			txt := part.Get("text")
			if txt.Exists() && len(txt.String()) > limit {
				if truncated, did := codexTruncateText(txt.String(), limit); did {
					updated, _ = sjson.SetBytes(updated, fmt.Sprintf("input.%d.content.%d.text", idx, j), truncated)
					changed = true
				}
			}
		}
	}
	if changed {
		helps.LogWithRequestID(ctx).Debugf("%s: context overflow: truncated oversized content (limit=%d)", provider, limit)
	}
	return updated, changed
}

// trimCodexOldestHistory drops the oldest half of the trimmable input items,
// preserving leading developer/system instructions and the most recent items.
func trimCodexOldestHistory(ctx context.Context, provider string, body []byte) ([]byte, bool) {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, false
	}
	items := input.Array()
	n := len(items)
	if n == 0 {
		return body, false
	}

	// Leading instruction messages (developer/system) are protected.
	leading := 0
	for leading < n {
		it := items[leading]
		typ := strings.TrimSpace(it.Get("type").String())
		role := strings.TrimSpace(it.Get("role").String())
		if (typ == "message" || typ == "") && (role == "developer" || role == "system") {
			leading++
			continue
		}
		break
	}

	// The most recent items are protected.
	tailStart := n - codexHistoryKeepRecentItems
	if tailStart < leading {
		tailStart = leading
	}
	trimmable := tailStart - leading
	if trimmable <= 0 {
		return body, false
	}
	dropCount := (trimmable + 1) / 2
	if dropCount <= 0 {
		return body, false
	}

	var buf bytes.Buffer
	buf.WriteByte('[')
	first := true
	for idx, item := range items {
		if idx >= leading && idx < leading+dropCount {
			continue // drop the oldest trimmable chunk
		}
		if !first {
			buf.WriteByte(',')
		}
		buf.WriteString(item.Raw)
		first = false
	}
	buf.WriteByte(']')

	updated, err := sjson.SetRawBytes(body, "input", buf.Bytes())
	if err != nil {
		return body, false
	}
	helps.LogWithRequestID(ctx).Debugf("%s: context overflow: dropped %d oldest history items", codexNormalizeProviderLabel(provider), dropCount)
	return updated, true
}

// shrinkCodexToolDescriptions shortens oversized tool descriptions. Tools are never
// removed and their name/parameters are left intact.
func shrinkCodexToolDescriptions(ctx context.Context, provider string, body []byte, limit int) ([]byte, bool) {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body, false
	}
	provider = codexNormalizeProviderLabel(provider)
	updated := body
	changed := false
	for i, tool := range tools.Array() {
		desc := tool.Get("description")
		if desc.Type == gjson.String && len(desc.String()) > limit {
			if truncated, did := codexTruncateText(desc.String(), limit); did {
				updated, _ = sjson.SetBytes(updated, fmt.Sprintf("tools.%d.description", i), truncated)
				changed = true
			}
		}
	}
	if changed {
		helps.LogWithRequestID(ctx).Debugf("%s: context overflow: shortened tool descriptions (limit=%d)", provider, limit)
	}
	return updated, changed
}

// codexTruncateText shortens s to at most limitRunes runes, appending a marker.
// The second return value reports whether truncation actually happened (false when
// the rune count was already within the limit, even if the byte length exceeded it).
func codexTruncateText(s string, limitRunes int) (string, bool) {
	r := []rune(s)
	if len(r) <= limitRunes {
		return s, false
	}
	return string(r[:limitRunes]) + fmt.Sprintf("\n…[truncated %d chars]", len(r)-limitRunes), true
}
