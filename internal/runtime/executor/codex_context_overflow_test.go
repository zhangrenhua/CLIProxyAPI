package executor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/tidwall/gjson"
)

func TestCodexErrIsContextOverflow(t *testing.T) {
	overflow := newCodexStatusErr(400, []byte(`{"error":{"message":"Your input exceeds the context window of this model."}}`))
	if !codexErrIsContextOverflowErr(overflow) {
		t.Fatalf("expected context overflow detected for %v", overflow)
	}
	other := newCodexStatusErr(400, []byte(`{"error":{"message":"bad request","type":"invalid_request_error"}}`))
	if codexErrIsContextOverflowErr(other) {
		t.Fatalf("did not expect context overflow for a generic 400")
	}
	if codexErrIsContextOverflowErr(nil) {
		t.Fatalf("nil error must not be context overflow")
	}
}

func TestCodexContextOverflowRetryGating(t *testing.T) {
	if (&CodexExecutor{}).codexContextOverflowRetryEnabled() {
		t.Fatalf("nil cfg must disable retry")
	}
	off := &CodexExecutor{cfg: &config.Config{}}
	if off.codexContextOverflowRetryEnabled() {
		t.Fatalf("default (flag off) must disable retry")
	}
	on := &CodexExecutor{cfg: &config.Config{Codex: config.CodexConfig{ContextOverflowRetry: true}}}
	if !on.codexContextOverflowRetryEnabled() {
		t.Fatalf("flag on must enable retry")
	}
}

func TestShrinkCodexLargeContent(t *testing.T) {
	big := strings.Repeat("x", codexContentCharCap+500)
	body := []byte(fmt.Sprintf(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":%q}]},{"type":"function_call_output","call_id":"c1","output":%q}]}`, big, big))

	out, changed := shrinkCodexLargeContent(context.Background(), "test", body, codexContentCharCap)
	if !changed {
		t.Fatalf("expected content to be truncated")
	}
	gotText := gjson.GetBytes(out, "input.0.content.0.text").String()
	if len([]rune(gotText)) <= codexContentCharCap || !strings.Contains(gotText, "truncated") {
		t.Fatalf("text not truncated as expected (len=%d)", len([]rune(gotText)))
	}
	gotOut := gjson.GetBytes(out, "input.1.output").String()
	if !strings.Contains(gotOut, "truncated") {
		t.Fatalf("output not truncated")
	}

	// Small content is untouched.
	small := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	if _, changed := shrinkCodexLargeContent(context.Background(), "test", small, codexContentCharCap); changed {
		t.Fatalf("small content should not change")
	}
}

func TestShrinkCodexLargeContentMultibyte(t *testing.T) {
	// 10 CJK runes = 30 bytes. With limit 20, byte length exceeds it but the rune
	// count does not, so it must NOT be reported as changed (regression guard).
	within := []byte(fmt.Sprintf(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":%q}]}]}`, strings.Repeat("中", 10)))
	if _, changed := shrinkCodexLargeContent(context.Background(), "test", within, 20); changed {
		t.Fatalf("multibyte content within the rune limit must not be reported changed")
	}

	// 30 CJK runes with limit 10 must actually truncate.
	over := []byte(fmt.Sprintf(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":%q}]}]}`, strings.Repeat("中", 30)))
	out, changed := shrinkCodexLargeContent(context.Background(), "test", over, 10)
	if !changed {
		t.Fatalf("multibyte content over the rune limit must truncate")
	}
	if !strings.Contains(gjson.GetBytes(out, "input.0.content.0.text").String(), "truncated") {
		t.Fatalf("expected truncation marker")
	}
}

func TestTrimCodexOldestHistory(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{"input":[`)
	sb.WriteString(`{"type":"message","role":"developer","content":[{"type":"input_text","text":"sys"}]}`)
	for i := 0; i < 10; i++ {
		sb.WriteString(fmt.Sprintf(`,{"type":"message","role":"user","content":[{"type":"input_text","text":"m%d"}]}`, i))
	}
	sb.WriteString(`]}`)
	body := []byte(sb.String())

	out, ok := trimCodexOldestHistory(context.Background(), "test", body)
	if !ok {
		t.Fatalf("expected trimming to occur")
	}
	items := gjson.GetBytes(out, "input").Array()
	if len(items) >= 11 {
		t.Fatalf("expected items dropped, still %d", len(items))
	}
	// Leading instruction preserved.
	if got := items[0].Get("role").String(); got != "developer" {
		t.Fatalf("leading instruction must be kept, got role %q", got)
	}
	// Most recent message preserved.
	last := items[len(items)-1]
	if got := last.Get("content.0.text").String(); got != "m9" {
		t.Fatalf("most recent message must be kept, got %q", got)
	}
}

func TestReduceCombinedTrimsHistory(t *testing.T) {
	// No oversized content, but trimmable history -> the combined reduction still
	// trims the oldest history in a single pass.
	var sb strings.Builder
	sb.WriteString(`{"input":[`)
	sb.WriteString(`{"type":"message","role":"developer","content":[{"type":"input_text","text":"sys"}]}`)
	for i := 0; i < 10; i++ {
		sb.WriteString(fmt.Sprintf(`,{"type":"message","role":"user","content":[{"type":"input_text","text":"m%d"}]}`, i))
	}
	sb.WriteString(`]}`)
	body := []byte(sb.String())

	out, ok := reduceCodexResponsesBodyForOverflow(context.Background(), "test", body)
	if !ok {
		t.Fatalf("expected combined reduction to trim history")
	}
	if len(gjson.GetBytes(out, "input").Array()) >= 11 {
		t.Fatalf("expected history trimmed")
	}
}

func TestReduceReturnsFalseWhenNothingToReduce(t *testing.T) {
	body := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	if _, ok := reduceCodexResponsesBodyForOverflow(context.Background(), "test", body); ok {
		t.Fatalf("nothing to reduce should return false")
	}
}

func TestShrinkCodexToolDescriptions(t *testing.T) {
	big := strings.Repeat("d", codexToolDescCharCap+200)
	body := []byte(fmt.Sprintf(`{"tools":[{"type":"function","name":"f","description":%q,"parameters":{"type":"object"}}]}`, big))

	out, changed := shrinkCodexToolDescriptions(context.Background(), "test", body, codexToolDescCharCap)
	if !changed {
		t.Fatalf("expected tool description truncated")
	}
	// Tool kept, name and parameters intact.
	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "f" {
		t.Fatalf("tool name must be kept, got %q", got)
	}
	if got := gjson.GetBytes(out, "tools.0.parameters.type").String(); got != "object" {
		t.Fatalf("tool parameters must be kept")
	}
	if !strings.Contains(gjson.GetBytes(out, "tools.0.description").String(), "truncated") {
		t.Fatalf("description not truncated")
	}
}
