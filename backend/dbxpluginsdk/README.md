# DBX Plugin SDK (Go)

本目录是 [t8y2/dbx](https://github.com/t8y2/dbx) 的 Go 插件 SDK，源码取自
`plugins/sdk/go/dbx-plugin-sdk`，Apache-2.0 许可。

## 为什么随仓库自带

官方 CLI（`@dbx-app/plugin-cli`）在打包时会把 SDK 注入到构建环境，
但**仓库本身**若依赖一个外部 module 版本：

- 构建要联网，且依赖某个恰好存在的伪版本；
- 评审者 clone 仓库后无法离线复现二进制，也就无法核对「源码 ↔ 发布产物」。

因此这里保留一份源码副本，主模块用 `replace` 指向它：

```
// backend/go.mod
replace github.com/t8y2/dbx/plugins/sdk/go/dbx-plugin-sdk => ./dbxpluginsdk
```

效果是 `go build` 在 `GOPROXY=off` 下也能成功，任何人拿到的都是同一份 SDK。

## 同步上游

```bash
# 从上游取一份新的 SDK 覆盖本目录
curl -fsSL https://raw.githubusercontent.com/t8y2/dbx/plugin-cli-v0.1.10/plugins/sdk/go/dbx-plugin-sdk/sdk.go \
  -o backend/dbxpluginsdk/sdk.go
curl -fsSL https://raw.githubusercontent.com/t8y2/dbx/plugin-cli-v0.1.10/plugins/sdk/go/dbx-plugin-sdk/go.mod \
  -o backend/dbxpluginsdk/go.mod
```

升级后请重跑 `go test ./...` 与打包握手测试（`cmd/dbxpoke`）。

## 许可

DBX 插件 SDK 随 DBX 主仓库发布，Apache-2.0。见仓库根目录的 `LICENSE`。
