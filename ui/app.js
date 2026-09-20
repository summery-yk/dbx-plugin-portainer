"use strict";

const MAX_RENDERED_LINES = 3000;
const AUTO_REFRESH_MS = 3000;

const state = {
  connectionId: "",
  endpoints: [],
  endpointId: 0,
  view: "containers",
  containers: [],
  stacks: [],
  currentStack: "",
  selectedId: "",
  databases: new Map(),
  detail: null,
  logs: [],
  selectedLines: new Set(),
  logTimer: null,
  onlyDb: false,
  filter: "",
  tab: "overview",
};

const strings = {
  zh: {
    endpoint: "环境", reload: "刷新", viewContainers: "容器", viewStacks: "堆栈",
    onlyDb: "只看数据库", filterPlaceholder: "过滤容器名或镜像",
    containers: "容器", stacks: "堆栈",
    tabOverview: "概览", tabLogs: "日志", tabInspect: "检查", tabStats: "统计", tabConnect: "连接参数",
    autoRefresh: "自动刷新日志", wrapLines: "自动换行", showTimestamps: "显示时间戳",
    range: "获取范围", rangeAll: "全部日志", range1h: "最近 1 小时", range24h: "最近 24 小时", range7d: "最近 7 天",
    lines: "行数", search: "搜索", searchPlaceholder: "过滤…",
    download: "下载日志", copy: "复制", copySelected: "复制所选行", clearSelection: "取消选择",
    pickContainer: "从左侧选择一个容器。", empty: "没有匹配项。", loading: "加载中…", ready: "就绪",
    noConnection: "没有活动的 Portainer 连接。请从 DBX 的 Portainer 连接打开此工作台。",
    notDb: "未识别为数据库容器，可在概览查看端口映射。",
    dbHint: "已识别为数据库容器，可在 DBX 中按下面的参数新建连接。",
    copied: "已复制", copyFailed: "复制失败，请手动选择文本",
    noLogs: "暂无日志输出", statsHint: "选择容器后可采样资源占用。",
    logTruncated: "日志已截断为 512 KiB", logLoaded: "日志已加载",
    downloadFailed: "下载被沙箱阻止，请改用复制",
    truncatedRender: "仅渲染前 3000 行",
    restartPolicy: "重启策略", started: "启动时间", finished: "结束时间",
    running: "运行中", env: "环境变量名", ports: "端口", mounts: "挂载", networks: "网络",
    inspectHint: "选择容器后查看详细检查信息。",
    statsCpu: "CPU", statsMemory: "内存", statsNet: "网络 I/O", statsBlock: "块设备 I/O", statsPids: "进程数",
    statsReceived: "接收", statsSent: "发送", statsRead: "读", statsWrite: "写", statsSampledAt: "采样时间",
  },
  en: {
    endpoint: "Environment", reload: "Reload", viewContainers: "Containers", viewStacks: "Stacks",
    onlyDb: "Databases only", filterPlaceholder: "Filter by name or image",
    containers: "Containers", stacks: "Stacks",
    tabOverview: "Overview", tabLogs: "Logs", tabInspect: "Inspect", tabStats: "Stats", tabConnect: "Connection",
    autoRefresh: "Auto refresh logs", wrapLines: "Wrap lines", showTimestamps: "Show timestamps",
    range: "Range", rangeAll: "All logs", range1h: "Last hour", range24h: "Last 24 hours", range7d: "Last 7 days",
    lines: "Lines", search: "Search", searchPlaceholder: "Filter...",
    download: "Download", copy: "Copy", copySelected: "Copy selected", clearSelection: "Clear selection",
    pickContainer: "Select a container on the left.", empty: "Nothing matches.", loading: "Loading...", ready: "Ready",
    noConnection: "No active Portainer connection. Open this workbench from a Portainer connection in DBX.",
    notDb: "Not recognised as a database container. See the overview tab for port mappings.",
    dbHint: "Recognised as a database container. Create a DBX connection with these values.",
    copied: "Copied", copyFailed: "Copy failed, select the text manually",
    noLogs: "No log output", statsHint: "Select a container to sample its resource usage.",
    logTruncated: "Log truncated to 512 KiB", logLoaded: "Log loaded",
    downloadFailed: "Download blocked by the sandbox, use Copy instead",
    truncatedRender: "Only the first 3000 lines are rendered",
    restartPolicy: "Restart policy", started: "Started", finished: "Finished",
    running: "Running", env: "Env keys", ports: "Ports", mounts: "Mounts", networks: "Networks",
    inspectHint: "Select a container to inspect it.",
    statsCpu: "CPU", statsMemory: "Memory", statsNet: "Network I/O", statsBlock: "Block I/O", statsPids: "PIDs",
    statsReceived: "Received", statsSent: "Sent", statsRead: "Read", statsWrite: "Write", statsSampledAt: "Sampled at",
  },
};

