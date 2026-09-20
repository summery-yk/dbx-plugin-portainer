// Package main implements the DBX Portainer plugin sidecar.
//
// The sidecar speaks the DBX Sidecar Protocol v1 over stdio (JSON Lines) and
// exposes a read-only view of a Portainer instance: environments, containers,
// port mappings and container logs.
package main

import (
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/summery-yk/dbx-plugin-portainer/backend/mcp"
	dbxpluginsdk "github.com/t8y2/dbx/plugins/sdk/go/dbx-plugin-sdk"
)

const (
	pluginID        = "io.github.summery-yk.portainer"
	pluginVersion   = "0.2.0"
	requestTimeout  = 25 * time.Second
	maxResponseBody = 8 << 20
	maxLogBytes     = 512 << 10
	defaultLogTail  = 200
	maxLogTail      = 5000
)

// session holds the runtime state of one Portainer connection.
type session struct {
	baseURL string
	token   string
	client  *http.Client
}

// plugin is the sidecar request handler.
type plugin struct {
	mutex    sync.RWMutex
	sessions map[string]*session
	// tools is the MCP surface exposed to AI clients through DBX.
	tools *mcp.Registry
}

func main() {
	metadata := dbxpluginsdk.Metadata{
		ID:           pluginID,
		Version:      pluginVersion,
		Capabilities: []string{"connections", "portainer.read"},
	}
	instance := &plugin{sessions: map[string]*session{}, tools: mcp.NewRegistry()}
	instance.registerMCPTools(instance.tools)
	server := dbxpluginsdk.NewServer(metadata, instance)
	if err := server.Serve(); err != nil {
		log.Fatal(err)
	}
}

// Handle dispatches one sidecar request.
func (p *plugin) Handle(_ dbxpluginsdk.RequestContext, method string, params json.RawMessage, _ *dbxpluginsdk.Emitter) (any, *dbxpluginsdk.PluginError) {
	values := decodeParams(params)
	switch method {
	case "connection/test":
		return p.handleTest(values)
	case "connection/connect":
		return p.handleConnect(values)
	case "connection/disconnect":
		return p.handleDisconnect(values)
	case "mcp/tools":
		return p.tools.List(), nil
	case "mcp/call":
		return mcp.CallAndWrap(p.tools, toStr(values["tool"]), values), nil
	case "portainer/sessions":
		return p.handleSessions()
	case "portainer/endpoints":
		return p.handleEndpoints(values)
	case "portainer/containers":
		return p.handleContainers(values)
	case "portainer/inspect":
		return p.handleInspect(values)
	case "portainer/logs":
		return p.handleLogs(values)
	case "portainer/stacks":
		return p.handleStacks(values)
	case "portainer/stackContainers":
		return p.handleStackContainers(values)
	case "portainer/stats":
		return p.handleStats(values)
	case "portainer/databases":
		return p.handleDatabases(values)
	default:
		return nil, dbxpluginsdk.MethodNotFound(method)
	}
}

func (p *plugin) handleTest(values map[string]any) (any, *dbxpluginsdk.PluginError) {
	target, pluginError := buildSession(values)
	if pluginError != nil {
		return failure(pluginError.Message), nil
	}
	body, pluginError := target.get("/api/system/status", nil)
	if pluginError != nil {
		return failure(pluginError.Message), nil
	}
	var status struct {
		Version string `json:"Version"`
	}
	_ = json.Unmarshal(body, &status)

	message := "Connected to Portainer"
	if status.Version != "" {
		message = "Connected to Portainer " + status.Version
	}
	endpointsBody, pluginError := target.get("/api/endpoints", nil)
	if pluginError != nil {
		return failure(pluginError.Message), nil
	}
	var endpoints []map[string]any
	if err := json.Unmarshal(endpointsBody, &endpoints); err == nil {
		message += fmt.Sprintf(", %d environments available", len(endpoints))
	}
	return map[string]any{"success": true, "message": message}, nil
}

func (p *plugin) handleConnect(values map[string]any) (any, *dbxpluginsdk.PluginError) {
	connectionID := connectionIDFrom(values)
	if connectionID == "" {
		return nil, dbxpluginsdk.NewError(-32602, "Missing connection id")
	}
	target, pluginError := buildSession(values)
	if pluginError != nil {
		return failure(pluginError.Message), nil
	}
	if _, pluginError := target.get("/api/system/status", nil); pluginError != nil {
		return failure(pluginError.Message), nil
	}
	p.mutex.Lock()
	p.sessions[connectionID] = target
	p.mutex.Unlock()
	return map[string]any{"success": true}, nil
}

func (p *plugin) handleDisconnect(values map[string]any) (any, *dbxpluginsdk.PluginError) {
	connectionID := connectionIDFrom(values)
	if connectionID == "" {
		return nil, dbxpluginsdk.NewError(-32602, "Missing connection id")
	}
	p.mutex.Lock()
	delete(p.sessions, connectionID)
	p.mutex.Unlock()
	return map[string]any{"success": true}, nil
}

