# 第三方组件声明

本插件的源码中包含以下第三方组件。

## DBX Plugin SDK (Go)

- 位置：`backend/third_party/dbx-plugin-sdk/`
- 来源：https://github.com/t8y2/dbx （路径 `plugins/sdk/go/dbx-plugin-sdk`）
- 许可：Apache License 2.0
- 用途：实现 DBX Sidecar Protocol v1（JSON-RPC over stdio）的服务端骨架。
- 说明：该目录为官方 SDK 源码的本地副本，`backend/go.mod` 通过 `replace` 指向它，
  以便在离线环境下执行 `go build` / `go vet`。使用 `dbx-plugin package` 打包时，
  CLI 也会使用其自带的同版本 SDK。

## 图标

`assets/plugin.svg` 与 `assets/connection.svg` 为本项目自行绘制，无第三方素材。