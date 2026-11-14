# 发布到 DBX Store

本文件描述如何把 SQL Review 提交到 [t8y2/dbx-store](https://github.com/t8y2/dbx-store)。
规范来源是该仓库的 `CONTRIBUTING.md`，本文只讲本插件的具体操作。

## 一句话流程

1. 把本仓库推到 GitHub 公开仓库，打 tag 并发布 Release；
2. Release 触发的 CI 会按 5 个平台构建未签名候选 `.dbxp`，并生成 `release-candidates.json`；
3. Fork `t8y2/dbx-store`，开一个 PR，带上 `publishers/smallrob.json` 与 `candidates/io.github.smallrob.sql-review.json`；
4. DBX Store 评审并用官方密钥签名，签名产物回写到同一个 PR；
5. CI 变绿后由维护者合并。

## 1. 前置条件

- 仓库公开可读（评审者要能核对「源码 ↔ 二进制」的对应关系）；
- `manifest.json` 的 `id`、`version`、`publisher` 与候选元数据完全一致；
- 仓库里有 `LICENSE`（Apache-2.0）与 `.dbx-store.json`。

## 2. 发布 Release

```bash
git tag v0.1.27
git push origin main --tags
# 然后在 GitHub 上创建 Release（tag 用 v0.1.27），触发 .github/workflows/plugin-release.yml
```

CI（`t8y2/dbx/.github/workflows/plugin-release-reusable.yml@plugin-cli-v0.1.10`）会：

- 在 `linux-x64` / `linux-arm64` / `windows-x64` / `darwin-x64` / `darwin-arm64` 五个 runner 上各构建一个 `.dbxp`；
- 校验每个包的字节 sha256 与大小、确认包内**没有** `signature.json`（必须是未签名候选）、确认各平台 manifest 身份一致；
- 汇总出 `release-candidates.json` 并把全部产物挂到 Release。

发布后 Release 附件应包含：

```
io.github.smallrob.sql-review-0.1.27-linux-x64.dbxp
io.github.smallrob.sql-review-0.1.27-linux-arm64.dbxp
io.github.smallrob.sql-review-0.1.27-windows-x64.dbxp
io.github.smallrob.sql-review-0.1.27-darwin-x64.dbxp
io.github.smallrob.sql-review-0.1.27-darwin-arm64.dbxp
release-candidates.json
```

## 3. 准备候选元数据

### 3.1 publisher 记录（首次提交才需要）

`publishers/smallrob.json`：

```json
{
  "id": "smallrob",
  "name": "SmallRob",
  "status": "unverified"
}
```

`verified` 标记由 DBX 维护者分配，提交时保持 `unverified`。

### 3.2 候选文件

`candidates/io.github.smallrob.sql-review.json` 的 `targets` 必须逐个平台的
`sha256` 与 `size`——这两个值**钉死被评审的字节**，签名时任何不匹配都会被拒。

最省事的方式是用 store 仓库里的脚本从 Release 生成：

```bash
git clone https://github.com/t8y2/dbx-store.git
cd dbx-store
node scripts/sync-release-candidate.mjs \
  --repository smallrob/dbx-sql-review \
  --tag v0.1.27 \
  --release-candidates-url https://github.com/SmallRob/dbx-sql-review/releases/download/v0.1.27/release-candidates.json \
  --metadata-url https://raw.githubusercontent.com/smallrob/dbx-sql-review/v0.1.27/.dbx-store.json \
  --output candidates/io.github.smallrob.sql-review.json
```

脚本会从 `release-candidates.json` 取包内 manifest 生成的 `plugin` 身份，
并把 `.dbx-store.json` 的元数据合并进来。注意脚本以**包内 manifest 的
permissions 为准**，所以 `.dbx-store.json` 里的 `permissions` 必须与
`manifest.json` 一致——CI 里的 `verify.yml` 会检查这一点。

生成结果形如：

```json
{
  "schemaVersion": 1,
  "id": "io.github.smallrob.sql-review",
  "publisher": "smallrob",
  "version": "0.1.27",
  "name": "SQL Review",
  "description": "静态 SQL 评审台：规则化检查破坏性变更、迁移规范、性能与结构质量，不连接数据库、不执行被评审的 SQL。",
  "icon": "https://raw.githubusercontent.com/smallrob/dbx-sql-review/v0.1.27/assets/plugin.svg",
  "tags": ["sql", "review", "lint", "migration", "security"],
  "permissions": ["host.workbench"],
  "source": "https://github.com/SmallRob/dbx-sql-review/tree/v0.1.27",
  "homepage": "https://github.com/SmallRob/dbx-sql-review",
  "license": "Apache-2.0",
  "releaseNotes": "新增 PostgreSQL 静态评审与 SQL080–087 八条规则，规则总数增至 61；同时修复新版 DBX 的初始化兼容性。",
  "localizations": {
    "zh-CN": {
      "name": "SQL 评审台",
      "description": "静态 SQL 评审台：…"
    }
  },
  "targets": [
    {
      "target": "darwin-arm64",
      "url": "https://github.com/SmallRob/dbx-sql-review/releases/download/v0.1.27/io.github.smallrob.sql-review-0.1.27-darwin-arm64.dbxp",
      "sha256": "<64 位十六进制>",
      "size": 2856861
    }
  ]
}
```

## 4. 提 PR

Fork `t8y2/dbx-store`，新建分支，把上面两个文件放进对应目录：

```
publishers/smallrob.json
candidates/io.github.smallrob.sql-review.json
```

**不要**手工编辑 `plugins/*.json` 或 `catalog/index.json`——它们由签名工作流生成。

PR 模板需要如实填写这几项：

| 项 | 本插件的答案 |
|---|---|
| Plugin ID | `io.github.smallrob.sql-review` |
| Version | `0.1.27` |
| Publisher ID | `smallrob` |
| 源码仓库与 tag | `https://github.com/SmallRob/dbx-sql-review` @ `v0.1.27` |
| 能力与用户流程 | 工作台 + 两个命令（全屏标签页 / 底部面板）+ 工具栏入口；评审单段 SQL 或整个迁移目录 |
| 权限 | 仅 `host.workbench`（用于 `openWorkbench` / `closeWorkbench`） |
| 数据访问 | 不访问数据库、不访问凭据；只读用户提供的 SQL 文本与内存中的最近评审结果 |
| 网络访问 | 无。sidecar 不发起任何网络请求 |
| 内置 AI / MCP | 4 个只读工具：`sql_review`、`sql_review_result`、`sql_optimize`、`sql_rules` |
| 原生 sidecar 行为 | 含 Go 编译的 sidecar 二进制；不监听端口、不起子进程；仅在插件数据目录写 `config.json`（0600）与 `audit.jsonl`（append-only 哈希链） |
| License | Apache-2.0 |
| 支持地址 | https://github.com/SmallRob/dbx-sql-review/issues |

CI 会校验候选文件，此时**保持红色是预期行为**——错误信息是
`open candidate(s) awaiting DBX Store signing`，它会阻止未签名产物被合并。

## 5. 评审者可能会问的

### 「它会不会执行我的 SQL？」

不会。后端是纯词法分析器：它 tokenize SQL、匹配规则，**没有任何 `database/sql`
导入，没有任何数据库驱动**。二进制里不存在建立连接的能力。
`backend/cmd/dbxpoke` 会对打包产物做一次真实 JSON-RPC 握手，CI 每次都跑。

### 「为什么不用连库拿真实行数？」

因为连库会引入凭据处理、权限申请与审计负担，而这个插件的价值在**提交前**——
在代码还没落到任何环境的时候就能发现问题。要看真实表结构时，
用 DBX 自己的连接功能查看即可，评审不需要那个能力。

### 「源码和二进制能对上吗？」

能。SDK 随仓库自带（`backend/dbxpluginsdk/`），构建不依赖外网也不依赖
特定版本的 Go module proxy；CI 会跑 `go vet`、`go test`、五个平台交叉编译，
并对打包出的二进制做协议握手。任何人都能复现同样的构建。

### 「规则会不会误报到没法用？」

团队约定类规则（`SQL012` 幂等守卫、`SQL018` 必须写 USE、`SQL033` 列注释、
`SQL034` 显式引擎、`SQL070` SQL Server DDL 守卫）**默认关闭**，只在用户按自己仓库的规范显式开启时才生效。
其余规则是跨项目通用的 SQL 质量问题。

## 6. 版本更新

已上架插件再发新版时，只提交 `candidates/<plugin-id>.json`，且只改 `version`
与 `targets`；其余字段留空表示沿用目录里的现值。版本号必须严格递增，
已上架或已撤销的版本不能重提。
