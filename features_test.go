package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// sanitizeBlockedTemplates
// ---------------------------------------------------------------------------

func TestSanitize_ClaudeCodeIdentity(t *testing.T) {
	in := "You are Claude Code, Anthropic's official CLI for Claude."
	out := sanitizeBlockedTemplates(in)
	if strings.Contains(out, "CLI for Claude") {
		t.Errorf("expected 'CLI for Claude' rewritten, got: %q", out)
	}
	if !strings.Contains(out, "CLI tool for Claude") {
		t.Errorf("expected 'CLI tool for Claude' in output, got: %q", out)
	}
}

func TestSanitize_CodexCLIIdentity(t *testing.T) {
	in := "You are Codex, Anthropic's official CLI for Claude."
	out := sanitizeBlockedTemplates(in)
	if !strings.Contains(out, "CLI tool for Claude") {
		t.Errorf("expected Codex identity rewritten, got: %q", out)
	}
}

func TestSanitize_MainBranch(t *testing.T) {
	in := "Main branch (you will usually use this for PRs)"
	out := sanitizeBlockedTemplates(in)
	if strings.Contains(out, "Main branch") {
		t.Errorf("expected 'Main branch' replaced, got: %q", out)
	}
	if !strings.Contains(out, "Default branch") {
		t.Errorf("expected 'Default branch' in output, got: %q", out)
	}
}

func TestSanitize_FeedbackSentenceRemoved(t *testing.T) {
	in := "Use the feedback tool to give feedback to Anthropic."
	out := sanitizeBlockedTemplates(in)
	if strings.Contains(out, "give feedback to Anthropic") {
		t.Errorf("expected feedback sentence removed, got: %q", out)
	}
}

func TestSanitize_11128AntiProbe(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"error code 11128 occurred", "error code 11-128 occurred"},
		{"11128", "11-128"},
		{"prefix11128suffix", "prefix11-128suffix"},
	}
	for _, c := range cases {
		out := sanitizeBlockedTemplates(c.in)
		if out != c.want {
			t.Errorf("input=%q: want %q, got %q", c.in, c.want, out)
		}
	}
}

func TestSanitize_XAnthropicHeaderStripped(t *testing.T) {
	in := "header x-anthropic-billing-header:abc123 value"
	out := sanitizeBlockedTemplates(in)
	if strings.Contains(out, "x-anthropic") {
		t.Errorf("expected x-anthropic header stripped, got: %q", out)
	}
}

func TestSanitize_CcKVStripped(t *testing.T) {
	in := "some cc_session=xyz123; more text"
	out := sanitizeBlockedTemplates(in)
	if strings.Contains(out, "cc_") {
		t.Errorf("expected cc_ kv stripped, got: %q", out)
	}
	if !strings.Contains(out, "some") || !strings.Contains(out, "more text") {
		t.Errorf("surrounding text should be preserved, got: %q", out)
	}
}

func TestSanitize_NopOnCleanString(t *testing.T) {
	in := "Hello, world! This is a normal message."
	out := sanitizeBlockedTemplates(in)
	if out != in {
		t.Errorf("clean string should be unchanged; got: %q", out)
	}
}

// ---------------------------------------------------------------------------
// rewriteSystemForUpstream — developer→system, max_completion_tokens→max_tokens
// ---------------------------------------------------------------------------

func TestRewriteSystem_DeveloperRoleNormalized(t *testing.T) {
	payload := `{"model":"glm-5.2","messages":[{"role":"developer","content":"be helpful"}]}`
	out := rewriteSystemForUpstream([]byte(payload))
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	msgs := obj["messages"].([]any)
	role := msgs[0].(map[string]any)["role"].(string)
	if role != "system" {
		t.Errorf("expected role 'system', got %q", role)
	}
}

func TestRewriteSystem_MaxCompletionTokensTranslated(t *testing.T) {
	payload := `{"model":"glm-5.2","messages":[],"max_completion_tokens":4096}`
	out := rewriteSystemForUpstream([]byte(payload))
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if _, ok := obj["max_completion_tokens"]; ok {
		t.Error("max_completion_tokens should have been removed")
	}
	mt, ok := obj["max_tokens"]
	if !ok {
		t.Fatal("max_tokens should be set")
	}
	// JSON numbers unmarshal as float64.
	if mt.(float64) != 4096 {
		t.Errorf("expected max_tokens=4096, got %v", mt)
	}
}

func TestRewriteSystem_MaxTokensNotOverwritten(t *testing.T) {
	// When max_tokens is already present, it must not be overwritten.
	payload := `{"model":"glm-5.2","messages":[],"max_tokens":1024,"max_completion_tokens":4096}`
	out := rewriteSystemForUpstream([]byte(payload))
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if obj["max_tokens"].(float64) != 1024 {
		t.Errorf("existing max_tokens must not be overwritten, got %v", obj["max_tokens"])
	}
}

// ---------------------------------------------------------------------------
// repackToolResultBlocks
// ---------------------------------------------------------------------------

func msgs(roles ...string) []any {
	out := make([]any, len(roles))
	for i, r := range roles {
		m := map[string]any{"role": r, "content": r + "_content"}
		if r == "assistant_tc" {
			// assistant with a tool_calls array
			m["role"] = "assistant"
			m["tool_calls"] = []any{map[string]any{"id": "c1", "type": "function"}}
		}
		out[i] = m
	}
	return out
}

func TestRepack_NoChange_WhenClean(t *testing.T) {
	// [assistant_tc, tool, tool] → unchanged
	input := []any{
		map[string]any{"role": "assistant", "content": "hi", "tool_calls": []any{map[string]any{"id": "c1"}}},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "result"},
	}
	_, changed := repackToolResultBlocks(input)
	if changed {
		t.Error("expected no change for already-clean ordering")
	}
}

