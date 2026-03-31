package executor

import (
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
	input := []byte(`{
		"model":"coder-model",
		"messages":[
			{"role":"system","content":"You are helpful"},
			{"role":"user","content":"Cache this prompt"}
		]
	}`)

	output := ensureQwenExplicitCacheControl("qwen3.5-plus", input)

	if got := gjson.GetBytes(output, "messages.1.content.0.cache_control.type").String(); got != "ephemeral" {
		t.Fatalf("messages.1.content.0.cache_control.type = %q, want %q", got, "ephemeral")
	}
	if got := gjson.GetBytes(output, "messages.1.content.0.text").String(); got != "Cache this prompt" {
		t.Fatalf("messages.1.content.0.text = %q, want %q", got, "Cache this prompt")
	}
}

func TestEnsureQwenExplicitCacheControl_ArrayContent(t *testing.T) {
	input := []byte(`{
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"part 1"},
				{"type":"text","text":"part 2"}
			]}
		]
	}`)

	output := ensureQwenExplicitCacheControl("coder-model", input)

	if gjson.GetBytes(output, "messages.0.content.0.cache_control").Exists() {
		t.Fatal("messages.0.content.0.cache_control should not be injected")
	}
	if got := gjson.GetBytes(output, "messages.0.content.1.cache_control.type").String(); got != "ephemeral" {
		t.Fatalf("messages.0.content.1.cache_control.type = %q, want %q", got, "ephemeral")
	}
}

func TestEnsureQwenExplicitCacheControl_PreservesExistingMarkers(t *testing.T) {
	input := []byte(`{
		"messages":[
			{"role":"system","content":[{"type":"text","text":"cached","cache_control":{"type":"ephemeral"}}]},
			{"role":"user","content":"do not touch"}
		]
	}`)

	output := ensureQwenExplicitCacheControl("qwen3.5-plus", input)

	if got := gjson.GetBytes(output, "messages.0.content.0.cache_control.type").String(); got != "ephemeral" {
		t.Fatalf("existing cache_control lost: got %q", got)
	}
	if gjson.GetBytes(output, "messages.1.content.0.cache_control").Exists() {
		t.Fatal("new cache_control should not be injected when one already exists")
	}
}

func TestEnsureQwenExplicitCacheControl_UnsupportedModel(t *testing.T) {
	input := []byte(`{"messages":[{"role":"user","content":"plain"}]}`)

	output := ensureQwenExplicitCacheControl("qwen3-max", input)

	if string(output) != string(input) {
		t.Fatalf("unsupported model payload changed:\n got: %s\nwant: %s", output, input)
	}
}
