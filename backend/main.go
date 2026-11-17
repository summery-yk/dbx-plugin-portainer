// Package main implements the DBX Portainer plugin sidecar.
//
// The sidecar speaks the DBX Sidecar Protocol v1 over stdio (JSON Lines) and
// exposes a read-only view of a Portainer instance: environments, containers,
// port mappings and container logs.
package main

import (
	"crypto/tls"
	"encoding/base64"
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
	pluginVersion   = "0.2.3"
	requestTimeout  = 25 * time.Second
	maxResponseBody = 8 << 20
	maxLogBytes     = 512 << 10
	defaultLogTail  = 200
	maxLogTail      = 5000

	// Portainer endpoint Type 5 is a Kubernetes environment. Docker and other
	// environment types keep using the existing /docker/... API gateway.
	endpointTypeKubernetes = 5
	k8sContainerIDPrefix   = "k8s:"
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
	case "portainer/namespaces":
		return p.handleNamespaces(values)
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
	containers, pluginError := listContainers(target, endpointID, values)
	if pluginError != nil {
		return nil, pluginError
	}
	containers = applyNamespaceFilter(target, endpointID, containers, values)
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
	if isKubernetesEndpoint(target, endpointID) {
		return p.handleKubernetesInspect(target, endpointID, containerID)
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
	if isKubernetesEndpoint(target, endpointID) {
		return p.handleKubernetesLogs(target, endpointID, containerID, tail, values)
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
	containers, pluginError := listContainers(target, endpointID, map[string]any{"all": true})
	if pluginError != nil {
		return nil, pluginError
	}
	containers = applyNamespaceFilter(target, endpointID, containers, values)
	kubernetes := isKubernetesEndpoint(target, endpointID)
	items := []map[string]any{}
	for _, container := range containers {
		rule, matched := matchEngine(container.Image, container.Names)
		if !matched {
			continue
		}
		publicPort := selectPort(container, rule.defaultPort)
		if publicPort == 0 && isKubernetesContainer(container) {
			publicPort = selectKubernetesPort(container, rule.defaultPort)
		}
		if publicPort == 0 {
			continue
		}
		itemHost := host
		if kubernetes && toStr(values["host"]) == "" {
			if serviceHost := kubernetesDatabaseHost(container); serviceHost != "" {
				itemHost = serviceHost
			}
		}
		items = append(items, map[string]any{
			"containerId":   container.ID,
			"containerName": container.displayName(),
			"image":         container.Image,
			"state":         container.State,
			"engine":        rule.engine,
			"dbxType":       rule.dbxType,
			"host":          itemHost,
			"port":          publicPort,
			"privatePort":   rule.defaultPort,
			"endpointId":    endpointID,
		})
	}
	if host == "" && len(items) > 0 {
		host = toStr(items[0]["host"])
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
	containers, pluginError := listContainers(target, endpointID, map[string]any{"all": true})
	if pluginError != nil {
		return nil, pluginError
	}
	containers = applyNamespaceFilter(target, endpointID, containers, values)
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

// handleNamespaces exposes the Kubernetes-native counterpart of handleStacks.
// Docker environments can still call it and receive the Compose grouping, but
// the UI only uses this method for Type=5 endpoints.
func (p *plugin) handleNamespaces(values map[string]any) (any, *dbxpluginsdk.PluginError) {
	target, pluginError := p.pickSession(values)
	if pluginError != nil {
		return nil, pluginError
	}
	endpointID := toInt(values["endpointId"])
	if endpointID == 0 {
		return nil, dbxpluginsdk.NewError(-32602, "Missing endpointId")
	}
	result, pluginError := p.handleStacks(values)
	if pluginError != nil {
		return nil, pluginError
	}
	payload, _ := result.(map[string]any)
	kind := "stacks"
	if isKubernetesEndpoint(target, endpointID) {
		kind = "namespaces"
	}
	return map[string]any{"kind": kind, "items": payload["items"]}, nil
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
	containers, pluginError := listContainers(target, endpointID, map[string]any{"all": true})
	if pluginError != nil {
		return nil, pluginError
	}
	filtered := make([]dockerContainer, 0, len(containers))
	for _, container := range containers {
		name := container.stackName()
		if name == "" {
			name = noStackName
		}
		if name == stack {
			filtered = append(filtered, container)
		}
	}
	containers = filtered
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
	if isKubernetesEndpoint(target, endpointID) {
		return p.handleKubernetesStats(target, endpointID, containerID)
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

// listContainers selects the Portainer API family for an environment. Docker
// environments keep using the original gateway; Kubernetes environments are
// translated into the same dockerContainer shape consumed by the UI and MCP.
func listContainers(target *session, endpointID int, values map[string]any) ([]dockerContainer, *dbxpluginsdk.PluginError) {
	if isKubernetesEndpoint(target, endpointID) {
		body, pluginError := target.get(kubernetesPodsPath(endpointID), nil)
		if pluginError != nil {
			return nil, pluginError
		}
		return decodeKubernetesPods(body)
	}
	query := url.Values{}
	if toBool(values["all"]) {
		query.Set("all", "1")
	}
	body, pluginError := target.get(containerListPath(endpointID), query)
	if pluginError != nil {
		return nil, pluginError
	}
	return decodeContainers(body)
}

// applyNamespaceFilter filters Kubernetes pod containers by namespace. Docker
// environments ignore this parameter so the existing API stays unchanged.
func applyNamespaceFilter(target *session, endpointID int, containers []dockerContainer, values map[string]any) []dockerContainer {
	namespace := strings.TrimSpace(toStr(values["namespace"]))
	if namespace == "" || !isKubernetesEndpoint(target, endpointID) {
		return containers
	}
	filtered := make([]dockerContainer, 0, len(containers))
	for _, container := range containers {
		if container.stackName() == namespace {
			filtered = append(filtered, container)
		}
	}
	return filtered
}

// endpointType reads the Portainer endpoint type. A failure falls back to the
// Docker path so older or restricted Portainer instances keep working.
func endpointType(target *session, endpointID int) int {
	body, pluginError := target.get("/api/endpoints", nil)
	if pluginError != nil {
		return 0
	}
	var endpoints []map[string]any
	if err := json.Unmarshal(body, &endpoints); err != nil {
		return 0
	}
	for _, endpoint := range endpoints {
		if toInt(endpoint["Id"]) == endpointID {
			return toInt(endpoint["Type"])
		}
	}
	return 0
}

func isKubernetesEndpoint(target *session, endpointID int) bool {
	return endpointType(target, endpointID) == endpointTypeKubernetes
}

/* ------------------------------------------------------- Kubernetes support */

type kubernetesPodList struct {
	Items []kubernetesPod `json:"items"`
}

type kubernetesPod struct {
	Metadata kubernetesObjectMeta `json:"metadata"`
	Spec     kubernetesPodSpec    `json:"spec"`
	Status   kubernetesPodStatus  `json:"status"`
}

type kubernetesObjectMeta struct {
	Name            string                     `json:"name"`
	Namespace       string                     `json:"namespace"`
	UID             string                     `json:"uid"`
	Labels          map[string]string          `json:"labels"`
	Annotations     map[string]string          `json:"annotations"`
	OwnerReferences []kubernetesOwnerReference `json:"ownerReferences"`
}

type kubernetesOwnerReference struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

type kubernetesPodSpec struct {
	NodeName       string                `json:"nodeName"`
	RestartPolicy  string                `json:"restartPolicy"`
	Containers     []kubernetesContainer `json:"containers"`
	InitContainers []kubernetesContainer `json:"initContainers"`
}

type kubernetesContainer struct {
	Name         string                    `json:"name"`
	Image        string                    `json:"image"`
	Env          []kubernetesEnvVar        `json:"env"`
	Ports        []kubernetesContainerPort `json:"ports"`
	Resources    kubernetesResources       `json:"resources"`
	VolumeMounts []kubernetesVolumeMount   `json:"volumeMounts"`
}

type kubernetesEnvVar struct {
	Name      string `json:"name"`
	Value     string `json:"value"`
	ValueFrom any    `json:"valueFrom"`
}

type kubernetesContainerPort struct {
	Name          string `json:"name"`
	ContainerPort int    `json:"containerPort"`
	HostPort      int    `json:"hostPort"`
	Protocol      string `json:"protocol"`
}

type kubernetesResources struct {
	Limits   map[string]string `json:"limits"`
	Requests map[string]string `json:"requests"`
}

type kubernetesVolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	SubPath   string `json:"subPath"`
	ReadOnly  bool   `json:"readOnly"`
}

type kubernetesPodStatus struct {
	Phase                 string                      `json:"phase"`
	PodIP                 string                      `json:"podIP"`
	HostIP                string                      `json:"hostIP"`
	StartTime             string                      `json:"startTime"`
	ContainerStatuses     []kubernetesContainerStatus `json:"containerStatuses"`
	InitContainerStatuses []kubernetesContainerStatus `json:"initContainerStatuses"`
}

type kubernetesContainerStatus struct {
	Name         string                   `json:"name"`
	Ready        bool                     `json:"ready"`
	RestartCount int                      `json:"restartCount"`
	Image        string                   `json:"image"`
	State        kubernetesContainerState `json:"state"`
}

type kubernetesContainerState struct {
	Running    *kubernetesStateRunning    `json:"running"`
	Waiting    *kubernetesStateWaiting    `json:"waiting"`
	Terminated *kubernetesStateTerminated `json:"terminated"`
}

type kubernetesStateRunning struct {
	StartedAt string `json:"startedAt"`
}

type kubernetesStateWaiting struct {
	Reason string `json:"reason"`
}

type kubernetesStateTerminated struct {
	Reason     string `json:"reason"`
	ExitCode   int    `json:"exitCode"`
	FinishedAt string `json:"finishedAt"`
}

type kubernetesPodMetrics struct {
	Timestamp  string                      `json:"timestamp"`
	Containers []kubernetesMetricContainer `json:"containers"`
}

type kubernetesMetricContainer struct {
	Name  string            `json:"name"`
	Usage map[string]string `json:"usage"`
}

func kubernetesPodsPath(endpointID int) string {
	return fmt.Sprintf("/api/endpoints/%d/kubernetes/api/v1/pods", endpointID)
}

func kubernetesPodPath(endpointID int, namespace, pod string) string {
	return fmt.Sprintf("/api/endpoints/%d/kubernetes/api/v1/namespaces/%s/pods/%s",
		endpointID, url.PathEscape(namespace), url.PathEscape(pod))
}

func kubernetesPodLogPath(endpointID int, namespace, pod string) string {
	return kubernetesPodPath(endpointID, namespace, pod) + "/log"
}

func kubernetesPodMetricsPath(endpointID int, namespace, pod string) string {
	return fmt.Sprintf("/api/endpoints/%d/kubernetes/apis/metrics.k8s.io/v1beta1/namespaces/%s/pods/%s",
		endpointID, url.PathEscape(namespace), url.PathEscape(pod))
}

func decodeKubernetesPods(body []byte) ([]dockerContainer, *dbxpluginsdk.PluginError) {
	var list kubernetesPodList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, dbxpluginsdk.NewError(-32000, "Unexpected response from the Portainer Kubernetes pods API")
	}
	containers := make([]dockerContainer, 0)
	for _, pod := range list.Items {
		statuses := kubernetesStatusMap(pod.Status)
		containers = append(containers, kubernetesSpecContainers(pod, pod.Spec.InitContainers, statuses)...)
		containers = append(containers, kubernetesSpecContainers(pod, pod.Spec.Containers, statuses)...)
	}
	return containers, nil
}

func kubernetesSpecContainers(pod kubernetesPod, specs []kubernetesContainer, statuses map[string]kubernetesContainerStatus) []dockerContainer {
	namespace := strings.TrimSpace(pod.Metadata.Namespace)
	if namespace == "" {
		namespace = "default"
	}
	service := kubernetesServiceName(pod)
	containers := make([]dockerContainer, 0, len(specs))
	for _, spec := range specs {
		labels := make(map[string]string, len(pod.Metadata.Labels)+6)
		for key, value := range pod.Metadata.Labels {
			labels[key] = value
		}
		labels["com.docker.compose.project"] = namespace
		labels["com.docker.compose.service"] = service
		labels["io.dbx.portainer.k8s.pod"] = pod.Metadata.Name
		labels["io.dbx.portainer.k8s.container"] = spec.Name
		labels["io.dbx.portainer.k8s.node"] = pod.Spec.NodeName

		state, status, health := kubernetesContainerStateValues(spec.Name, pod.Status.Phase, statuses)
		if health != "" {
			status += " (" + health + ")"
		}
		ports := make([]dockerPort, 0, len(spec.Ports))
		for _, port := range spec.Ports {
			protocol := strings.ToLower(strings.TrimSpace(port.Protocol))
			if protocol == "" {
				protocol = "tcp"
			}
			ports = append(ports, dockerPort{
				PrivatePort: port.ContainerPort,
				PublicPort:  port.HostPort,
				Type:        protocol,
			})
		}
		containers = append(containers, dockerContainer{
			ID:     encodeK8sContainerID(namespace, pod.Metadata.Name, spec.Name),
			Names:  []string{"/" + pod.Metadata.Name + "/" + spec.Name},
			Image:  spec.Image,
			State:  state,
			Status: status,
			Labels: labels,
			Ports:  ports,
		})
	}
	return containers
}

func kubernetesStatusMap(status kubernetesPodStatus) map[string]kubernetesContainerStatus {
	result := make(map[string]kubernetesContainerStatus)
	for _, item := range status.ContainerStatuses {
		result[item.Name] = item
	}
	for _, item := range status.InitContainerStatuses {
		result[item.Name] = item
	}
	return result
}

func kubernetesContainerStateValues(name, phase string, statuses map[string]kubernetesContainerStatus) (string, string, string) {
	lowerPhase := strings.ToLower(strings.TrimSpace(phase))
	item, ok := statuses[name]
	if !ok {
		if lowerPhase == "" {
			return "unknown", "Unknown", ""
		}
		return lowerPhase, capitalize(lowerPhase), ""
	}
	if item.State.Running != nil {
		if item.Ready {
			return "running", "Running", "healthy"
		}
		return "running", "Running", "starting"
	}
	if item.State.Waiting != nil {
		reason := strings.TrimSpace(item.State.Waiting.Reason)
		if reason == "" {
			reason = "Waiting"
		}
		lowerReason := strings.ToLower(reason)
		health := "starting"
		if strings.Contains(lowerReason, "crashloop") || strings.Contains(lowerReason, "backoff") ||
			strings.Contains(lowerReason, "error") || strings.Contains(lowerReason, "evicted") ||
			strings.Contains(lowerReason, "imagepull") || strings.Contains(lowerReason, "oomkilled") {
			health = "unhealthy"
		}
		return "waiting", reason, health
	}
	if item.State.Terminated != nil {
		reason := strings.TrimSpace(item.State.Terminated.Reason)
		if reason == "" {
			reason = "Terminated"
		}
		health := ""
		if item.State.Terminated.ExitCode != 0 {
			health = "unhealthy"
		}
		return "terminated", reason, health
	}
	if lowerPhase != "" {
		return lowerPhase, capitalize(lowerPhase), ""
	}
	return "unknown", "Unknown", ""
}

func capitalize(value string) string {
	if value == "" {
		return value
	}
	runes := []rune(value)
	runes[0] = []rune(strings.ToUpper(string(runes[0])))[0]
	return string(runes)
}

func kubernetesServiceName(pod kubernetesPod) string {
	for _, key := range []string{
		"app.kubernetes.io/name",
		"app",
		"k8s-app",
		"app.kubernetes.io/instance",
		"statefulset.kubernetes.io/pod-name",
	} {
		if value := strings.TrimSpace(pod.Metadata.Labels[key]); value != "" {
			return value
		}
	}
	for _, owner := range pod.Metadata.OwnerReferences {
		switch owner.Kind {
		case "ReplicaSet", "Deployment", "StatefulSet", "DaemonSet":
			name := strings.TrimSpace(owner.Name)
			if name == "" {
				continue
			}
			if owner.Kind == "ReplicaSet" {
				if index := strings.LastIndex(name, "-"); index > 0 {
					name = name[:index]
				}
			}
			return name
		}
	}
	return pod.Metadata.Name
}

func encodeK8sContainerID(namespace, pod, container string) string {
	raw := namespace + "\x1f" + pod + "\x1f" + container
	return k8sContainerIDPrefix + base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeK8sContainerID(value string) (string, string, string, bool) {
	trimmed := strings.TrimSpace(value)
	if strings.HasPrefix(trimmed, k8sContainerIDPrefix) {
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(trimmed, k8sContainerIDPrefix))
		if err == nil {
			parts := strings.Split(string(raw), "\x1f")
			if len(parts) == 3 && parts[0] != "" && parts[1] != "" && parts[2] != "" {
				return parts[0], parts[1], parts[2], true
			}
		}
	}
	parts := strings.Split(trimmed, "/")
	if len(parts) == 3 && parts[0] != "" && parts[1] != "" && parts[2] != "" {
		return parts[0], parts[1], parts[2], true
	}
	return "", "", "", false
}

func isKubernetesContainer(container dockerContainer) bool {
	return strings.HasPrefix(container.ID, k8sContainerIDPrefix) ||
		container.Labels["io.dbx.portainer.k8s.container"] != ""
}

func selectKubernetesPort(container dockerContainer, preferred int) int {
	fallback := 0
	for _, port := range container.Ports {
		if port.PrivatePort == 0 {
			continue
		}
		if port.PrivatePort == preferred {
			return port.PrivatePort
		}
		if fallback == 0 {
			fallback = port.PrivatePort
		}
	}
	return fallback
}

func kubernetesDatabaseHost(container dockerContainer) string {
	namespace := strings.TrimSpace(container.Labels["com.docker.compose.project"])
	service := strings.TrimSpace(container.Labels["com.docker.compose.service"])
	if service == "" {
		service = strings.TrimSpace(container.Labels["io.dbx.portainer.k8s.pod"])
	}
	if namespace == "" || service == "" {
		return ""
	}
	return service + "." + namespace + ".svc.cluster.local"
}

func findKubernetesContainerSpec(pod kubernetesPod, name string) (kubernetesContainer, bool) {
	for _, spec := range pod.Spec.InitContainers {
		if spec.Name == name {
			return spec, true
		}
	}
	for _, spec := range pod.Spec.Containers {
		if spec.Name == name {
			return spec, true
		}
	}
	return kubernetesContainer{}, false
}

func (p *plugin) handleKubernetesInspect(target *session, endpointID int, containerID string) (any, *dbxpluginsdk.PluginError) {
	namespace, podName, containerName, ok := decodeK8sContainerID(containerID)
	if !ok {
		return nil, dbxpluginsdk.NewError(-32602, "Container id is not a Kubernetes pod container")
	}
	body, pluginError := target.get(kubernetesPodPath(endpointID, namespace, podName), nil)
	if pluginError != nil {
		return nil, pluginError
	}
	var pod kubernetesPod
	if err := json.Unmarshal(body, &pod); err != nil {
		return nil, dbxpluginsdk.NewError(-32000, "Unexpected response from the Portainer Kubernetes pod API")
	}
	spec, found := findKubernetesContainerSpec(pod, containerName)
	if !found {
		return nil, dbxpluginsdk.NewError(-32000, "Container no longer exists in pod "+podName)
	}
	statuses := kubernetesStatusMap(pod.Status)
	state, status, health := kubernetesContainerStateValues(containerName, pod.Status.Phase, statuses)
	result := map[string]any{
		"id":            containerID,
		"name":          podName + "/" + containerName,
		"namespace":     namespace,
		"pod":           podName,
		"container":     containerName,
		"image":         spec.Image,
		"state":         state,
		"status":        status,
		"health":        health,
		"env":           kubernetesEnvKeys(spec.Env),
		"ports":         kubernetesInspectPorts(spec.Ports),
		"mounts":        kubernetesInspectMounts(spec.VolumeMounts),
		"networks":      kubernetesInspectNetworks(pod),
		"restartPolicy": pod.Spec.RestartPolicy,
		"node":          pod.Spec.NodeName,
	}
	if item, ok := statuses[containerName]; ok {
		result["restartCount"] = item.RestartCount
		if item.State.Running != nil {
			result["running"] = true
			result["startedAt"] = item.State.Running.StartedAt
		} else {
			result["running"] = false
		}
		if item.State.Terminated != nil {
			result["finishedAt"] = item.State.Terminated.FinishedAt
		}
	}
	return result, nil
}

func kubernetesEnvKeys(values []kubernetesEnvVar) []string {
	keys := make([]string, 0, len(values))
	for _, item := range values {
		if name := strings.TrimSpace(item.Name); name != "" {
			keys = append(keys, name)
		}
	}
	return keys
}

func kubernetesInspectPorts(values []kubernetesContainerPort) []map[string]any {
	items := make([]map[string]any, 0, len(values))
	for _, port := range values {
		protocol := strings.ToLower(strings.TrimSpace(port.Protocol))
		if protocol == "" {
			protocol = "tcp"
		}
		items = append(items, map[string]any{
			"containerPort": strconv.Itoa(port.ContainerPort) + "/" + protocol,
			"hostIp":        "",
			"hostPort":      port.HostPort,
			"name":          port.Name,
		})
	}
	return items
}

func kubernetesInspectMounts(values []kubernetesVolumeMount) []map[string]any {
	items := make([]map[string]any, 0, len(values))
	for _, mount := range values {
		items = append(items, map[string]any{
			"type":        "volume",
			"source":      mount.Name,
			"destination": mount.MountPath,
			"mode":        mountMode(mount.ReadOnly),
			"name":        mount.Name,
			"subPath":     mount.SubPath,
		})
	}
	return items
}

func mountMode(readOnly bool) string {
	if readOnly {
		return "ro"
	}
	return "rw"
}

func kubernetesInspectNetworks(pod kubernetesPod) []map[string]any {
	items := make([]map[string]any, 0, 2)
	if pod.Status.PodIP != "" {
		items = append(items, map[string]any{"name": "pod", "ipAddress": pod.Status.PodIP, "networkId": ""})
	}
	if pod.Status.HostIP != "" {
		items = append(items, map[string]any{"name": "node", "ipAddress": pod.Status.HostIP, "networkId": ""})
	}
	return items
}

func (p *plugin) handleKubernetesLogs(target *session, endpointID int, containerID string, tail int, values map[string]any) (any, *dbxpluginsdk.PluginError) {
	namespace, podName, containerName, ok := decodeK8sContainerID(containerID)
	if !ok {
		return nil, dbxpluginsdk.NewError(-32602, "Container id is not a Kubernetes pod container")
	}
	query := url.Values{}
	query.Set("container", containerName)
	query.Set("tailLines", strconv.Itoa(tail))
	if toBool(values["previous"]) {
		query.Set("previous", "true")
	}
	if toBool(values["timestamps"]) {
		query.Set("timestamps", "true")
	}
	if since := toStr(values["since"]); since != "" {
		if seconds, err := strconv.ParseInt(since, 10, 64); err == nil && seconds > 0 {
			query.Set("sinceSeconds", strconv.FormatInt(seconds, 10))
		} else {
			query.Set("sinceTime", since)
		}
	} else {
		switch strings.ToLower(toStr(values["range"])) {
		case "1h":
			query.Set("sinceSeconds", "3600")
		case "24h":
			query.Set("sinceSeconds", "86400")
		case "7d":
			query.Set("sinceSeconds", "604800")
		}
	}
	body, pluginError := target.get(kubernetesPodLogPath(endpointID, namespace, podName), query)
	if pluginError != nil {
		if toBool(values["previous"]) && strings.Contains(pluginError.Message, "HTTP 400") {
			return map[string]any{
				"text":      "Previous container logs are not available for this container.",
				"bytes":     0,
				"truncated": false,
				"notice":    true,
			}, nil
		}
		return nil, pluginError
	}
	text := string(body)
	truncated := false
	if len(text) > maxLogBytes {
		text = text[len(text)-maxLogBytes:]
		truncated = true
	}
	return map[string]any{"text": text, "bytes": len(body), "truncated": truncated}, nil
}

func (p *plugin) handleKubernetesStats(target *session, endpointID int, containerID string) (any, *dbxpluginsdk.PluginError) {
	namespace, podName, containerName, ok := decodeK8sContainerID(containerID)
	if !ok {
		return nil, dbxpluginsdk.NewError(-32602, "Container id is not a Kubernetes pod container")
	}
	podBody, pluginError := target.get(kubernetesPodPath(endpointID, namespace, podName), nil)
	if pluginError != nil {
		return nil, pluginError
	}
	var pod kubernetesPod
	if err := json.Unmarshal(podBody, &pod); err != nil {
		return nil, dbxpluginsdk.NewError(-32000, "Unexpected response from the Portainer Kubernetes pod API")
	}
	spec, found := findKubernetesContainerSpec(pod, containerName)
	if !found {
		return nil, dbxpluginsdk.NewError(-32000, "Container no longer exists in pod "+podName)
	}
	metricsBody, pluginError := target.get(kubernetesPodMetricsPath(endpointID, namespace, podName), nil)
	if pluginError != nil {
		return nil, dbxpluginsdk.NewError(-32000, "Kubernetes metrics API is unavailable on this Portainer endpoint; install metrics-server or use a Docker environment for stats: "+pluginError.Message)
	}
	var metrics kubernetesPodMetrics
	if err := json.Unmarshal(metricsBody, &metrics); err != nil {
		return nil, dbxpluginsdk.NewError(-32000, "Unexpected response from the Portainer Kubernetes metrics API")
	}
	usage := kubernetesMetricContainer{}
	for _, item := range metrics.Containers {
		if item.Name == containerName {
			usage = item
			break
		}
	}
	cpuUsage := parseK8sQuantity(usage.Usage["cpu"])
	memoryUsage := parseK8sQuantity(usage.Usage["memory"])
	cpuLimit := parseK8sQuantity(spec.Resources.Limits["cpu"])
	memoryLimit := parseK8sQuantity(spec.Resources.Limits["memory"])
	cpuPercent := 0.0
	if cpuLimit > 0 {
		cpuPercent = cpuUsage / cpuLimit * 100
	} else {
		// Without a limit, show percent against a single core so the value
		// remains meaningful instead of always zero.
		cpuPercent = cpuUsage * 100
	}
	memoryPercent := 0.0
	if memoryLimit > 0 {
		memoryPercent = memoryUsage / memoryLimit * 100
	}
	return map[string]any{
		"cpuPercent":    cpuPercent,
		"onlineCpus":    0,
		"memoryUsage":   memoryUsage,
		"memoryLimit":   memoryLimit,
		"memoryPercent": memoryPercent,
		"networkRx":     0,
		"networkTx":     0,
		"blockRead":     0,
		"blockWrite":    0,
		"pids":          0,
		"read":          metrics.Timestamp,
	}, nil
}

// parseK8sQuantity converts the subset of Kubernetes resource quantities used
// by CPU and memory metrics into base units (cores and bytes).
func parseK8sQuantity(value string) float64 {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0
	}
	suffixes := []struct {
		text   string
		factor float64
	}{
		{"Ki", 1024},
		{"Mi", 1024 * 1024},
		{"Gi", 1024 * 1024 * 1024},
		{"Ti", 1024 * 1024 * 1024 * 1024},
		{"m", 0.001},
		{"u", 0.000001},
		{"n", 0.000000001},
		{"k", 1000},
		{"M", 1000 * 1000},
		{"G", 1000 * 1000 * 1000},
		{"T", 1000 * 1000 * 1000 * 1000},
	}
	for _, suffix := range suffixes {
		if strings.HasSuffix(trimmed, suffix.text) {
			number, err := strconv.ParseFloat(strings.TrimSuffix(trimmed, suffix.text), 64)
			if err == nil {
				return number * suffix.factor
			}
		}
	}
	number, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return 0
	}
	return number
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
