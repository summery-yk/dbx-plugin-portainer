package main

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/summery-yk/dbx-plugin-portainer/backend/mcp"
	dbxpluginsdk "github.com/t8y2/dbx/plugins/sdk/go/dbx-plugin-sdk"
)

// The tools below are the plugin's MCP surface. Each one reuses the very same
// handler the sandboxed workbench calls, so behaviour cannot drift between the
// UI and the agent; only argument naming and result shaping differ.
//
// Design rules kept by every tool:
//   - read-only,
//   - defaults for everything optional, so a bare call always works,
//   - containers may be addressed by name or id (models rarely know ids),
//   - results are bounded, so one call cannot flood the model context.

const (
	defaultContainerLimit = 100
	maxContainerLimit     = 500
	defaultLogLines       = 200
	maxLogLines           = 5000
	mcpMaxLogBytes        = 256 << 10
)

func (p *plugin) registerMCPTools(registry *mcp.Registry) {
	registry.Add(mcp.Tool{
		Name: "portainer_overview",
		Description: "Summarise one Portainer environment: environment metadata plus counts of stacks, " +
			"containers, running containers, database containers and unhealthy containers. " +
			"Call this first to understand what lives in an environment.",
		Schema: mcp.ObjectSchema(map[string]any{
			"environmentId": mcp.Prop("integer", "Portainer environment (endpoint) id, e.g. 11"),
		}, []string{"environmentId"}),
		Handler: p.toolOverview,
	})

	registry.Add(mcp.Tool{
		Name: "portainer_list_environments",
		Description: "List every Portainer environment (endpoint) with its id, name, type, online status " +
			"and host. Use the returned id as environmentId in the other tools.",
		Schema: mcp.ObjectSchema(map[string]any{
			"includeOffline": mcp.PropWithDefault("boolean", "Include environments that are currently offline", true),
		}, nil),
		Handler: p.toolListEnvironments,
	})

	registry.Add(mcp.Tool{
		Name: "portainer_list_stacks",
		Description: "List Compose stacks in an environment. Containers are grouped by the " +
			"com.docker.compose.project label, which covers both Portainer-managed stacks and " +
			"Compose projects deployed outside Portainer.",
		Schema: mcp.ObjectSchema(map[string]any{
			"environmentId": mcp.Prop("integer", "Portainer environment id"),
			"includeEmpty":  mcp.PropWithDefault("boolean", "Also return stacks without containers", false),
		}, []string{"environmentId"}),
		Handler: p.toolListStacks,
	})

	registry.Add(mcp.Tool{
		Name: "portainer_list_containers",
		Description: "List containers in an environment with state, health, image, stack, service and " +
			"published ports. Supports filtering by stack, state, name, image and database containers only.",
		Schema: mcp.ObjectSchema(map[string]any{
			"environmentId":   mcp.Prop("integer", "Portainer environment id"),
			"stack":           mcp.Prop("string", "Only containers of this Compose project, e.g. bjjlml-test"),
			"state":           mcp.PropEnum("Only containers in this state", "all", "running", "stopped"),
			"onlyDatabases":   mcp.PropWithDefault("boolean", "Only database containers (MySQL, PostgreSQL, Redis, MongoDB, ...)", false),
			"nameContains":    mcp.Prop("string", "Case-insensitive substring match on the container name"),
			"imageContains":   mcp.Prop("string", "Case-insensitive substring match on the image reference"),
			"serviceContains": mcp.Prop("string", "Case-insensitive substring match on the Compose service name"),
			"limit":           mcp.PropWithDefault("integer", "Maximum containers to return (1-500)", defaultContainerLimit),
		}, []string{"environmentId"}),
		Handler: p.toolListContainers,
	})

	registry.Add(mcp.Tool{
		Name: "portainer_find_containers",
		Description: "Find containers by name across an environment. Returns id, name, stack, service, state " +
			"and health so you can pick the right container before inspecting it or reading its logs.",
		Schema: mcp.ObjectSchema(map[string]any{
			"environmentId": mcp.Prop("integer", "Portainer environment id"),
			"name":          mcp.Prop("string", "Container name or part of it, e.g. mysql"),
			"limit":         mcp.PropWithDefault("integer", "Maximum matches to return (1-100)", 20),
		}, []string{"environmentId", "name"}),
		Handler: p.toolFindContainers,
	})

	registry.Add(mcp.Tool{
		Name: "portainer_find_databases",
		Description: "Find database containers in an environment and derive ready-to-use DBX connection " +
			"parameters (host, published port, database type) for each one.",
		Schema: mcp.ObjectSchema(map[string]any{
			"environmentId": mcp.Prop("integer", "Portainer environment id"),
			"engine":        mcp.Prop("string", "Only this engine, e.g. mysql, postgres, redis, mongodb, elasticsearch"),
			"runningOnly":   mcp.PropWithDefault("boolean", "Exclude stopped database containers", true),
		}, []string{"environmentId"}),
		Handler: p.toolFindDatabases,
	})

	registry.Add(mcp.Tool{
		Name: "portainer_inspect_container",
		Description: "Return container details: image, state, restart policy, start/finish times, port " +
			"bindings, mounts, networks and environment variable names (values are never returned).",
		Schema: mcp.ObjectSchema(map[string]any{
			"environmentId": mcp.Prop("integer", "Portainer environment id"),
			"container":     mcp.Prop("string", "Container name or id"),
		}, []string{"environmentId", "container"}),
		Handler: p.toolInspectContainer,
	})

	registry.Add(mcp.Tool{
		Name: "portainer_container_logs",
		Description: "Read container logs. Supports a time range, a line limit, timestamps and a " +
			"case-insensitive substring filter; the multi-stream Docker framing is decoded for you.",
		Schema: mcp.ObjectSchema(map[string]any{
			"environmentId": mcp.Prop("integer", "Portainer environment id"),
			"container":     mcp.Prop("string", "Container name or id"),
			"tail":          mcp.PropWithDefault("integer", "Number of trailing lines to fetch (1-5000)", defaultLogLines),
			"range":         mcp.PropEnum("Time range to fetch", "all", "1h", "24h", "7d"),
			"since":         mcp.Prop("string", "Explicit start time (Docker since syntax, e.g. 2026-09-18T00:00:00Z); overrides range"),
			"timestamps":    mcp.PropWithDefault("boolean", "Prefix each line with its timestamp", true),
			"search":        mcp.Prop("string", "Only return lines containing this text (case-insensitive)"),
		}, []string{"environmentId", "container"}),
		Handler: p.toolContainerLogs,
	})

	registry.Add(mcp.Tool{
		Name: "portainer_container_stats",
		Description: "Sample one container's resource usage once: CPU percent, memory usage and limit, " +
			"network and block-device I/O, and process count.",
		Schema: mcp.ObjectSchema(map[string]any{
			"environmentId": mcp.Prop("integer", "Portainer environment id"),
			"container":     mcp.Prop("string", "Container name or id"),
		}, []string{"environmentId", "container"}),
		Handler: p.toolContainerStats,
	})
}