func (p *plugin) handleSessions() (any, *dbxpluginsdk.PluginError) {
	p.mutex.RLock()
	defer p.mutex.RUnlock()
	connectionIDs := make([]string, 0, len(p.sessions))
	for connectionID := range p.sessions {
		connectionIDs = append(connectionIDs, connectionID)
	}
	return map[string]any{"connectionIds": connectionIDs}, nil
}

func (p *plugin) handleEndpoints(values map[string]any) (any, *dbxpluginsdk.PluginError) {
	target, pluginError := p.pickSession(values)
	if pluginError != nil {
		return nil, pluginError
	}
	body, pluginError := target.get("/api/endpoints", nil)
	if pluginError != nil {
		return nil, pluginError
	}
	var raw []map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, dbxpluginsdk.NewError(-32000, "Unexpected response from the Portainer endpoints API")
	}
	items := make([]map[string]any, 0, len(raw))
	for _, endpoint := range raw {
		items = append(items, map[string]any{
			"id":        toInt(endpoint["Id"]),
			"name":      toStr(endpoint["Name"]),
			"type":      toInt(endpoint["Type"]),
			"status":    toInt(endpoint["Status"]),
			"url":       toStr(endpoint["URL"]),
			"publicUrl": toStr(endpoint["PublicURL"]),
			"host":      endpointHost(endpoint),
		})
	}
	return map[string]any{"items": items}, nil
}

func (p *plugin) handleContainers(values map[string]any) (any, *dbxpluginsdk.PluginError) {
	target, pluginError := p.pickSession(values)
	if pluginError != nil {
		return nil, pluginError
	}
	endpointID := toInt(values["endpointId"])
	if endpointID == 0 {
		return nil, dbxpluginsdk.NewError(-32602, "Missing endpointId")
	}
	query := url.Values{}
	if toBool(values["all"]) {
		query.Set("all", "1")
	}
	body, pluginError := target.get(containerListPath(endpointID), query)
	if pluginError != nil {
		return nil, pluginError
	}
	containers, pluginError := decodeContainers(body)
	if pluginError != nil {
		return nil, pluginError
	}
	return map[string]any{"items": containerItems(containers)}, nil
}

func (p *plugin) handleInspect(values map[string]any) (any, *dbxpluginsdk.PluginError) {
	target, pluginError := p.pickSession(values)
	if pluginError != nil {
		return nil, pluginError
	}
	endpointID := toInt(values["endpointId"])
	containerID := toStr(values["containerId"])
	if endpointID == 0 || containerID == "" {
		return nil, dbxpluginsdk.NewError(-32602, "Missing endpointId or containerId")
	}
	path := fmt.Sprintf("/api/endpoints/%d/docker/containers/%s/json", endpointID, url.PathEscape(containerID))
	body, pluginError := target.get(path, nil)
	if pluginError != nil {
		return nil, pluginError
	}
	var detail map[string]any
	if err := json.Unmarshal(body, &detail); err != nil {
		return nil, dbxpluginsdk.NewError(-32000, "Unexpected response from the Portainer inspect API")
	}
	result := map[string]any{
		"id":       toStr(detail["Id"]),
		"name":     strings.TrimPrefix(toStr(detail["Name"]), "/"),
		"env":      envKeys(detail),
		"ports":    inspectPorts(detail),
		"mounts":   inspectMounts(detail),
		"networks": inspectNetworks(detail),
	}
	if config, ok := detail["Config"].(map[string]any); ok {
		result["image"] = toStr(config["Image"])
	}
	if state, ok := detail["State"].(map[string]any); ok {
		result["state"] = toStr(state["Status"])
		result["running"] = toBool(state["Running"])
		result["startedAt"] = toStr(state["StartedAt"])
		result["finishedAt"] = toStr(state["FinishedAt"])
	}
	if hostConfig, ok := detail["HostConfig"].(map[string]any); ok {
		if policy, ok := hostConfig["RestartPolicy"].(map[string]any); ok {
			result["restartPolicy"] = toStr(policy["Name"])
		}
	}
	return result, nil
}