let text = strings.en;

function $(id) {
  return document.getElementById(id);
}

async function invoke(method, params) {
  const payload = Object.assign({ connectionId: state.connectionId }, params || {});
  return window.dbxPlugin.invoke(method, payload, { timeoutMs: 60000 });
}

function setStatus(message, kind) {
  const node = $("status");
  node.textContent = message || "";
  if (kind) {
    node.dataset.kind = kind;
  } else {
    delete node.dataset.kind;
  }
}

function errorMessage(error) {
  if (!error) {
    return "Unknown error";
  }
  return error.message ? error.message : String(error);
}

function applyStrings() {
  document.querySelectorAll("[data-i18n]").forEach((node) => {
    const key = node.getAttribute("data-i18n");
    if (text[key]) {
      node.textContent = text[key];
    }
  });
  document.querySelectorAll("[data-i18n-placeholder]").forEach((node) => {
    const key = node.getAttribute("data-i18n-placeholder");
    if (text[key]) {
      node.setAttribute("placeholder", text[key]);
    }
  });
}

function formatBytes(value) {
  const size = Number(value) || 0;
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let index = 0;
  let current = size;
  while (current >= 1024 && index < units.length - 1) {
    current /= 1024;
    index += 1;
  }
  return current.toFixed(index === 0 ? 0 : 2) + " " + units[index];
}

function badgeClass(health, containerState) {
  if (health) {
    return health;
  }
  return containerState || "none";
}

function badgeText(container) {
  return container.health || container.state || "unknown";
}

function createBadge(container) {
  const badge = document.createElement("span");
  badge.className = "badge " + badgeClass(container.health, container.state);
  badge.textContent = badgeText(container);
  return badge;
}

function appendRow(parent, label, value) {
  if (value === undefined || value === null || value === "") {
    return;
  }
  const row = document.createElement("div");
  row.className = "row";
  const key = document.createElement("div");
  key.className = "row-key";
  key.textContent = label;
  const val = document.createElement("div");
  val.className = "row-value";
  val.textContent = value;
  row.appendChild(key);
  row.appendChild(val);
  parent.appendChild(row);
}

/* ---------------------------------------------------------------- navigation */

function setView(view) {
  state.view = view;
  state.currentStack = "";
  $("view-containers").classList.toggle("active", view === "containers");
  $("view-stacks").classList.toggle("active", view === "stacks");
  $("back-to-stacks").classList.add("hidden");
  if (view === "stacks") {
    loadStacks();
  } else {
    renderSidebar();
  }
}

function renderSidebar() {
  const list = $("items");
  list.textContent = "";
  const title = $("sidebar-title");
  const count = $("sidebar-count");

  if (state.view === "stacks" && !state.currentStack) {
    title.textContent = text.stacks;
    count.textContent = String(state.stacks.length);
    renderStackList(list);
    return;
  }

  title.textContent = state.currentStack ? state.currentStack : text.containers;
  const items = visibleContainers();
  count.textContent = String(items.length);
  renderContainerList(list, items);
}