/* ------------------------------------------------------------------ helpers */

// mcpSession resolves the connection a tool call belongs to. DBX supplies the
// saved connection (including secrets) as the lifecycle payload, so a session
// is created on demand the first time an agent calls a tool for it.
func (p *plugin) mcpSession(ctx mcp.Context) (*session, error) {
	connectionID := connectionIDFrom(map[string]any{"connection": ctx.Lifecycle})
	if connectionID == "" {
		return nil, errors.New("this tool needs a Portainer connection, but DBX did not supply a connection for the call")
	}
	p.mutex.RLock()
	target, ok := p.sessions[connectionID]
	p.mutex.RUnlock()
	if ok {
		return target, nil
	}
	built, pluginError := buildSession(map[string]any{"connection": ctx.Lifecycle})
	if pluginError != nil {
		return nil, errors.New(pluginError.Message)
	}
	p.mutex.Lock()
	p.sessions[connectionID] = built
	p.mutex.Unlock()
	return built, nil
}

// mcpValues builds the request shape the existing handlers expect.
func (p *plugin) mcpValues(ctx mcp.Context) (map[string]any, error) {
	target, err := p.mcpSession(ctx)
	if err != nil {
		return nil, err
	}
	_ = target
	connectionID := connectionIDFrom(map[string]any{"connection": ctx.Lifecycle})
	return map[string]any{"connectionId": connectionID}, nil
}

func asError(pluginError *dbxpluginsdk.PluginError) error {
	if pluginError == nil {
		return nil
	}
	return errors.New(pluginError.Message)
}

// endpointHostFor resolves the host to advertise for an environment.
func (p *plugin) endpointHostFor(target *session, endpointID int) string {
	return p.lookupEndpointHost(target, endpointID)
}

