# 更新日志

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