func (p *plugin) handleLogs(values map[string]any) (any, *dbxpluginsdk.PluginError) {
	target, pluginError := p.pickSession(values)
	if pluginError != nil {
		return nil, pluginError
	}
	endpointID := toInt(values["endpointId"])
	containerID := toStr(values["containerId"])
	if endpointID == 0 || containerID == "" {
		return nil, dbxpluginsdk.NewError(-32602, "Missing endpointId or containerId")
	}
	tail := toInt(values["tail"])
	if tail <= 0 {
		tail = defaultLogTail
	}
	if tail > maxLogTail {
		tail = maxLogTail
	}
	query := url.Values{}
	query.Set("stdout", "1")
	query.Set("stderr", "1")
	query.Set("tail", strconv.Itoa(tail))
	if toBool(values["timestamps"]) {
		query.Set("timestamps", "1")
	}
	if since := resolveSince(values); since != "" {
		query.Set("since", since)
	}
	path := fmt.Sprintf("/api/endpoints/%d/docker/containers/%s/logs", endpointID, url.PathEscape(containerID))
	body, pluginError := target.get(path, query)
	if pluginError != nil {
		return nil, pluginError
	}
	text := decodeDockerLogs(body)
	truncated := false
	if len(text) > maxLogBytes {
		text = text[len(text)-maxLogBytes:]
		truncated = true
	}
	return map[string]any{"text": text, "bytes": len(body), "truncated": truncated}, nil
}

func (p *plugin) handleDatabases(values map[string]any) (any, *dbxpluginsdk.PluginError) {
	target, pluginError := p.pickSession(values)
	if pluginError != nil {
		return nil, pluginError
	}
	endpointID := toInt(values["endpointId"])
	if endpointID == 0 {
		return nil, dbxpluginsdk.NewError(-32602, "Missing endpointId")
	}
	host := toStr(values["host"])
	if host == "" {
		host = p.lookupEndpointHost(target, endpointID)
	}
	query := url.Values{}
	query.Set("all", "1")
	body, pluginError := target.get(containerListPath(endpointID), query)
	if pluginError != nil {
		return nil, pluginError
	}
	containers, pluginError := decodeContainers(body)
	if pluginError != nil {
		return nil, pluginError
	}
	items := []map[string]any{}
	for _, container := range containers {
		rule, matched := matchEngine(container.Image, container.Names)
		if !matched {
			continue
		}
		publicPort := selectPort(container, rule.defaultPort)
		if publicPort == 0 {
			continue
		}
		items = append(items, map[string]any{
			"containerId":   container.ID,
			"containerName": container.displayName(),
			"image":         container.Image,
			"state":         container.State,
			"engine":        rule.engine,
			"dbxType":       rule.dbxType,
			"host":          host,
			"port":          publicPort,
			"privatePort":   rule.defaultPort,
			"endpointId":    endpointID,
		})
	}
	return map[string]any{"items": items, "host": host}, nil
}

func (p *plugin) lookupEndpointHost(target *session, endpointID int) string {
	body, pluginError := target.get("/api/endpoints", nil)
	if pluginError != nil {
		return ""
	}
	var raw []map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return ""
	}
	for _, endpoint := range raw {
		if toInt(endpoint["Id"]) == endpointID {
			return endpointHost(endpoint)
		}
	}
	return ""
}

// containerItems converts Docker containers into the payload consumed by the
// workbench, including stack and health metadata.
func containerItems(containers []dockerContainer) []map[string]any {
	items := make([]map[string]any, 0, len(containers))
	for _, container := range containers {
		entry := map[string]any{
			"id":      container.ID,
			"name":    container.displayName(),
			"image":   container.Image,
			"state":   container.State,
			"status":  container.Status,
			"health":  container.health(),
			"stack":   container.stackName(),
			"service": container.serviceName(),
			"ports":   container.portList(),
		}
		if rule, matched := matchEngine(container.Image, container.Names); matched {
			entry["engine"] = rule.engine
			entry["dbxType"] = rule.dbxType
		}
		items = append(items, entry)
	}
	return items
}

// noStackName labels containers that are not part of a Compose project.
const noStackName = "(no stack)"

// stackSummary aggregates the containers of one Compose project.
type stackSummary struct {
	name         string
	containers   int
	running      int
	services     map[string]struct{}
	healthCounts map[string]int
}

func (summary *stackSummary) health() string {
	switch {
	case summary.healthCounts["unhealthy"] > 0:
		return "unhealthy"
	case summary.healthCounts["starting"] > 0:
		return "starting"
	case summary.healthCounts["healthy"] > 0 && summary.healthCounts["healthy"] == summary.running:
		return "healthy"
	}
	return ""
}

