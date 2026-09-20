package mcp

// TextResult wraps plain text as a successful tool result.
func TextResult(text string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": false,
	}
}

// JSONResult serialises a value into the text channel. Values are pretty
// printed because the same payload often ends up in a transcript a human reads.
func JSONResult(value any) map[string]any {
	if value == nil {
		return TextResult("(no data)")
	}
	if text, ok := value.(string); ok {
		return TextResult(text)
	}
	encoded, err := encodeJSON(value)
	if err != nil {
		return ErrorResult("failed to encode the tool result: " + err.Error())
	}
	return TextResult(encoded)
}

// ErrorResult reports a failure the model is expected to read and react to.
func ErrorResult(message string) map[string]any {
	if message == "" {
		message = "the tool failed without a message"
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": message}},
		"isError": true,
	}
}