func TestRepack_MovesInterleavedUser(t *testing.T) {
	// Scenario: a non-map element (e.g. a bare string) is interleaved inside a
	// tool-result block immediately after an assistant-with-tool_calls message.
	// repack must move such elements to after the last tool result.
	stray := "unexpected-non-map-element"
	input := []any{
		map[string]any{
			"role":       "assistant",
			"tool_calls": []any{map[string]any{"id": "c1"}},
		},
		stray, // non-map interleaved element — triggers interleaved branch
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "r1"},
	}
	out, changed := repackToolResultBlocks(input)
	if !changed {
		t.Fatal("expected repack to detect and move interleaved non-map element")
	}
	// Result must be: [assistant_tc, tool, stray] — tool before stray.
	if len(out) != 3 {
		t.Fatalf("expected 3 elements, got %d", len(out))
	}
	// Second element should now be the tool message, stray should be last.
	role, _ := out[1].(map[string]any)["role"].(string)
	if role != "tool" {
		t.Errorf("expected tool message at index 1 after repack, got: %v", out[1])
	}
	if out[2] != stray {
		t.Errorf("expected stray element at index 2 after repack, got: %v", out[2])
	}
}

// ---------------------------------------------------------------------------
// cleanupOrphanToolCalls
// ---------------------------------------------------------------------------

func TestCleanup_SymmetricPairPreserved(t *testing.T) {
	input := []any{
		map[string]any{
			"role":       "assistant",
			"tool_calls": []any{map[string]any{"id": "c1", "type": "function"}},
		},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "result"},
	}
	out, changed := cleanupOrphanToolCalls(input)
	if changed {
		t.Error("symmetric pair must not be changed")
	}
	if len(out) != 2 {
		t.Errorf("expected 2 messages, got %d", len(out))
	}
}

func TestCleanup_OrphanResultDropped(t *testing.T) {
	// Tool result with no matching call → drop it.
	input := []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "tool", "tool_call_id": "ghost", "content": "orphan"},
	}
	out, changed := cleanupOrphanToolCalls(input)
	if !changed {
		t.Fatal("expected orphan result to be pruned")
	}
	for _, m := range out {
		if m.(map[string]any)["role"] == "tool" {
			t.Error("orphan tool message should have been removed")
		}
	}
}

func TestCleanup_OrphanCallPruned(t *testing.T) {
	// Assistant has two calls; only one has a matching result.
	input := []any{
		map[string]any{
			"role": "assistant",
			"tool_calls": []any{
				map[string]any{"id": "c1", "type": "function"},
				map[string]any{"id": "c2", "type": "function"}, // orphan
			},
		},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "r1"},
	}
	out, changed := cleanupOrphanToolCalls(input)
	if !changed {
		t.Fatal("expected orphan call to be pruned")
	}
	asst := out[0].(map[string]any)
	tcs := asst["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Errorf("expected 1 remaining tool_call, got %d", len(tcs))
	}
	if tcs[0].(map[string]any)["id"] != "c1" {
		t.Error("wrong tool_call retained")
	}
}

func TestCleanup_EmptyToolCallsKeyRemoved(t *testing.T) {
	// All calls are orphans → tool_calls key deleted from assistant message.
	input := []any{
		map[string]any{
			"role": "assistant",
			"tool_calls": []any{
				map[string]any{"id": "c1", "type": "function"},
			},
		},
		// No tool result message at all.
	}
	out, _ := cleanupOrphanToolCalls(input)
	asst := out[0].(map[string]any)
	if _, ok := asst["tool_calls"]; ok {
		t.Error("tool_calls should be deleted when all calls are orphans")
	}
}

// ---------------------------------------------------------------------------
// isGlobalDomain
// ---------------------------------------------------------------------------

func TestIsGlobalDomain(t *testing.T) {
	cases := []struct {
		domain string
		want   bool
	}{
		{"www.workbuddy.ai", true},
		{"api.workbuddy.ai", true},
		{"copilot.tencent.com", false},
		{"workbuddy.ai", false}, // bare apex — not www or sub
		{"www.codebuddy.cn", false},
		{"", false},
	}
	for _, c := range cases {
		got := isGlobalDomain(c.domain)
		if got != c.want {
			t.Errorf("isGlobalDomain(%q) = %v, want %v", c.domain, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// nonChatModel
// ---------------------------------------------------------------------------

func TestNonChatModel(t *testing.T) {
	cases := []struct {
		e    dynModelEntry
		want bool
	}{
		{dynModelEntry{ID: "nes-v3"}, true},
		{dynModelEntry{ID: "completion-fast"}, true},
		{dynModelEntry{ID: "codewise-base"}, true},
		{dynModelEntry{ID: "glm-5.2", MaxOutputTokens: 8192}, false},
		{dynModelEntry{ID: "glm-5.2", MaxOutputTokens: 200}, true},   // embedding-sized
		{dynModelEntry{ID: "glm-5.2", MaxOutputTokens: 256}, true},   // boundary
		{dynModelEntry{ID: "glm-5.2", MaxOutputTokens: 257}, false},  // just over boundary
		{dynModelEntry{ID: "sdxl", Tags: []string{"text-to-image"}}, true},
		{dynModelEntry{ID: "glm-5.2", Tags: []string{"chat", "reasoning"}}, false},
	}
	for _, c := range cases {
		got := nonChatModel(c.e)
		if got != c.want {
			t.Errorf("nonChatModel(%+v) = %v, want %v", c.e, got, c.want)
		}
	}
}
