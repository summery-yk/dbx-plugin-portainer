# 更新日志

## 0.2.3

- 修复工作台打开或插件进程重建后 `Portainer connection is not established` 错误：启动时通过宿主 `host.reopenConnection` 重新建立 sidecar 会话
- Kubernetes 环境新增专属 namespace 筛选器，可一次查看全部命名空间或只看指定 namespace 的 Pod 内容器
- Kubernetes 视图改为显示 namespace / Pod 内容器，列表、徽章和统计文案不再沿用 Docker 堆栈语义
- 新增 `portainer/namespaces` 方法；`portainer/containers`、`portainer/stacks`、`portainer/databases` 支持 `namespace` 参数
- Kubernetes 日志新增「上一轮容器日志」开关，便于排查 CrashLoopBackOff / OOMKilled 等重启问题；没有上一轮实例时返回友好提示而不是 HTTP 400
- 数据库连接面板在 Kubernetes 环境下显示 Namespace 而非 Stack

## 0.2.1

- 新增 Kubernetes 环境支持：Portainer 的 Docker 与 Kubernetes（`Type=5`）端点现在自动识别，分别调用对应的只读 API
- Kubernetes 端点支持：列出 Pod 内容器、按命名空间分组、查看容器详情、读取容器日志、识别数据库容器并生成集群内 Service DNS
- Kubernetes 容器 ID 使用可解码的紧凑编码（namespace / pod / container），UI 与 MCP 均可直接复用
- 日志面板支持 Kubernetes `tailLines` / `timestamps` / `sinceSeconds` / `sinceTime` 参数，并保持 512 KiB 体积上限
- 检查面板增加命名空间、Pod、容器、节点、重启次数与健康状态；环境变量仍只返回名称
- 统计面板接入 Kubernetes metrics API；有 resource limits 时按 limit 计算 CPU / 内存百分比，无 CPU limit 时按单核基准展示
- 统计、数据库发现、堆栈与 MCP 工具全部复用同一套 endpoint 类型路由，Docker 环境行为保持不变

## 0.2.0

- 新增 MCP 工具层：插件现在实现 DBX 的 `mcp/tools` 与 `mcp/call` 协议，向 AI 客户端暴露 9 个只读工具
- 新增通用 MCP 支撑包 `backend/mcp/`（零依赖、与业务无关，可整目录复用到其他 DBX 插件）
- 工具清单：`portainer_overview`、`portainer_list_environments`、`portainer_list_stacks`、`portainer_list_containers`、`portainer_find_containers`、`portainer_find_databases`、`portainer_inspect_container`、`portainer_container_logs`、`portainer_container_stats`
- 容器参数支持按名字或 ID 定位（模糊匹配会在歧义时报错并列出候选）
- 日志工具支持范围、行数、时间戳与关键字过滤，并限制返回体积
- 全部工具复用工作台已有的 sidecar 方法，UI 与 AI 的行为不会分叉

## 0.1.0

首个版本。

- 连接 Portainer 实例并列出环境（endpoint）与容器
- 堆栈视图：按 `com.docker.compose.project` 分组，显示运行数与服务数
- 容器状态徽章：healthy / unhealthy / starting / running / exited
- 数据库容器识别，覆盖 MySQL、MariaDB、Percona、TiDB、PostgreSQL、Redis / Valkey、MongoDB、Elasticsearch / OpenSearch、ClickHouse、Neo4j、MinIO、SQL Server、Oracle、Kingbase、达梦、Nacos
- 连接参数推导：从端口映射生成 host / port / 类型，可在 DBX 中直接建连
- 日志面板：自动刷新、自动换行、时间戳、获取范围、搜索、行数、下载、复制、复制所选行、取消选择
- 统计面板：CPU、内存、网络 I/O、块设备 I/O、PID
- 检查面板：镜像、状态、重启策略、端口、挂载、网络、环境变量名
- Docker 日志多路复用帧解码
- 只读实现：全部为 GET 请求