function renderStackList(list) {
  if (!state.stacks.length) {
    const empty = document.createElement("li");
    empty.className = "hint";
    empty.textContent = text.empty;
    list.appendChild(empty);
    return;
  }
  state.stacks.forEach((stack) => {
    const card = document.createElement("li");
    card.className = "card";
    card.addEventListener("click", () => enterStack(stack.name));

    const head = document.createElement("div");
    head.className = "stack-row";
    const name = document.createElement("span");
    name.className = "stack-name";
    name.textContent = stack.name;
    head.appendChild(name);
    if (stack.health) {
      const badge = document.createElement("span");
      badge.className = "badge " + stack.health;
      badge.textContent = stack.health;
      head.appendChild(badge);
    }
    const counter = document.createElement("span");
    counter.className = "stack-count";
    counter.textContent = stack.running + "/" + stack.containers;
    head.appendChild(counter);
    card.appendChild(head);

    const meta = document.createElement("div");
    meta.className = "card-meta";
    meta.textContent = stack.services ? stack.services + " services" : stack.containers + " containers";
    card.appendChild(meta);

    list.appendChild(card);
  });
}

function renderContainerList(list, items) {
  if (!items.length) {
    const empty = document.createElement("li");
    empty.className = "hint";
    empty.textContent = text.empty;
    list.appendChild(empty);
    return;
  }
  items.forEach((item) => {
    const card = document.createElement("li");
    card.className = "card";
    if (item.id === state.selectedId) {
      card.classList.add("selected");
    }
    if (state.databases.has(item.id)) {
      card.classList.add("db");
    }
    card.addEventListener("click", () => selectContainer(item.id));

    const head = document.createElement("div");
    head.className = "card-head";
    const title = document.createElement("span");
    title.className = "card-title";
    title.textContent = item.name;
    head.appendChild(title);
    head.appendChild(createBadge(item));
    card.appendChild(head);

    const meta = document.createElement("div");
    meta.className = "card-meta";
    meta.textContent = [item.service, state.databases.get(item.id) ? state.databases.get(item.id).engine : item.engine, item.image]
      .filter(Boolean)
      .join(" | ");
    card.appendChild(meta);

    const ports = (item.ports || []).filter((port) => port.publicPort);
    if (ports.length) {
      const portLine = document.createElement("div");
      portLine.className = "card-ports";
      portLine.textContent = ports.map((port) => port.privatePort + " -> " + port.publicPort).join("   ");
      card.appendChild(portLine);
    }
    list.appendChild(card);
  });
}

function visibleContainers() {
  const needle = state.filter.trim().toLowerCase();
  return state.containers.filter((item) => {
    if (state.onlyDb && !state.databases.has(item.id)) {
      return false;
    }
    if (!needle) {
      return true;
    }
    return (item.name + " " + item.image + " " + (item.service || "")).toLowerCase().indexOf(needle) >= 0;
  });
}

/* -------------------------------------------------------------------- loading */

async function loadContainers() {
  setStatus(text.loading);
  try {
    const result = await invoke("portainer/containers", { endpointId: state.endpointId, all: true });
    state.containers = result.items || [];
    state.databases = new Map();
    state.selectedId = "";
    if (state.containers.some((item) => item.dbxType)) {
      try {
        const discovered = await invoke("portainer/databases", { endpointId: state.endpointId });
        (discovered.items || []).forEach((item) => state.databases.set(item.containerId, item));
      } catch (error) {
        // Database discovery is best effort.
      }
    }
    if (!state.currentStack) {
      renderSidebar();
    }
    setStatus(state.containers.length + " " + text.containers);
  } catch (error) {
    setStatus(errorMessage(error), "error");
  }
}

async function loadStacks() {
  setStatus(text.loading);
  try {
    const result = await invoke("portainer/stacks", { endpointId: state.endpointId });
    state.stacks = result.items || [];
    renderSidebar();
    setStatus(state.stacks.length + " " + text.stacks);
  } catch (error) {
    setStatus(errorMessage(error), "error");
  }
}

async function enterStack(name) {
  state.currentStack = name;
  setStatus(text.loading);
  try {
    const result = await invoke("portainer/stackContainers", { endpointId: state.endpointId, stack: name });
    state.containers = result.items || [];
    state.selectedId = "";
    $("back-to-stacks").classList.remove("hidden");
    renderSidebar();
    setStatus(name + ": " + state.containers.length + " " + text.containers);
  } catch (error) {
    setStatus(errorMessage(error), "error");
  }
}