// handleStacks groups containers by their Compose project label. This covers
// both Portainer-managed stacks and Compose projects deployed outside of
// Portainer, and it matches what the Portainer stack view lists.
func (p *plugin) handleStacks(values map[string]any) (any, *dbxpluginsdk.PluginError) {
	target, pluginError := p.pickSession(values)
	if pluginError != nil {
		return nil, pluginError
	}
	endpointID := toInt(values["endpointId"])
	if endpointID == 0 {
		return nil, dbxpluginsdk.NewError(-32602, "Missing endpointId")
	}
	query := url.Values{}
	query.Set("all", "1")
	body, pluginError := target.get(containerListPath(endpointID), query)
	if pluginError != nil {
		return nil, pluginError
	}
	containers, pluginError := decodeContainers(body)
	if pluginError != nil {
		return nil, pluginError
	}
	groups := map[string]*stackSummary{}
	for _, container := range containers {
		name := container.stackName()
		if name == "" {
			name = noStackName
		}
		group, ok := groups[name]
		if !ok {
			group = &stackSummary{
				name:         name,
				services:     map[string]struct{}{},
				healthCounts: map[string]int{},
			}
			groups[name] = group
		}
		group.containers++
		if container.State == "running" {
			group.running++
		}
		if service := container.serviceName(); service != "" {
			group.services[service] = struct{}{}
		}
		if health := container.health(); health != "" {
			group.healthCounts[health]++
		}
	}
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)
	items := make([]map[string]any, 0, len(names))
	for _, name := range names {
		group := groups[name]
		items = append(items, map[string]any{
			"name":       group.name,
			"containers": group.containers,
			"running":    group.running,
			"services":   len(group.services),
			"health":     group.health(),
		})
	}
	return map[string]any{"items": items}, nil
}

// handleStackContainers lists the containers of a single Compose project.
func (p *plugin) handleStackContainers(values map[string]any) (any, *dbxpluginsdk.PluginError) {
	target, pluginError := p.pickSession(values)
	if pluginError != nil {
		return nil, pluginError
	}
	endpointID := toInt(values["endpointId"])
	stack := toStr(values["stack"])
	if endpointID == 0 || stack == "" {
		return nil, dbxpluginsdk.NewError(-32602, "Missing endpointId or stack")
	}
	query := url.Values{}
	query.Set("all", "1")
	if stack != noStackName {
		filters, err := json.Marshal(map[string][]string{"label": {"com.docker.compose.project=" + stack}})
		if err != nil {
			return nil, dbxpluginsdk.NewError(-32603, "Failed to encode the container filter")
		}
		query.Set("filters", string(filters))
	}
	body, pluginError := target.get(containerListPath(endpointID), query)
	if pluginError != nil {
		return nil, pluginError
	}
	containers, pluginError := decodeContainers(body)
	if pluginError != nil {
		return nil, pluginError
	}
	sort.Slice(containers, func(left, right int) bool {
		return containers[left].displayName() < containers[right].displayName()
	})
	return map[string]any{"items": containerItems(containers), "stack": stack}, nil
}

// handleStats returns a single resource sample for one container, mirroring the
// values the Portainer stats view derives from the Docker stats endpoint.
func (p *plugin) handleStats(values map[string]any) (any, *dbxpluginsdk.PluginError) {
	target, pluginError := p.pickSession(values)
	if pluginError != nil {
		return nil, pluginError
	}
	endpointID := toInt(values["endpointId"])
	containerID := toStr(values["containerId"])
	if endpointID == 0 || containerID == "" {
		return nil, dbxpluginsdk.NewError(-32602, "Missing endpointId or containerId")
	}
	query := url.Values{}
	query.Set("stream", "false")
	path := fmt.Sprintf("/api/endpoints/%d/docker/containers/%s/stats", endpointID, url.PathEscape(containerID))
	body, pluginError := target.get(path, query)
	if pluginError != nil {
		return nil, pluginError
	}
	var stats map[string]any
	if err := json.Unmarshal(body, &stats); err != nil {
		return nil, dbxpluginsdk.NewError(-32000, "Unexpected response from the Portainer stats API")
	}
	cpuStats := nestedMap(stats["cpu_stats"])
	preCPUStats := nestedMap(stats["precpu_stats"])
	cpuUsage := nestedMap(cpuStats["cpu_usage"])
	preCPUUsage := nestedMap(preCPUStats["cpu_usage"])
	cpuDelta := numberFrom(cpuUsage["total_usage"]) - numberFrom(preCPUUsage["total_usage"])
	systemDelta := numberFrom(cpuStats["system_cpu_usage"]) - numberFrom(preCPUStats["system_cpu_usage"])
	onlineCPUs := numberFrom(cpuStats["online_cpus"])
	if onlineCPUs == 0 {
		if perCPU, ok := cpuUsage["percpu_usage"].([]any); ok {
			onlineCPUs = float64(len(perCPU))
		}
	}
	cpuPercent := 0.0
	if systemDelta > 0 && cpuDelta > 0 {
		cpuPercent = cpuDelta / systemDelta * onlineCPUs * 100
	}
	memoryStats := nestedMap(stats["memory_stats"])
	memoryUsage := numberFrom(memoryStats["usage"])
	memoryLimit := numberFrom(memoryStats["limit"])
	memoryPercent := 0.0
	if memoryLimit > 0 {
		memoryPercent = memoryUsage / memoryLimit * 100
	}
	networkRx := 0.0
	networkTx := 0.0
	for _, rawInterface := range nestedMap(stats["networks"]) {
		iface := nestedMap(rawInterface)
		networkRx += numberFrom(iface["rx_bytes"])
		networkTx += numberFrom(iface["tx_bytes"])
	}
	blockRead := 0.0
	blockWrite := 0.0
	if records, ok := nestedMap(stats["blkio_stats"])["io_service_bytes_recursive"].([]any); ok {
		for _, rawRecord := range records {
			record := nestedMap(rawRecord)
			switch strings.ToLower(toStr(record["op"])) {
			case "read":
				blockRead += numberFrom(record["value"])
			case "write":
				blockWrite += numberFrom(record["value"])
			}
		}
	}
	return map[string]any{
		"cpuPercent":    cpuPercent,
		"onlineCpus":    onlineCPUs,
		"memoryUsage":   memoryUsage,
		"memoryLimit":   memoryLimit,
		"memoryPercent": memoryPercent,
		"networkRx":     networkRx,
		"networkTx":     networkTx,
		"blockRead":     blockRead,
		"blockWrite":    blockWrite,
		"pids":          numberFrom(nestedMap(stats["pids_stats"])["current"]),
		"read":          toStr(stats["read"]),
	}, nil
}

