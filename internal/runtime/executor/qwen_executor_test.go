package executor

import "testing"

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