// resolveContainerID accepts a container id (full or short) or a container
// name, so a model can address containers the way it saw them.
func resolveContainerID(target *session, endpointID int, reference string) (string, error) {
	trimmed := strings.TrimSpace(reference)
	if trimmed == "" {
		return "", errors.New("container name or id must not be empty")
	}
	if len(trimmed) >= 12 && isHexString(trimmed) {
		return trimmed, nil
	}
	body, pluginError := target.get(containerListPath(endpointID), url.Values{"all": {"1"}})
	if pluginError != nil {
		return "", asError(pluginError)
	}
	containers, pluginError := decodeContainers(body)
	if pluginError != nil {
		return "", asError(pluginError)
	}
	for _, container := range containers {
		if strings.EqualFold(container.displayName(), trimmed) {
			return container.ID, nil
		}
	}
	matches := make([]string, 0, 4)
	for _, container := range containers {
		if strings.Contains(strings.ToLower(container.displayName()), strings.ToLower(trimmed)) {
			matches = append(matches, container.displayName())
		}
	}
	if len(matches) == 1 {
		for _, container := range containers {
			if container.displayName() == matches[0] {
				return container.ID, nil
			}
		}
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("container name %q is ambiguous; matches: %s", reference, strings.Join(matches, ", "))
	}
	return "", fmt.Errorf("no container matches %q in environment %d", reference, endpointID)
}

func isHexString(value string) bool {
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') && (character < 'A' || character > 'F') {
			return false
		}
	}
	return true
}

func matchesState(state, filter string) bool {
	switch strings.ToLower(filter) {
	case "", "all":
		return true
	case "running":
		return state == "running" || state == "paused" || state == "restarting"
	case "stopped":
		return state == "exited" || state == "dead" || state == "created"
	}
	return true
}

func containsFold(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

/* -------------------------------------------------------------------- tools */

func (p *plugin) toolListEnvironments(ctx mcp.Context) (any, error) {
	values, err := p.mcpValues(ctx)
	if err != nil {
		return nil, err
	}
	result, pluginError := p.handleEndpoints(values)
	if pluginError != nil {
		return nil, asError(pluginError)
	}
	payload, _ := result.(map[string]any)
	items, _ := payload["items"].([]map[string]any)
	includeOffline := mcp.BoolArg(ctx.Arguments, "includeOffline", true)
	environments := make([]map[string]any, 0, len(items))
	for _, item := range items {
		status := toInt(item["status"])
		if !includeOffline && status != 1 {
			continue
		}
		environments = append(environments, map[string]any{
			"id":        toInt(item["id"]),
			"name":      toStr(item["name"]),
			"type":      toInt(item["type"]),
			"online":    status == 1,
			"url":       toStr(item["url"]),
			"publicUrl": toStr(item["publicUrl"]),
			"host":      toStr(item["host"]),
		})
	}
	return map[string]any{"count": len(environments), "environments": environments}, nil
}

func (p *plugin) toolListStacks(ctx mcp.Context) (any, error) {
	values, err := p.mcpValues(ctx)
	if err != nil {
		return nil, err
	}
	endpointID, err := mcp.RequireInt(ctx.Arguments, "environmentId")
	if err != nil {
		return nil, err
	}
	values["endpointId"] = endpointID
	result, pluginError := p.handleStacks(values)
	if pluginError != nil {
		return nil, asError(pluginError)
	}
	payload, _ := result.(map[string]any)
	items, _ := payload["items"].([]map[string]any)
	includeEmpty := mcp.BoolArg(ctx.Arguments, "includeEmpty", false)
	stacks := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if !includeEmpty && toInt(item["containers"]) == 0 {
			continue
		}
		stacks = append(stacks, map[string]any{
			"name":       toStr(item["name"]),
			"containers": toInt(item["containers"]),
			"running":    toInt(item["running"]),
			"services":   toInt(item["services"]),
			"health":     toStr(item["health"]),
		})
	}
	return map[string]any{"environmentId": endpointID, "count": len(stacks), "stacks": stacks}, nil
}