// resolveSince maps the log range selector onto the Docker "since" parameter.
func resolveSince(values map[string]any) string {
	switch strings.ToLower(toStr(values["range"])) {
	case "1h":
		return strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10)
	case "24h":
		return strconv.FormatInt(time.Now().Add(-24*time.Hour).Unix(), 10)
	case "7d":
		return strconv.FormatInt(time.Now().Add(-7*24*time.Hour).Unix(), 10)
	}
	return toStr(values["since"])
}

func nestedMap(value any) map[string]any {
	nested, _ := value.(map[string]any)
	return nested
}

func numberFrom(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case int:
		return float64(typed)
	case string:
		if parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64); err == nil {
			return parsed
		}
	}
	return 0
}

// pickSession resolves the connection that a request refers to. When the
// sandboxed UI cannot read the connection id from its context and exactly one
// Portainer connection is active, that single session is used.
func (p *plugin) pickSession(values map[string]any) (*session, *dbxpluginsdk.PluginError) {
	connectionID := connectionIDFrom(values)
	p.mutex.RLock()
	defer p.mutex.RUnlock()
	if connectionID != "" {
		if target, ok := p.sessions[connectionID]; ok {
			return target, nil
		}
	}
	if len(p.sessions) == 1 {
		for _, target := range p.sessions {
			return target, nil
		}
	}
	if connectionID != "" {
		return nil, dbxpluginsdk.NewError(-32000, "Portainer connection is not established: "+connectionID)
	}
	if len(p.sessions) == 0 {
		return nil, dbxpluginsdk.NewError(-32000, "No active Portainer connection. Open this workbench from a Portainer connection.")
	}
	return nil, dbxpluginsdk.NewError(-32000, "Multiple Portainer connections are active; the request did not specify a connectionId")
}

func (s *session) get(path string, query url.Values) ([]byte, *dbxpluginsdk.PluginError) {
	endpoint := s.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	request, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, dbxpluginsdk.NewError(-32000, "Failed to build the Portainer request: "+err.Error())
	}
	if looksLikeJWT(s.token) {
		// Portainer access tokens travel in X-API-Key. A session token (JWT)
		// minted by /api/auth is only accepted as a bearer credential.
		request.Header.Set("Authorization", "Bearer "+s.token)
	} else {
		request.Header.Set("X-API-Key", s.token)
	}
	request.Header.Set("Accept", "application/json")
	response, err := s.client.Do(request)
	if err != nil {
		return nil, dbxpluginsdk.NewError(-32000, "Portainer request failed: "+err.Error())
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBody))
	if err != nil {
		return nil, dbxpluginsdk.NewError(-32000, "Failed to read the Portainer response: "+err.Error())
	}
	switch {
	case response.StatusCode == http.StatusUnauthorized, response.StatusCode == http.StatusForbidden:
		return nil, dbxpluginsdk.NewError(-32000, "Portainer rejected the API access token")
	case response.StatusCode < 200 || response.StatusCode >= 300:
		return nil, dbxpluginsdk.NewError(-32000, fmt.Sprintf("Portainer returned HTTP %d: %s", response.StatusCode, truncate(strings.TrimSpace(string(body)), 300)))
	}
	return body, nil
}

