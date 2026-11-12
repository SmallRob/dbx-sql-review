module github.com/SmallRob/dbx-sql-review

go 1.22

require github.com/t8y2/dbx/plugins/sdk/go/dbx-plugin-sdk v0.1.0

// SDK 随仓库自带（backend/dbxpluginsdk），保证构建可复现：
// 评审者 clone 仓库后无需联网即可编译出与发布产物一致的二进制。
// SDK 源码来自 t8y2/dbx（Apache-2.0），见 dbxpluginsdk/README.md。
replace github.com/t8y2/dbx/plugins/sdk/go/dbx-plugin-sdk => ./dbxpluginsdk
