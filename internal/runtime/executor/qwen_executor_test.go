package executor

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestResolveQwenUpstreamModel(t *testing.T) {
	tests := []struct {
		name  string
		model string
		want  string
	}{
		{name: "plus model uses upstream alias", model: "qwen3.5-plus", want: "coder-model"},
		{name: "flash model keeps upstream name", model: "qwen3.5-flash", want: "qwen3.5-flash"},
		{name: "legacy model stays valid", model: "coder-model", want: "coder-model"},
		{name: "unknown model passthrough", model: "custom-qwen-model", want: "custom-qwen-model"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveQwenUpstreamModel(tt.model); got != tt.want {
				t.Fatalf("resolveQwenUpstreamModel(%q) = %q, want %q", tt.model, got, tt.want)
			}
		})
	}
}

func TestEnsureQwenExplicitCacheControl_StringContent(t *testing.T) {
	// With 2 user turns: system gets cache, second-to-last user gets cache
	input := []byte(`{
		"model":"coder-model",
		"messages":[
			{"role":"system","content":"You are helpful"},
			{"role":"user","content":"First question"},
			{"role":"assistant","content":"First answer"},
			{"role":"user","content":"Second question"}
		]
	}`)

	output := ensureQwenExplicitCacheControl("qwen3.5-plus", input)

	// System message should have cache_control (promoted to array)
	if got := gjson.GetBytes(output, "messages.0.content.0.cache_control.type").String(); got != "ephemeral" {
		t.Fatalf("system cache_control.type = %q, want %q", got, "ephemeral")
	}
	if got := gjson.GetBytes(output, "messages.0.content.0.text").String(); got != "You are helpful" {
		t.Fatalf("system text = %q, want %q", got, "You are helpful")
	}

	// First user (second-to-last user) should have cache_control
	if got := gjson.GetBytes(output, "messages.1.content.0.cache_control.type").String(); got != "ephemeral" {
		t.Fatalf("second-to-last user cache_control.type = %q, want %q", got, "ephemeral")
	}

	// Last user message should NOT have cache_control
	if gjson.GetBytes(output, "messages.3.content.0.cache_control").Exists() {
		t.Fatal("last user message should NOT have cache_control")
	}
}

func TestEnsureQwenExplicitCacheControl_ArrayContent(t *testing.T) {
	input := []byte(`{
		"messages":[
			{"role":"system","content":[{"type":"text","text":"system prompt"}]},
			{"role":"user","content":[
				{"type":"text","text":"part 1"},
				{"type":"text","text":"part 2"}
			]},
			{"role":"assistant","content":"answer"},
			{"role":"user","content":"latest question"}
		]
	}`)

	output := ensureQwenExplicitCacheControl("coder-model", input)

	// System: last block (index 0) should have cache_control
	if got := gjson.GetBytes(output, "messages.0.content.0.cache_control.type").String(); got != "ephemeral" {
		t.Fatalf("system cache_control.type = %q, want %q", got, "ephemeral")
	}

	// Second-to-last user (index 1): cache on last content block (index 1)
	if gjson.GetBytes(output, "messages.1.content.0.cache_control").Exists() {
		t.Fatal("messages.1.content.0 should NOT have cache_control (only last block gets it)")
	}
	if got := gjson.GetBytes(output, "messages.1.content.1.cache_control.type").String(); got != "ephemeral" {
		t.Fatalf("messages.1.content.1.cache_control.type = %q, want %q", got, "ephemeral")
	}
}

func TestEnsureQwenExplicitCacheControl_PreservesExistingMarkers(t *testing.T) {
	input := []byte(`{
		"messages":[
			{"role":"system","content":[{"type":"text","text":"cached","cache_control":{"type":"ephemeral"}}]},
			{"role":"user","content":"first"},
			{"role":"assistant","content":"reply"},
			{"role":"user","content":"second"}
		]
	}`)

	output := ensureQwenExplicitCacheControl("qwen3.5-plus", input)

	// Existing marker preserved
	if got := gjson.GetBytes(output, "messages.0.content.0.cache_control.type").String(); got != "ephemeral" {
		t.Fatalf("existing cache_control lost: got %q", got)
	}
	// No new markers injected (payload already has cache_control)
	if gjson.GetBytes(output, "messages.1.content.0.cache_control").Exists() {
		t.Fatal("should not inject new cache_control when existing markers present")
	}
}

