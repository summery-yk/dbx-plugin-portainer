# 项目约定

本仓库是 DBX 插件（Portainer）。以下是开发与提交约定，与全局规范并存。

## 结构

- `manifest.json`：运行时契约，Manifest v1 不接受未声明字段，改动后必须通过 `tools/verify-manifest.mjs` 校验。
- `dbx-plugin.toml`：打包配置，`[package].include` 决定哪些目录进入 `.dbxp`。
- `ui/`：沙箱工作台，纯静态 HTML/CSS/JS，无构建步骤，只通过 `window.dbxPlugin` 与宿主和 sidecar 通信。
- `backend/main.go`：Sidecar 全部逻辑，只允许使用 Go 标准库与本仓库内的 DBX SDK。
- `backend/third_party/dbx-plugin-sdk/`：DBX 官方 Go SDK 的本地副本（Apache-2.0），保持与上游一致，不要就地修改。

## 必须遵守

1. 保持只读：sidecar 只对 Portainer 发起 GET 请求；新增任何写操作（exec、start/stop、remove 等）必须先在 issue 中讨论。
2. 凭证处理：API 令牌只能通过 `binding: "secret"` 声明，不得进入 `config`、日志、事件或错误消息。
3. 不新增第三方依赖：`backend/go.mod` 除官方 SDK 外不应出现其它 require。
4. 提交前必须通过：
   - `gofmt -l backend`（无输出）
   - `cd backend && go build ./... && go vet ./...`
   - `node --check ui/app.js`
   - `node tools/verify-manifest.mjs`
5. 版本号：改动功能后同步递增 `manifest.json` 的 `version`，并在 `CHANGELOG.md` 记录。

## 本地联调

Sidecar 可以脱离 DBX 直接驱动，便于验证接口：

```bash
node tools/drive-sidecar.mjs   # 需要 PORTAINER_URL / PORTAINER_USER / PORTAINER_PASS / SIDECAR_EXE
```

界面可用任意静态服务器渲染，`window.dbxPlugin` 由测试桩提供即可。