func (p *plugin) toolListContainers(ctx mcp.Context) (any, error) {
	values, err := p.mcpValues(ctx)
	if err != nil {
		return nil, err
	}
	endpointID, err := mcp.RequireInt(ctx.Arguments, "environmentId")
	if err != nil {
		return nil, err
	}
	stack := mcp.StringArg(ctx.Arguments, "stack", "")
	if stack != "" {
		values["endpointId"] = endpointID
		values["stack"] = stack
		result, pluginError := p.handleStackContainers(values)
		if pluginError != nil {
			return nil, asError(pluginError)
		}
		payload, _ := result.(map[string]any)
		items, _ := payload["items"].([]map[string]any)
		return p.shapeContainers(ctx, endpointID, items, stack)
	}
	values["endpointId"] = endpointID
	values["all"] = true
	result, pluginError := p.handleContainers(values)
	if pluginError != nil {
		return nil, asError(pluginError)
	}
	payload, _ := result.(map[string]any)
	items, _ := payload["items"].([]map[string]any)
	return p.shapeContainers(ctx, endpointID, items, "")
}

// shapeContainers applies the MCP-side filters and caps the result size.
func (p *plugin) shapeContainers(ctx mcp.Context, endpointID int, items []map[string]any, stack string) (any, error) {
	state := mcp.StringArg(ctx.Arguments, "state", "all")
	onlyDatabases := mcp.BoolArg(ctx.Arguments, "onlyDatabases", false)
	nameContains := mcp.StringArg(ctx.Arguments, "nameContains", "")
	imageContains := mcp.StringArg(ctx.Arguments, "imageContains", "")
	serviceContains := mcp.StringArg(ctx.Arguments, "serviceContains", "")
	limit := mcp.Clamp(mcp.IntArg(ctx.Arguments, "limit", defaultContainerLimit), 1, maxContainerLimit)

	containers := make([]map[string]any, 0, len(items))
	for _, item := range items {
		name := toStr(item["name"])
		image := toStr(item["image"])
		service := toStr(item["service"])
		if onlyDatabases && toStr(item["dbxType"]) == "" {
			continue
		}
		if !matchesState(toStr(item["state"]), state) {
			continue
		}
		if !containsFold(name, nameContains) || !containsFold(image, imageContains) || !containsFold(service, serviceContains) {
			continue
		}
		entry := map[string]any{
			"id":      toStr(item["id"]),
			"name":    name,
			"state":   toStr(item["state"]),
			"health":  toStr(item["health"]),
			"image":   image,
			"stack":   toStr(item["stack"]),
			"service": service,
			"ports":   item["ports"],
		}
		if engine := toStr(item["engine"]); engine != "" {
			entry["engine"] = engine
			entry["dbxType"] = toStr(item["dbxType"])
		}
		containers = append(containers, entry)
	}
	sort.Slice(containers, func(left, right int) bool {
		return toStr(containers[left]["name"]) < toStr(containers[right]["name"])
	})
	total := len(containers)
	truncated := false
	if total > limit {
		containers = containers[:limit]
		truncated = true
	}
	response := map[string]any{
		"environmentId": endpointID,
		"total":         total,
		"returned":      len(containers),
		"containers":    containers,
	}
	if stack != "" {
		response["stack"] = stack
	}
	if truncated {
		response["note"] = fmt.Sprintf("结果已截断到 %d 条，可用 nameContains / stack / state 缩小范围", limit)
	}
	return response, nil
}

func (p *plugin) toolFindContainers(ctx mcp.Context) (any, error) {
	values, err := p.mcpValues(ctx)
	if err != nil {
		return nil, err
	}
	endpointID, err := mcp.RequireInt(ctx.Arguments, "environmentId")
	if err != nil {
		return nil, err
	}
	needle, err := mcp.RequireString(ctx.Arguments, "name")
	if err != nil {
		return nil, err
	}
	limit := mcp.Clamp(mcp.IntArg(ctx.Arguments, "limit", 20), 1, 100)
	values["endpointId"] = endpointID
	values["all"] = true
	result, pluginError := p.handleContainers(values)
	if pluginError != nil {
		return nil, asError(pluginError)
	}
	payload, _ := result.(map[string]any)
	items, _ := payload["items"].([]map[string]any)
	matches := make([]map[string]any, 0, limit)
	for _, item := range items {
		if !containsFold(toStr(item["name"]), needle) {
			continue
		}
		matches = append(matches, map[string]any{
			"id":      toStr(item["id"]),
			"name":    toStr(item["name"]),
			"stack":   toStr(item["stack"]),
			"service": toStr(item["service"]),
			"state":   toStr(item["state"]),
			"health":  toStr(item["health"]),
			"image":   toStr(item["image"]),
		})
		if len(matches) >= limit {
			break
		}
	}
	return map[string]any{"environmentId": endpointID, "query": needle, "count": len(matches), "matches": matches}, nil
}