function backToStacks() {
  state.currentStack = "";
  state.containers = [];
  state.selectedId = "";
  state.detail = null;
  stopAutoRefresh();
  $("back-to-stacks").classList.add("hidden");
  loadStacks();
}

async function loadEndpoints() {
  setStatus(text.loading);
  try {
    const result = await invoke("portainer/endpoints");
    state.endpoints = result.items || [];
    const select = $("endpoint");
    select.textContent = "";
    state.endpoints.forEach((endpoint) => {
      const option = document.createElement("option");
      option.value = String(endpoint.id);
      option.textContent = endpoint.name + (endpoint.status === 1 ? "" : " (offline)");
      select.appendChild(option);
    });
    const preferred = state.endpoints.find((endpoint) => endpoint.status === 1) || state.endpoints[0];
    if (preferred) {
      state.endpointId = preferred.id;
      select.value = String(preferred.id);
      await loadContainers();
    } else {
      setStatus(text.ready);
    }
  } catch (error) {
    setStatus(errorMessage(error), "error");
  }
}

/* ------------------------------------------------------------------- details */

async function selectContainer(containerId) {
  state.selectedId = containerId;
  state.detail = null;
  state.logs = [];
  state.selectedLines = new Set();
  renderSidebar();
  const container = state.containers.find((item) => item.id === containerId);
  if (!container) {
    return;
  }
  renderOverview(container, null);
  renderConnect(container);
  setTab(state.tab);
  try {
    const detail = await invoke("portainer/inspect", { endpointId: state.endpointId, containerId: containerId });
    if (state.selectedId !== containerId) {
      return;
    }
    state.detail = detail;
    renderOverview(container, detail);
    renderInspect(detail);
    setStatus(text.ready);
  } catch (error) {
    setStatus(errorMessage(error), "error");
  }
  if (state.tab === "stats") {
    loadStats();
  }
  if (state.tab === "logs") {
    loadLogs();
  }
}

function renderOverview(container, detail) {
  const panel = $("panel-overview");
  panel.textContent = "";
  appendRow(panel, "Name", container.name);
  appendRow(panel, "Image", container.image);
  appendRow(panel, "State", container.status || container.state);
  appendRow(panel, text.restartPolicy, detail && detail.restartPolicy);
  appendRow(panel, text.started, detail && detail.startedAt);
  appendRow(panel, text.finished, detail && detail.finishedAt);
  const ports = (detail && detail.ports ? detail.ports : [])
    .map((port) => port.containerPort + (port.hostPort ? " -> " + (port.hostIp || "0.0.0.0") + ":" + port.hostPort : ""))
    .join("\n");
  appendRow(panel, text.ports, ports);
  const mounts = (detail && detail.mounts ? detail.mounts : []).map((mount) => mount.source + " -> " + mount.destination).join("\n");
  appendRow(panel, text.mounts, mounts);
  const networks = (detail && detail.networks ? detail.networks : [])
    .map((network) => network.name + (network.ipAddress ? " " + network.ipAddress : ""))
    .join("\n");
  appendRow(panel, text.networks, networks);
  appendRow(panel, text.env, detail && detail.env ? detail.env.join(", ") : "");
}

function renderInspect(detail) {
  const panel = $("panel-inspect");
  panel.textContent = "";
  if (!detail) {
    const hint = document.createElement("p");
    hint.className = "hint";
    hint.textContent = text.inspectHint;
    panel.appendChild(hint);
    return;
  }
  appendRow(panel, "Id", detail.id);
  appendRow(panel, "Name", detail.name);
  appendRow(panel, "Image", detail.image);
  appendRow(panel, "State", detail.state + (detail.running ? " (" + text.running + ")" : ""));
  appendRow(panel, text.restartPolicy, detail.restartPolicy);
  appendRow(panel, text.started, detail.startedAt);
  appendRow(panel, text.finished, detail.finishedAt);
  const ports = (detail.ports || [])
    .map((port) => port.containerPort + (port.hostPort ? " -> " + (port.hostIp || "0.0.0.0") + ":" + port.hostPort : ""))
    .join("\n");
  appendRow(panel, text.ports, ports);
  const mounts = (detail.mounts || []).map((mount) => mount.source + " -> " + mount.destination).join("\n");
  appendRow(panel, text.mounts, mounts);
  const networks = (detail.networks || []).map((network) => network.name + (network.ipAddress ? " " + network.ipAddress : "")).join("\n");
  appendRow(panel, text.networks, networks);
  appendRow(panel, text.env, (detail.env || []).join(", "));
}

