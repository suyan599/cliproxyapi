package misc

import _ "embed"

// QwenCodeSystemPrompt holds the embedded baseline Qwen Code system prompt
// used to align upstream Qwen chat-completions requests with current client
// expectations.
//
//go:embed qwen_code_system_prompt.txt
var QwenCodeSystemPrompt string