// buildSession reads the connection form values. Portainer stores its URL
// across the standard host/port bindings plus two config fields, and the DBX
// host may nest them differently, so every known location is inspected.
func buildSession(values map[string]any) (*session, *dbxpluginsdk.PluginError) {
	objects := collectObjects(values)
	host := lookupString(objects, "host", "hostname", "hostName", "endpoint", "server", "address", "baseUrl", "baseURL", "url")
	token := lookupString(objects, "apiToken", "api_token", "accessToken", "access_token", "token", "secret", "password")
	protocol := strings.ToLower(lookupString(objects, "protocol", "scheme"))
	port := lookupInt(objects, "port")

	if host == "" {
		return nil, dbxpluginsdk.NewError(-32602, "A Portainer host is required")
	}
	if token == "" {
		return nil, dbxpluginsdk.NewError(-32602, "A Portainer API access token is required")
	}

	schemeFromHost := ""
	if strings.Contains(host, "://") {
		if parsed, err := url.Parse(host); err == nil {
			schemeFromHost = parsed.Scheme
			if parsed.Port() != "" && port == 0 {
				if parsedPort, convErr := strconv.Atoi(parsed.Port()); convErr == nil {
					port = parsedPort
				}
			}
			if parsed.Hostname() != "" {
				host = parsed.Hostname()
			}
		}
	}

	if protocol == "" {
		protocol = schemeFromHost
	}
	protocol = strings.TrimSuffix(strings.TrimSpace(protocol), "://")
	if protocol != "http" && protocol != "https" {
		// Portainer serves plain HTTP on 8000/9000 and HTTPS on 9443.
		if port == 8000 || port == 9000 {
			protocol = "http"
		} else {
			protocol = "https"
		}
	}
	if port == 0 {
		if protocol == "http" {
			port = 8000
		} else {
			port = 9443
		}
	}

	verifyTLS := true
	if value, ok := lookupBool(objects, "tlsVerify", "tls_verify", "verifyTls", "verifyTLS", "verifyCertificate"); ok {
		verifyTLS = value
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		// Portainer is often deployed with a self-signed certificate on a
		// private network. Verification stays on unless the user opts out.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: !verifyTLS}, //nolint:gosec
	}
	return &session{
		baseURL: protocol + "://" + net.JoinHostPort(host, strconv.Itoa(port)),
		token:   token,
		client:  &http.Client{Timeout: requestTimeout, Transport: transport},
	}, nil
}

// collectObjects returns the request map plus its nested objects so that a
// field can be found regardless of how the DBX host wraps the connection.
func collectObjects(root map[string]any) []map[string]any {
	objects := make([]map[string]any, 0, 8)
	queue := []map[string]any{root}
	for len(queue) > 0 && len(objects) < 24 {
		current := queue[0]
		queue = queue[1:]
		objects = append(objects, current)
		for _, value := range current {
			if nested, ok := value.(map[string]any); ok {
				queue = append(queue, nested)
			}
		}
	}
	return objects
}

func lookupString(objects []map[string]any, keys ...string) string {
	for _, object := range objects {
		for _, key := range keys {
			for actualKey, value := range object {
				if strings.EqualFold(actualKey, key) {
					if text := toStr(value); text != "" {
						return text
					}
				}
			}
		}
	}
	return ""
}

func lookupInt(objects []map[string]any, keys ...string) int {
	for _, object := range objects {
		for _, key := range keys {
			for actualKey, value := range object {
				if strings.EqualFold(actualKey, key) {
					if number := toInt(value); number != 0 {
						return number
					}
				}
			}
		}
	}
	return 0
}

func lookupBool(objects []map[string]any, keys ...string) (bool, bool) {
	for _, object := range objects {
		for _, key := range keys {
			for actualKey, value := range object {
				if !strings.EqualFold(actualKey, key) {
					continue
				}
				switch typed := value.(type) {
				case bool:
					return typed, true
				case string:
					if parsed, err := strconv.ParseBool(strings.TrimSpace(typed)); err == nil {
						return parsed, true
					}
				}
			}
		}
	}
	return false, false
}

type dockerPort struct {
	IP          string `json:"IP"`
	PrivatePort int    `json:"PrivatePort"`
	PublicPort  int    `json:"PublicPort"`
	Type        string `json:"Type"`
}

type dockerContainer struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	State  string            `json:"State"`
	Status string            `json:"Status"`
	Labels map[string]string `json:"Labels"`
	Ports  []dockerPort      `json:"Ports"`
}

// health reports the health check state parsed from the Docker status line,
// for example "Up 3 hours (healthy)". Containers without a health check return
// an empty string.
func (container dockerContainer) health() string {
	status := strings.ToLower(container.Status)
	switch {
	case strings.Contains(status, "(unhealthy"):
		return "unhealthy"
	case strings.Contains(status, "health: starting"):
		return "starting"
	case strings.Contains(status, "(healthy"):
		return "healthy"
	}
	return ""
}

// stackName returns the Compose project the container belongs to.
func (container dockerContainer) stackName() string {
	return strings.TrimSpace(container.Labels["com.docker.compose.project"])
}

// serviceName returns the Compose service name inside the project.
func (container dockerContainer) serviceName() string {
	return strings.TrimSpace(container.Labels["com.docker.compose.service"])
}