function renderConnect(container) {
  const panel = $("panel-connect");
  panel.textContent = "";
  const database = state.databases.get(container.id);
  const hint = document.createElement("p");
  hint.className = "hint";
  if (!database) {
    hint.textContent = text.notDb;
    panel.appendChild(hint);
    return;
  }
  hint.textContent = text.dbHint;
  panel.appendChild(hint);
  appendRow(panel, "DBX type", database.dbxType);
  appendRow(panel, "Host", database.host || "(unknown)");
  appendRow(panel, "Port", String(database.port));
  appendRow(panel, "Container", database.containerName);
  appendRow(panel, "Stack", container.stack);
  const button = document.createElement("button");
  button.type = "button";
  button.textContent = text.copy;
  button.style.marginTop = "12px";
  button.addEventListener("click", async () => {
    copyText(database.host + ":" + database.port + "  type=" + database.dbxType);
  });
  panel.appendChild(button);
}

async function loadStats() {
  if (!state.selectedId) {
    return;
  }
  setStatus(text.loading);
  try {
    const values = await invoke("portainer/stats", { endpointId: state.endpointId, containerId: state.selectedId });
    renderStats(values);
    setStatus(text.ready);
  } catch (error) {
    setStatus(errorMessage(error), "error");
  }
}

function statMeter(parent, label, percent, detail) {
  const wrapper = document.createElement("div");
  wrapper.className = "stat-row";
  const caption = document.createElement("div");
  caption.className = "stat-label";
  const left = document.createElement("b");
  left.textContent = label;
  const right = document.createElement("span");
  right.textContent = detail;
  caption.appendChild(left);
  caption.appendChild(right);
  wrapper.appendChild(caption);
  const meter = document.createElement("div");
  meter.className = "meter";
  const fill = document.createElement("i");
  fill.style.width = Math.max(0, Math.min(100, percent)) + "%";
  meter.appendChild(fill);
  wrapper.appendChild(meter);
  parent.appendChild(wrapper);
}

function renderStats(values) {
  const panel = $("panel-stats");
  panel.textContent = "";
  const cpu = Number(values.cpuPercent) || 0;
  const memPercent = Number(values.memoryPercent) || 0;
  statMeter(panel, text.statsCpu, cpu, cpu.toFixed(2) + "%");
  statMeter(
    panel,
    text.statsMemory,
    memPercent,
    formatBytes(values.memoryUsage) + " / " + formatBytes(values.memoryLimit) + " (" + memPercent.toFixed(1) + "%)",
  );
  const head = document.createElement("div");
  head.className = "stat-label";
  head.style.marginTop = "6px";
  const net = document.createElement("b");
  net.textContent = text.statsNet;
  head.appendChild(net);
  panel.appendChild(head);
  appendRow(panel, text.statsReceived, formatBytes(values.networkRx));
  appendRow(panel, text.statsSent, formatBytes(values.networkTx));
  const blockHead = document.createElement("div");
  blockHead.className = "stat-label";
  blockHead.style.marginTop = "6px";
  const block = document.createElement("b");
  block.textContent = text.statsBlock;
  blockHead.appendChild(block);
  panel.appendChild(blockHead);
  appendRow(panel, text.statsRead, formatBytes(values.blockRead));
  appendRow(panel, text.statsWrite, formatBytes(values.blockWrite));
  appendRow(panel, text.statsPids, String(values.pids || 0));
  appendRow(panel, text.statsSampledAt, values.read);
}

/* ---------------------------------------------------------------------- logs */