func (p *plugin) toolFindDatabases(ctx mcp.Context) (any, error) {
	values, err := p.mcpValues(ctx)
	if err != nil {
		return nil, err
	}
	endpointID, err := mcp.RequireInt(ctx.Arguments, "environmentId")
	if err != nil {
		return nil, err
	}
	engineFilter := strings.ToLower(mcp.StringArg(ctx.Arguments, "engine", ""))
	runningOnly := mcp.BoolArg(ctx.Arguments, "runningOnly", true)
	values["endpointId"] = endpointID
	result, pluginError := p.handleDatabases(values)
	if pluginError != nil {
		return nil, asError(pluginError)
	}
	payload, _ := result.(map[string]any)
	items, _ := payload["items"].([]map[string]any)
	databases := make([]map[string]any, 0, len(items))
	for _, item := range items {
		engine := strings.ToLower(toStr(item["engine"]))
		if engineFilter != "" && engine != engineFilter {
			continue
		}
		if runningOnly && toStr(item["state"]) != "running" {
			continue
		}
		entry := map[string]any{
			"containerName": toStr(item["containerName"]),
			"containerId":   toStr(item["containerId"]),
			"engine":        engine,
			"dbxType":       toStr(item["dbxType"]),
			"host":          toStr(item["host"]),
			"port":          toInt(item["port"]),
			"containerPort": toInt(item["privatePort"]),
			"state":         toStr(item["state"]),
			"image":         toStr(item["image"]),
		}
		databases = append(databases, entry)
	}
	return map[string]any{"environmentId": endpointID, "count": len(databases), "databases": databases}, nil
}

func (p *plugin) toolInspectContainer(ctx mcp.Context) (any, error) {
	values, err := p.mcpValues(ctx)
	if err != nil {
		return nil, err
	}
	endpointID, err := mcp.RequireInt(ctx.Arguments, "environmentId")
	if err != nil {
		return nil, err
	}
	reference, err := mcp.RequireString(ctx.Arguments, "container")
	if err != nil {
		return nil, err
	}
	target, err := p.mcpSession(ctx)
	if err != nil {
		return nil, err
	}
	containerID, err := resolveContainerID(target, endpointID, reference)
	if err != nil {
		return nil, err
	}
	values["endpointId"] = endpointID
	values["containerId"] = containerID
	result, pluginError := p.handleInspect(values)
	if pluginError != nil {
		return nil, asError(pluginError)
	}
	return result, nil
}

func (p *plugin) toolContainerLogs(ctx mcp.Context) (any, error) {
	values, err := p.mcpValues(ctx)
	if err != nil {
		return nil, err
	}
	endpointID, err := mcp.RequireInt(ctx.Arguments, "environmentId")
	if err != nil {
		return nil, err
	}
	reference, err := mcp.RequireString(ctx.Arguments, "container")
	if err != nil {
		return nil, err
	}
	target, err := p.mcpSession(ctx)
	if err != nil {
		return nil, err
	}
	containerID, err := resolveContainerID(target, endpointID, reference)
	if err != nil {
		return nil, err
	}
	tail := mcp.Clamp(mcp.IntArg(ctx.Arguments, "tail", defaultLogLines), 1, maxLogLines)
	values["endpointId"] = endpointID
	values["containerId"] = containerID
	values["tail"] = tail
	values["timestamps"] = mcp.BoolArg(ctx.Arguments, "timestamps", true)
	if since := mcp.StringArg(ctx.Arguments, "since", ""); since != "" {
		values["since"] = since
	} else {
		values["range"] = mcp.StringArg(ctx.Arguments, "range", "all")
	}
	result, pluginError := p.handleLogs(values)
	if pluginError != nil {
		return nil, asError(pluginError)
	}
	payload, _ := result.(map[string]any)
	text := toStr(payload["text"])
	lines := strings.Split(text, "\n")
	search := mcp.StringArg(ctx.Arguments, "search", "")
	matched := lines
	if search != "" {
		matched = make([]string, 0, len(lines))
		for _, line := range lines {
			if containsFold(line, search) {
				matched = append(matched, line)
			}
		}
	}
	joined := strings.Join(matched, "\n")
	truncatedBytes := false
	if len(joined) > mcpMaxLogBytes {
		joined = joined[len(joined)-mcpMaxLogBytes:]
		truncatedBytes = true
	}
	response := map[string]any{
		"environmentId": endpointID,
		"container":     reference,
		"containerId":   containerID,
		"lines":         len(matched),
		"totalLines":    len(lines),
		"logs":          joined,
	}
	if search != "" {
		response["search"] = search
	}
	if truncatedBytes {
		response["note"] = "日志过长，已保留末尾部分；可减小 tail 或使用 since/range 缩小范围"
	}
	return response, nil
}

