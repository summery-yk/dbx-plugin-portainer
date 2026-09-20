package mcp

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// ObjectSchema builds a JSON Schema object for a tool's arguments.
func ObjectSchema(properties map[string]any, required []string) map[string]any {
	if properties == nil {
		properties = map[string]any{}
	}
	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// Prop describes one argument.
func Prop(kind, description string) map[string]any {
	property := map[string]any{"type": kind, "description": description}
	return property
}

// PropWithDefault describes an argument with a default value, so the model can
// omit it.
func PropWithDefault(kind, description string, fallback any) map[string]any {
	property := Prop(kind, description)
	property["default"] = fallback
	return property
}

// PropEnum describes an enumerated string argument.
func PropEnum(description string, values ...string) map[string]any {
	property := map[string]any{"type": "string", "description": description}
	if len(values) > 0 {
		property["enum"] = values
	}
	return property
}

// MapArg returns a nested object argument, or nil when absent.
func MapArg(args map[string]any, key string) map[string]any {
	if args == nil {
		return nil
	}
	if nested, ok := args[key].(map[string]any); ok {
		return nested
	}
	return nil
}

// StringArg reads a string argument, returning fallback when absent or blank.
func StringArg(args map[string]any, key, fallback string) string {
	if args == nil {
		return fallback
	}
	value, ok := args[key]
	if !ok {
		return fallback
	}
	text, ok := value.(string)
	if !ok {
		return fallback
	}
	if trimmed := strings.TrimSpace(text); trimmed != "" {
		return trimmed
	}
	return fallback
}

// StringSliceArg reads a string array argument.
func StringSliceArg(args map[string]any, key string) []string {
	if args == nil {
		return nil
	}
	raw, ok := args[key]
	if !ok {
		return nil
	}
	switch typed := raw.(type) {
	case []string:
		return typed
	case []any:
		values := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
				values = append(values, strings.TrimSpace(text))
			}
		}
		return values
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return nil
		}
		parts := strings.Split(trimmed, ",")
		values := make([]string, 0, len(parts))
		for _, part := range parts {
			if value := strings.TrimSpace(part); value != "" {
				values = append(values, value)
			}
		}
		return values
	}
	return nil
}

// IntArg reads an integer argument, accepting JSON numbers and numeric strings
// because models sometimes send "100" instead of 100.
func IntArg(args map[string]any, key string, fallback int) int {
	if args == nil {
		return fallback
	}
	raw, ok := args[key]
	if !ok {
		return fallback
	}
	switch typed := raw.(type) {
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return fallback
		}
		return int(typed)
	case int:
		return typed
	case json.Number:
		if parsed, err := typed.Int64(); err == nil {
			return int(parsed)
		}
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return fallback
		}
		var parsed int
		if _, err := fmt.Sscanf(trimmed, "%d", &parsed); err == nil {
			return parsed
		}
	}
	return fallback
}

// BoolArg reads a boolean argument.
func BoolArg(args map[string]any, key string, fallback bool) bool {
	if args == nil {
		return fallback
	}
	raw, ok := args[key]
	if !ok {
		return fallback
	}
	switch typed := raw.(type) {
	case bool:
		return typed
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "true", "1", "yes", "on":
			return true
		case "false", "0", "no", "off":
			return false
		}
	case float64:
		return typed != 0
	}
	return fallback
}

// Clamp constrains value to the inclusive range [low, high].
func Clamp(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

// RequireString reads a mandatory string argument.
func RequireString(args map[string]any, key string) (string, error) {
	value := StringArg(args, key, "")
	if value == "" {
		return "", fmt.Errorf("argument %q is required", key)
	}
	return value, nil
}

// RequireInt reads a mandatory positive integer argument.
func RequireInt(args map[string]any, key string) (int, error) {
	value := IntArg(args, key, 0)
	if value <= 0 {
		return 0, fmt.Errorf("argument %q is required and must be a positive integer", key)
	}
	return value, nil
}