function stopAutoRefresh() {
  if (state.logTimer) {
    clearInterval(state.logTimer);
    state.logTimer = null;
  }
}

function syncAutoRefresh() {
  stopAutoRefresh();
  if (state.tab === "logs" && $("log-auto-refresh").checked && state.selectedId) {
    state.logTimer = setInterval(() => {
      loadLogs(true);
    }, AUTO_REFRESH_MS);
  }
}

async function loadLogs(silent) {
  if (!state.selectedId) {
    return;
  }
  if (!silent) {
    setStatus(text.loading);
  }
  try {
    const result = await invoke("portainer/logs", {
      endpointId: state.endpointId,
      containerId: state.selectedId,
      tail: Number($("log-lines").value) || 100,
      timestamps: $("log-timestamps").checked,
      range: $("log-range").value,
    });
    state.logs = String(result.text || "").split(/\r?\n/);
    if (state.logs.length && state.logs[state.logs.length - 1] === "") {
      state.logs.pop();
    }
    state.selectedLines = new Set();
    renderLogs();
    if (!silent) {
      setStatus(result.truncated ? text.logTruncated : text.logLoaded + " (" + result.bytes + " bytes)");
    }
  } catch (error) {
    setStatus(errorMessage(error), "error");
  }
}

function filteredLogIndexes() {
  const needle = $("log-search").value.trim().toLowerCase();
  const indexes = [];
  state.logs.forEach((line, index) => {
    if (!needle || line.toLowerCase().indexOf(needle) >= 0) {
      indexes.push(index);
    }
  });
  return indexes;
}

function renderLogs() {
  const view = $("logs");
  view.textContent = "";
  view.classList.toggle("wrap", $("log-wrap").checked);
  const indexes = filteredLogIndexes();
  if (!indexes.length) {
    view.classList.add("empty");
    view.textContent = text.noLogs;
    updateSelectionButtons();
    return;
  }
  view.classList.remove("empty");
  const needle = $("log-search").value.trim();
  const limit = Math.min(indexes.length, MAX_RENDERED_LINES);
  const fragment = document.createDocumentFragment();
  for (let position = 0; position < limit; position += 1) {
    const index = indexes[position];
    const line = document.createElement("div");
    line.className = "log-line";
    line.dataset.index = String(index);
    if (state.selectedLines.has(index)) {
      line.classList.add("selected");
    }
    if (needle) {
      const lower = state.logs[index].toLowerCase();
      const at = lower.indexOf(needle.toLowerCase());
      if (at >= 0) {
        line.appendChild(document.createTextNode(state.logs[index].slice(0, at)));
        const mark = document.createElement("mark");
        mark.textContent = state.logs[index].slice(at, at + needle.length);
        line.appendChild(mark);
        line.appendChild(document.createTextNode(state.logs[index].slice(at + needle.length)));
      } else {
        line.textContent = state.logs[index];
      }
    } else {
      line.textContent = state.logs[index];
    }
    line.addEventListener("click", () => toggleLine(index, line));
    fragment.appendChild(line);
  }
  view.appendChild(fragment);
  if (indexes.length > limit) {
    const more = document.createElement("div");
    more.className = "log-line";
    more.style.opacity = "0.6";
    more.textContent = "... " + text.truncatedRender;
    view.appendChild(more);
  }
  updateSelectionButtons();
}

function toggleLine(index, node) {
  if (state.selectedLines.has(index)) {
    state.selectedLines.delete(index);
    node.classList.remove("selected");
  } else {
    state.selectedLines.add(index);
    node.classList.add("selected");
  }
  updateSelectionButtons();
}

function updateSelectionButtons() {
  const hasSelection = state.selectedLines.size > 0;
  $("log-copy-selected").disabled = !hasSelection;
  $("log-clear-selection").disabled = !hasSelection;
}

function selectedLogText() {
  return Array.from(state.selectedLines)
    .sort((left, right) => left - right)
    .map((index) => state.logs[index])
    .join("\n");
}

async function copyText(value) {
  try {
    await navigator.clipboard.writeText(value);
    setStatus(text.copied);
    return true;
  } catch (error) {
    setStatus(text.copyFailed, "error");
    return false;
  }
}

