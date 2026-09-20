// Package mcp is a small, dependency-free toolkit for exposing a DBX plugin's
// features as Model Context Protocol tools.
//
// DBX discovers plugin tools by calling the `mcp/tools` sidecar method and runs
// them through `mcp/call`, so a plugin only needs to build a registry and hand
// those two methods to its dispatcher. Nothing in this package knows about any
// particular plugin: copy `backend/mcp` into another plugin and register a
// different tool set.
package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Context is the per-call input handed to a tool handler.
type Context struct {
	// Lifecycle is the connection payload DBX fills in for the saved
	// connection. The host resolves secrets before the call, so handlers can
	// use it exactly like a connection lifecycle request.
	Lifecycle map[string]any
	// Arguments is the tool input object supplied by the model.
	Arguments map[string]any
}

// Handler executes one tool call. Returning an error produces an isError result
// rather than a protocol failure, which keeps the model in the loop.
type Handler func(ctx Context) (any, error)

// Tool is one exposed MCP tool.
type Tool struct {
	// Name is the stable tool identifier the model calls.
	Name string
	// Description is shown to the model when choosing a tool; write it for a
	// reader who has never seen this plugin.
	Description string
	// Schema is a JSON Schema object describing the arguments. Nil becomes an
	// empty object schema.
	Schema map[string]any
	// Handler runs the call.
	Handler Handler
}

// Registry holds the tools a plugin exposes.
type Registry struct {
	tools map[string]Tool
	order []string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}}
}

// Add registers a tool. Re-registering a name replaces the previous entry but
// keeps its original position, so a plugin can override a default tool.
func (registry *Registry) Add(tool Tool) {
	if strings.TrimSpace(tool.Name) == "" {
		panic("mcp: tool name must not be empty")
	}
	if tool.Handler == nil {
		panic("mcp: tool " + tool.Name + " needs a handler")
	}
	if _, exists := registry.tools[tool.Name]; !exists {
		registry.order = append(registry.order, tool.Name)
	}
	registry.tools[tool.Name] = tool
}

// Names lists the registered tool names in registration order.
func (registry *Registry) Names() []string {
	names := make([]string, len(registry.order))
	copy(names, registry.order)
	return names
}

// List renders the `mcp/tools` response.
func (registry *Registry) List() map[string]any {
	tools := make([]map[string]any, 0, len(registry.order))
	for _, name := range registry.order {
		tool := registry.tools[name]
		schema := tool.Schema
		if schema == nil {
			schema = ObjectSchema(nil, nil)
		}
		tools = append(tools, map[string]any{
			"name":        tool.Name,
			"description": tool.Description,
			"inputSchema": schema,
		})
	}
	return map[string]any{"tools": tools}
}

// Call renders the `mcp/call` response for one tool invocation. Unknown tools
// and handler failures are reported through the result body so the model can
// read the problem and try something else.
func (registry *Registry) Call(toolName string, arguments map[string]any, lifecycle map[string]any) map[string]any {
	tool, ok := registry.tools[strings.TrimSpace(toolName)]
	if !ok {
		return ErrorResult(fmt.Sprintf("unknown tool %q; available tools: %s", toolName, strings.Join(registry.Names(), ", ")))
	}
	if arguments == nil {
		arguments = map[string]any{}
	}
	value, err := tool.Handler(Context{Lifecycle: lifecycle, Arguments: arguments})
	if err != nil {
		return ErrorResult(err.Error())
	}
	return JSONResult(value)
}

// CallAndWrap is the convenience entry point for a sidecar dispatcher: it
// executes a tool and always yields a protocol-ready payload.
func CallAndWrap(registry *Registry, toolName string, params map[string]any) map[string]any {
	return registry.Call(toolName, MapArg(params, "arguments"), MapArg(params, "lifecycle"))
}

// encodeJSON keeps a single marshalling helper so every result shares the same
// formatting rules.
func encodeJSON(value any) (string, error) {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}