func (container dockerContainer) displayName() string {
	if len(container.Names) == 0 {
		return container.ID
	}
	return strings.TrimPrefix(container.Names[0], "/")
}

func (container dockerContainer) portList() []map[string]any {
	items := make([]map[string]any, 0, len(container.Ports))
	for _, port := range container.Ports {
		items = append(items, map[string]any{
			"privatePort": port.PrivatePort,
			"publicPort":  port.PublicPort,
			"type":        port.Type,
			"ip":          port.IP,
		})
	}
	return items
}

func decodeContainers(body []byte) ([]dockerContainer, *dbxpluginsdk.PluginError) {
	var containers []dockerContainer
	if err := json.Unmarshal(body, &containers); err != nil {
		return nil, dbxpluginsdk.NewError(-32000, "Unexpected response from the Portainer containers API")
	}
	return containers, nil
}

type engineRule struct {
	keyword     string
	engine      string
	dbxType     string
	defaultPort int
}

var engineRules = []engineRule{
	{keyword: "mariadb", engine: "mariadb", dbxType: "mariadb", defaultPort: 3306},
	{keyword: "percona", engine: "mysql", dbxType: "mysql", defaultPort: 3306},
	{keyword: "mysql", engine: "mysql", dbxType: "mysql", defaultPort: 3306},
	{keyword: "tidb", engine: "tidb", dbxType: "mysql", defaultPort: 4000},
	{keyword: "pgvector", engine: "postgresql", dbxType: "postgres", defaultPort: 5432},
	{keyword: "timescale", engine: "postgresql", dbxType: "postgres", defaultPort: 5432},
	{keyword: "postgres", engine: "postgresql", dbxType: "postgres", defaultPort: 5432},
	{keyword: "valkey", engine: "redis", dbxType: "redis", defaultPort: 6379},
	{keyword: "redis", engine: "redis", dbxType: "redis", defaultPort: 6379},
	{keyword: "mongo", engine: "mongodb", dbxType: "mongodb", defaultPort: 27017},
	{keyword: "opensearch", engine: "opensearch", dbxType: "elasticsearch", defaultPort: 9200},
	{keyword: "elasticsearch", engine: "elasticsearch", dbxType: "elasticsearch", defaultPort: 9200},
	{keyword: "clickhouse", engine: "clickhouse", dbxType: "clickhouse", defaultPort: 8123},
	{keyword: "neo4j", engine: "neo4j", dbxType: "neo4j", defaultPort: 7687},
	{keyword: "minio", engine: "object-storage", dbxType: "s3", defaultPort: 9000},
	{keyword: "sqlserver", engine: "sqlserver", dbxType: "sqlserver", defaultPort: 1433},
	{keyword: "mssql", engine: "sqlserver", dbxType: "sqlserver", defaultPort: 1433},
	{keyword: "oracle", engine: "oracle", dbxType: "oracle", defaultPort: 1521},
	{keyword: "kingbase", engine: "kingbase", dbxType: "kingbase", defaultPort: 54321},
	{keyword: "dameng", engine: "dameng", dbxType: "dameng", defaultPort: 5236},
	{keyword: "dm8", engine: "dameng", dbxType: "dameng", defaultPort: 5236},
	{keyword: "nacos", engine: "nacos", dbxType: "nacos", defaultPort: 8848},
}

func matchEngine(image string, names []string) (engineRule, bool) {
	haystack := strings.ToLower(image)
	for _, name := range names {
		haystack += " " + strings.ToLower(name)
	}
	for _, rule := range engineRules {
		if strings.Contains(haystack, rule.keyword) {
			return rule, true
		}
	}
	return engineRule{}, false
}

func selectPort(container dockerContainer, preferred int) int {
	fallback := 0
	for _, port := range container.Ports {
		if port.Type != "" && port.Type != "tcp" {
			continue
		}
		if port.PublicPort == 0 {
			continue
		}
		if port.PrivatePort == preferred {
			return port.PublicPort
		}
		if fallback == 0 {
			fallback = port.PublicPort
		}
	}
	return fallback
}

// decodeDockerLogs removes the Docker stream multiplexing header that wraps
// non-TTY container output: one stream-type byte, three zero bytes and a
// big-endian payload length. TTY containers return plain text and are passed
// through unchanged.
func decodeDockerLogs(raw []byte) string {
	if len(raw) < 8 || raw[0] > 2 || raw[1] != 0 || raw[2] != 0 || raw[3] != 0 {
		return string(raw)
	}
	var builder strings.Builder
	for offset := 0; offset+8 <= len(raw); {
		length := int(binary.BigEndian.Uint32(raw[offset+4 : offset+8]))
		if offset+8+length > len(raw) {
			builder.Write(raw[offset+8:])
			break
		}
		builder.Write(raw[offset+8 : offset+8+length])
		offset += 8 + length
	}
	return builder.String()
}