func TestEnsureQwenExplicitCacheControl_UnsupportedModel(t *testing.T) {
	input := []byte(`{"messages":[{"role":"user","content":"plain"}]}`)

	output := ensureQwenExplicitCacheControl("qwen3-max", input)

	if string(output) != string(input) {
		t.Fatalf("unsupported model payload changed:\n got: %s\nwant: %s", output, input)
	}
}

func TestEnsureQwenExplicitCacheControl_SingleUserTurn(t *testing.T) {
	// Only 1 user turn: system gets cache, but no message cache (nothing stable to cache)
	input := []byte(`{
		"messages":[
			{"role":"system","content":"You are helpful"},
			{"role":"user","content":"Only question"}
		]
	}`)

	output := ensureQwenExplicitCacheControl("qwen3.5-plus", input)

	// System should still get cache_control
	if got := gjson.GetBytes(output, "messages.0.content.0.cache_control.type").String(); got != "ephemeral" {
		t.Fatalf("system cache_control.type = %q, want %q", got, "ephemeral")
	}

	// Single user message should NOT get cache_control
	if gjson.GetBytes(output, "messages.1.content.0.cache_control").Exists() {
		t.Fatal("single user turn should not get cache_control")
	}
}

func TestEnsureQwenExplicitCacheControl_NoSystemMessage(t *testing.T) {
	// No system message, but 2 user turns: only second-to-last user gets cache
	input := []byte(`{
		"messages":[
			{"role":"user","content":"First"},
			{"role":"assistant","content":"Reply"},
			{"role":"user","content":"Second"}
		]
	}`)

	output := ensureQwenExplicitCacheControl("coder-model", input)

	// First user (second-to-last) should have cache_control
	if got := gjson.GetBytes(output, "messages.0.content.0.cache_control.type").String(); got != "ephemeral" {
		t.Fatalf("second-to-last user cache_control.type = %q, want %q", got, "ephemeral")
	}

	// Last user should NOT
	if gjson.GetBytes(output, "messages.2.content.0.cache_control").Exists() {
		t.Fatal("last user message should not get cache_control")
	}
}

func TestEnsureQwenSystemPrompt_InsertsFirstSystemMessage(t *testing.T) {
	input := []byte(`{
		"messages":[
			{"role":"user","content":"hello"}
		]
	}`)

	output := ensureQwenSystemPrompt(input)

	if got := gjson.GetBytes(output, "messages.0.role").String(); got != "system" {
		t.Fatalf("messages.0.role = %q, want %q", got, "system")
	}
	if got := gjson.GetBytes(output, "messages.1.role").String(); got != "user" {
		t.Fatalf("messages.1.role = %q, want %q", got, "user")
	}
	if got := gjson.GetBytes(output, "messages.0.content.0.text").String(); !strings.Contains(got, qwenSystemPromptKey) {
		t.Fatalf("messages.0.content.0.text missing qwen system prompt marker: %q", got)
	}
}

func TestEnsureQwenSystemPrompt_PrependsExistingFirstSystemMessage(t *testing.T) {
	input := []byte(`{
		"messages":[
			{"role":"system","content":"custom system"},
			{"role":"user","content":"hello"}
		]
	}`)

	output := ensureQwenSystemPrompt(input)

	if got := gjson.GetBytes(output, "messages.0.content.0.text").String(); !strings.Contains(got, qwenSystemPromptKey) {
		t.Fatalf("messages.0.content.0.text missing qwen system prompt marker: %q", got)
	}
	if got := gjson.GetBytes(output, "messages.0.content.1.text").String(); got != "custom system" {
		t.Fatalf("messages.0.content.1.text = %q, want %q", got, "custom system")
	}
}

func TestEnsureQwenSystemPrompt_DoesNotDuplicateExistingPrompt(t *testing.T) {
	input := []byte(`{
		"messages":[
			{"role":"system","content":[
				{"type":"text","text":"You are Qwen, an interactive agent developed by Alibaba Group, specializing in software engineering tasks."},
				{"type":"text","text":"custom system"}
			]},
			{"role":"user","content":"hello"}
		]
	}`)

	output := ensureQwenSystemPrompt(input)

	if string(output) != string(input) {
		t.Fatalf("payload changed even though qwen system prompt already existed:\n got: %s\nwant: %s", output, input)
	}
}