func (p *plugin) toolContainerStats(ctx mcp.Context) (any, error) {
	values, err := p.mcpValues(ctx)
	if err != nil {
		return nil, err
	}
	endpointID, err := mcp.RequireInt(ctx.Arguments, "environmentId")
	if err != nil {
		return nil, err
	}
	reference, err := mcp.RequireString(ctx.Arguments, "container")
	if err != nil {
		return nil, err
	}
	target, err := p.mcpSession(ctx)
	if err != nil {
		return nil, err
	}
	containerID, err := resolveContainerID(target, endpointID, reference)
	if err != nil {
		return nil, err
	}
	values["endpointId"] = endpointID
	values["containerId"] = containerID
	result, pluginError := p.handleStats(values)
	if pluginError != nil {
		return nil, asError(pluginError)
	}
	payload, _ := result.(map[string]any)
	return map[string]any{
		"environmentId": endpointID,
		"container":     reference,
		"containerId":   containerID,
		"cpuPercent":    fmt.Sprintf("%.2f%%", numberFrom(payload["cpuPercent"])),
		"memoryUsage":   formatBytes(numberFrom(payload["memoryUsage"])),
		"memoryLimit":   formatBytes(numberFrom(payload["memoryLimit"])),
		"memoryPercent": fmt.Sprintf("%.1f%%", numberFrom(payload["memoryPercent"])),
		"networkRx":     formatBytes(numberFrom(payload["networkRx"])),
		"networkTx":     formatBytes(numberFrom(payload["networkTx"])),
		"blockRead":     formatBytes(numberFrom(payload["blockRead"])),
		"blockWrite":    formatBytes(numberFrom(payload["blockWrite"])),
		"pids":          toInt(payload["pids"]),
		"sampledAt":     toStr(payload["read"]),
	}, nil
}

func (p *plugin) toolOverview(ctx mcp.Context) (any, error) {
	values, err := p.mcpValues(ctx)
	if err != nil {
		return nil, err
	}
	endpointID, err := mcp.RequireInt(ctx.Arguments, "environmentId")
	if err != nil {
		return nil, err
	}
	target, err := p.mcpSession(ctx)
	if err != nil {
		return nil, err
	}
	values["endpointId"] = endpointID
	values["all"] = true

	containersResult, pluginError := p.handleContainers(values)
	if pluginError != nil {
		return nil, asError(pluginError)
	}
	containersPayload, _ := containersResult.(map[string]any)
	containers, _ := containersPayload["items"].([]map[string]any)

	stacksResult, pluginError := p.handleStacks(values)
	if pluginError != nil {
		return nil, asError(pluginError)
	}
	stacksPayload, _ := stacksResult.(map[string]any)
	stacks, _ := stacksPayload["items"].([]map[string]any)

	databasesResult, pluginError := p.handleDatabases(values)
	if pluginError != nil {
		return nil, asError(pluginError)
	}
	databasesPayload, _ := databasesResult.(map[string]any)
	databases, _ := databasesPayload["items"].([]map[string]any)

	running := 0
	unhealthy := 0
	for _, container := range containers {
		if toStr(container["state"]) == "running" {
			running++
		}
		if toStr(container["health"]) == "unhealthy" {
			unhealthy++
		}
	}
	engines := map[string]int{}
	for _, database := range databases {
		engines[toStr(database["engine"])]++
	}
	return map[string]any{
		"environmentId":    endpointID,
		"host":             p.endpointHostFor(target, endpointID),
		"containers":       len(containers),
		"running":          running,
		"unhealthy":        unhealthy,
		"stacks":           len(stacks),
		"databaseCount":    len(databases),
		"databaseEngines":  engines,
		"databaseHostPort": toStr(databasesPayload["host"]),
	}, nil
}

func formatBytes(value float64) string {
	const unit = 1024
	if value < unit {
		return fmt.Sprintf("%.0f B", value)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	size := value
	index := -1
	for size >= unit && index < len(units)-1 {
		size /= unit
		index++
	}
	return fmt.Sprintf("%.1f %s", size, units[index])
}