func envKeys(detail map[string]any) []string {
	config, ok := detail["Config"].(map[string]any)
	if !ok {
		return []string{}
	}
	rawEnv, ok := config["Env"].([]any)
	if !ok {
		return []string{}
	}
	keys := make([]string, 0, len(rawEnv))
	for _, item := range rawEnv {
		entry := toStr(item)
		if index := strings.Index(entry, "="); index > 0 {
			keys = append(keys, entry[:index])
		}
	}
	return keys
}

func inspectPorts(detail map[string]any) []map[string]any {
	networkSettings, ok := detail["NetworkSettings"].(map[string]any)
	if !ok {
		return []map[string]any{}
	}
	rawPorts, ok := networkSettings["Ports"].(map[string]any)
	if !ok {
		return []map[string]any{}
	}
	items := []map[string]any{}
	for containerPort, rawBindings := range rawPorts {
		bindings, _ := rawBindings.([]any)
		if len(bindings) == 0 {
			items = append(items, map[string]any{"containerPort": containerPort})
			continue
		}
		for _, rawBinding := range bindings {
			binding, _ := rawBinding.(map[string]any)
			items = append(items, map[string]any{
				"containerPort": containerPort,
				"hostIp":        toStr(binding["HostIp"]),
				"hostPort":      toInt(binding["HostPort"]),
			})
		}
	}
	return items
}

func inspectMounts(detail map[string]any) []map[string]any {
	rawMounts, ok := detail["Mounts"].([]any)
	if !ok {
		return []map[string]any{}
	}
	items := make([]map[string]any, 0, len(rawMounts))
	for _, rawMount := range rawMounts {
		mount, ok := rawMount.(map[string]any)
		if !ok {
			continue
		}
		items = append(items, map[string]any{
			"type":        toStr(mount["Type"]),
			"source":      toStr(mount["Source"]),
			"destination": toStr(mount["Destination"]),
			"mode":        toStr(mount["Mode"]),
			"name":        toStr(mount["Name"]),
		})
	}
	return items
}

func inspectNetworks(detail map[string]any) []map[string]any {
	networkSettings, ok := detail["NetworkSettings"].(map[string]any)
	if !ok {
		return []map[string]any{}
	}
	rawNetworks, ok := networkSettings["Networks"].(map[string]any)
	if !ok {
		return []map[string]any{}
	}
	items := []map[string]any{}
	for name, rawNetwork := range rawNetworks {
		network, _ := rawNetwork.(map[string]any)
		items = append(items, map[string]any{
			"name":      name,
			"ipAddress": toStr(network["IPAddress"]),
			"networkId": toStr(network["NetworkID"]),
		})
	}
	return items
}

func endpointHost(endpoint map[string]any) string {
	if publicURL := strings.TrimSpace(toStr(endpoint["PublicURL"])); publicURL != "" {
		return stripScheme(publicURL)
	}
	raw := strings.TrimSpace(toStr(endpoint["URL"]))
	if raw == "" {
		return ""
	}
	if parsed, err := url.Parse(raw); err == nil && parsed.Hostname() != "" {
		return parsed.Hostname()
	}
	return stripScheme(raw)
}

func stripScheme(value string) string {
	trimmed := strings.TrimSpace(value)
	if index := strings.Index(trimmed, "://"); index >= 0 {
		trimmed = trimmed[index+3:]
	}
	if host, _, err := net.SplitHostPort(trimmed); err == nil {
		return host
	}
	return strings.TrimSuffix(trimmed, "/")
}

func connectionIDFrom(values map[string]any) string {
	if value := toStr(values["connectionId"]); value != "" {
		return value
	}
	if connection, ok := values["connection"].(map[string]any); ok {
		return toStr(connection["id"])
	}
	return ""
}

func containerListPath(endpointID int) string {
	return fmt.Sprintf("/api/endpoints/%d/docker/containers/json", endpointID)
}

func decodeParams(params json.RawMessage) map[string]any {
	values := map[string]any{}
	if len(params) == 0 {
		return values
	}
	if err := json.Unmarshal(params, &values); err != nil || values == nil {
		return map[string]any{}
	}
	return values
}

func failure(message string) map[string]any {
	return map[string]any{"success": false, "message": message}
}

func toStr(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(typed)
	}
	return ""
}

func toInt(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case string:
		if parsed, err := strconv.Atoi(strings.TrimSpace(typed)); err == nil {
			return parsed
		}
	}
	return 0
}

func toBool(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(typed))
		return err == nil && parsed
	case float64:
		return typed != 0
	}
	return false
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}

// looksLikeJWT reports whether a credential is a Portainer session token
// rather than a long-lived API access token.
func looksLikeJWT(token string) bool {
	return strings.HasPrefix(token, "eyJ") && strings.Count(token, ".") == 2
}