function downloadLogs() {
  try {
    const blob = new Blob([state.logs.join("\n")], { type: "text/plain;charset=utf-8" });
    const url = URL.createObjectURL(blob);
    const link = document.createElement("a");
    link.href = url;
    link.download = (state.selectedId || "container").slice(0, 12) + "-logs.txt";
    document.body.appendChild(link);
    link.click();
    document.body.removeChild(link);
    setTimeout(() => URL.revokeObjectURL(url), 4000);
  } catch (error) {
    setStatus(text.downloadFailed, "error");
  }
}

/* --------------------------------------------------------------------- shell */

function setTab(tab) {
  state.tab = tab;
  document.querySelectorAll(".tab").forEach((node) => {
    node.classList.toggle("active", node.getAttribute("data-tab") === tab);
  });
  ["overview", "logs", "inspect", "stats", "connect"].forEach((name) => {
    $("panel-" + name).classList.toggle("hidden", name !== tab);
  });
  stopAutoRefresh();
  if (tab === "logs" && state.selectedId) {
    loadLogs();
    syncAutoRefresh();
  }
  if (tab === "stats" && state.selectedId) {
    loadStats();
  }
}

function bindEvents() {
  $("reload").addEventListener("click", () => {
    if (state.view === "stacks") {
      if (state.currentStack) {
        enterStack(state.currentStack);
      } else {
        loadStacks();
      }
      return;
    }
    loadContainers();
  });
  $("endpoint").addEventListener("change", (event) => {
    state.endpointId = Number(event.target.value);
    state.containers = [];
    state.selectedId = "";
    state.currentStack = "";
    stopAutoRefresh();
    if (state.view === "stacks") {
      loadStacks();
    } else {
      loadContainers();
    }
  });
  $("view-containers").addEventListener("click", () => setView("containers"));
  $("view-stacks").addEventListener("click", () => setView("stacks"));
  $("back-to-stacks").addEventListener("click", backToStacks);
  $("only-db").addEventListener("change", (event) => {
    state.onlyDb = event.target.checked;
    renderSidebar();
  });
  $("filter").addEventListener("input", (event) => {
    state.filter = event.target.value;
    renderSidebar();
  });
  document.querySelectorAll(".tab").forEach((node) => {
    node.addEventListener("click", () => setTab(node.getAttribute("data-tab")));
  });
  $("log-wrap").addEventListener("change", renderLogs);
  $("log-search").addEventListener("input", renderLogs);
  $("log-timestamps").addEventListener("change", () => loadLogs());
  $("log-range").addEventListener("change", () => loadLogs());
  $("log-lines").addEventListener("change", () => loadLogs());
  $("log-auto-refresh").addEventListener("change", syncAutoRefresh);
  $("log-copy").addEventListener("click", () => copyText(state.logs.join("\n")));
  $("log-copy-selected").addEventListener("click", () => copyText(selectedLogText()));
  $("log-clear-selection").addEventListener("click", () => {
    state.selectedLines = new Set();
    renderLogs();
  });
  $("log-download").addEventListener("click", downloadLogs);
}

async function boot() {
  await window.dbxPlugin.ready;
  const locale = String(window.dbxPlugin.locale || "en").toLowerCase();
  text = locale.indexOf("zh") === 0 ? strings.zh : strings.en;
  applyStrings();

  const context = window.dbxPlugin.context || {};
  state.connectionId = context.connectionId || (context.connection ? context.connection.id : "") || "";

  if (!state.connectionId) {
    try {
      const sessions = await window.dbxPlugin.invoke("portainer/sessions", {});
      const ids = (sessions && sessions.connectionIds) || [];
      if (ids.length === 1) {
        state.connectionId = ids[0];
      }
    } catch (error) {
      // Ignore: the error below explains the missing connection.
    }
  }

  if (!state.connectionId) {
    setStatus(text.noConnection, "error");
    return;
  }

  bindEvents();
  await loadEndpoints();
}

boot().catch((error) => {
  setStatus(errorMessage(error), "error");
});