package main

// 最佳实践目录：把「SQL 评审规则」升级为「可读可执行的优化建议」。
//
// 与 rules.go 的分工：
//   - rules.go   判定「哪里错了」，输出 finding；
//   - practices.go 解释「应该怎么做」，输出按方言组织的最佳实践条目。
//
// 内容主要整理自 ASDM AI 资源中心的《MySQL 数据库性能优化最佳实践》
// （https://asdm-platform.leansoftx.com/portal/practices/29），并按
// PostgreSQL / SQL Server / SQLite 各自的实现机制改写为对应方言条目。
// 本文件是纯静态知识目录：不连库、不执行 SQL、不发起网络请求。

import (
	"sort"
	"strings"
)

// Practice 是一条规则化的最佳实践。字段刻意与 RuleMeta 对齐（ID / 分类 /
// 严重度 / 适用方言），额外补充「为什么 / 推荐 / 反例 / 正反代码」，
// 便于在评审结果里直接给出可执行建议。
type Practice struct {
	ID            string   `json:"id"`
	Title         string   `json:"title"`
	Category      string   `json:"category"`
	CategoryLabel string   `json:"categoryLabel"`
	Severity      string   `json:"severity"`
	Dialects      []string `json:"dialects,omitempty"` // nil = 所有方言
	Summary       string   `json:"summary"`
	Why           string   `json:"why"`
	Recommend     string   `json:"recommend"`
	Avoid         string   `json:"avoid,omitempty"`
	GoodSQL       string   `json:"goodSql,omitempty"`
	BadSQL        string   `json:"badSql,omitempty"`
	Tags          []string `json:"tags,omitempty"`
	RuleIDs       []string `json:"ruleIds,omitempty"` // 关联的评审规则，用于评审时联动推荐
	Source        string   `json:"source,omitempty"`  // 出处章节
}

// appliesTo 判断条目是否适用于某方言。nil / 空 = 全方言通用。
func (p Practice) appliesTo(d Dialect) bool {
	if len(p.Dialects) == 0 {
		return true
	}
	for _, x := range p.Dialects {
		if strings.EqualFold(x, string(d)) {
			return true
		}
	}
	return false
}

const (
	bpIndex       = "index"
	bpQuery       = "query"
	bpSchema      = "schema"
	bpTransaction = "transaction"
	bpMemory      = "memory"
	bpConfig      = "config"
	bpArch        = "architecture"
	bpDiagnosis   = "diagnosis"
)

var practiceCategoryLabels = map[string]string{
	bpIndex:       "索引优化",
	bpQuery:       "查询优化",
	bpSchema:      "表结构优化",
	bpTransaction: "事务与锁",
	bpMemory:      "内存与缓冲",
	bpConfig:      "配置参数",
	bpArch:        "架构扩展",
	bpDiagnosis:   "监控诊断",
}

var practiceCategoryOrder = []string{
	bpIndex, bpQuery, bpSchema, bpTransaction, bpMemory, bpConfig, bpArch, bpDiagnosis,
}

// practiceCatalog 是全部最佳实践条目。按「通用 → MySQL → PostgreSQL →
// SQL Server → SQLite」排列，前端可按方言与分类自由筛选。
var practiceCatalog = []Practice{
	// ============================================================ 通用（全方言）
	{ID: "BP-GEN-001", Title: "复合索引遵循最左前缀", Category: bpIndex, Severity: "high",
		Summary:   "查询条件必须从索引最左列开始，才能走 B+Tree 快速收敛。",
		Why:       "复合索引 (a,b,c) 等价于 (a)、(a,b)、(a,b,c) 三套前缀；跳过最左列无法定位，退化为全表扫描。范围条件右侧的列不再参与过滤。",
		Recommend: "把高频等值列放左边、范围列放右边；ORDER BY 列顺序与索引一致以避免额外排序。",
		Avoid:     "WHERE b=? 直接用 (a,b,c)；或对索引列套函数、做运算。",
		GoodSQL:   "CREATE INDEX idx ON orders(user_id, status, created_at);\nSELECT * FROM orders WHERE user_id=? AND status=?;",
		Tags:      []string{"索引", "最左前缀", "覆盖"}, RuleIDs: []string{"SQL025"}, Source: "一、索引优化"},

	{ID: "BP-GEN-002", Title: "优先使用覆盖索引", Category: bpIndex, Severity: "medium",
		Summary:   "查询列全部命中索引时无需回表，减少一次随机 I/O。",
		Why:       "回表是按主键随机读取聚簇索引页，是索引扫描之外的主要开销来源。",
		Recommend: "把 SELECT 与 WHERE 高频用到的列纳入复合索引；配合显式字段列表。",
		GoodSQL:   "-- idx(status,user_id,username)\nSELECT user_id, username FROM users WHERE status=1;",
		Tags:      []string{"索引", "覆盖", "回表"}, RuleIDs: []string{"SQL022"}, Source: "1.1 索引设计原则"},

	{ID: "BP-GEN-003", Title: "索引列选区分度高的值", Category: bpIndex, Severity: "medium",
		Summary:   "选择性越接近 1 越适合建索引；低区分度列不建单列索引。",
		Why:       "COUNT(DISTINCT col)/COUNT(*) 越低，索引过滤效果越差，还可能被优化器放弃。",
		Recommend: "性别、状态等低区分度列只作为复合索引后缀；高频等值列作前缀。",
		GoodSQL:   "SELECT COUNT(DISTINCT order_no)/COUNT(*) FROM orders; -- 接近 1 才适合",
		Tags:      []string{"索引", "选择性"}, Source: "1.1 索引设计原则"},

	{ID: "BP-GEN-004", Title: "不在索引列上使用函数或运算", Category: bpIndex, Severity: "high",
		Summary:   "函数/运算包裹索引列会让优化器无法使用索引。",
		Why:       "索引存的是列的原始值；对列求值后无法与索引键值匹配。",
		Recommend: "把常量移到右侧，改写为范围条件；需要函数过滤时用表达式索引/函数索引。",
		GoodSQL:   "-- 反例: WHERE YEAR(create_time)=2026\nWHERE create_time>='2026-01-01' AND create_time<'2027-01-01';",
		Tags:      []string{"索引", "函数", "范围查询"}, RuleIDs: []string{"SQL025"}, Source: "1.2 索引失效场景"},

	{ID: "BP-GEN-005", Title: "避免前导通配 LIKE", Category: bpQuery, Severity: "medium",
		Summary:   "LIKE '%x' 无法使用 B+Tree 前缀索引，必然全表扫描。",
		Why:       "B+Tree 按前缀有序，左侧模糊时无法定位起点。",
		Recommend: "改为后缀匹配 LIKE 'x%'；复杂搜索用全文索引或搜索引擎。",
		BadSQL:    "SELECT * FROM users WHERE name LIKE '%张%';",
		GoodSQL:   "SELECT * FROM users WHERE name LIKE '张%';",
		Tags:      []string{"查询", "LIKE", "全表扫描"}, RuleIDs: []string{"SQL024"}, Source: "1.1 / 2.1"},

	{ID: "BP-GEN-006", Title: "显式字段列表，不用 SELECT *", Category: bpQuery, Severity: "low",
		Summary:   "只取需要的列，降低传输与内存占用，并为覆盖索引创造条件。",
		Why:       "SELECT * 随表结构变化返回多余列，破坏隐式依赖与覆盖索引。",
		Recommend: "逐个列出需要的字段。",
		GoodSQL:   "SELECT id, username, email FROM users WHERE status=1;",
		Tags:      []string{"查询", "覆盖索引"}, RuleIDs: []string{"SQL022"}, Source: "2.1 SQL 编写规范"},

	{ID: "BP-GEN-007", Title: "JOIN 优先于 IN 子查询", Category: bpQuery, Severity: "low",
		Summary:   "IN 子查询可能被物化；显式 JOIN 优化更成熟、语义更清晰。",
		Why:       "子查询在不同版本/方言下可能被物化或转为 semijoin，执行计划不稳定。",
		Recommend: "改写为 INNER JOIN；按小表驱动、连接列有索引。",
		GoodSQL:   "SELECT o.id,o.amount FROM orders o JOIN users u ON o.user_id=u.id WHERE u.status=1;",
		Tags:      []string{"查询", "JOIN", "子查询"}, RuleIDs: []string{"SQL027", "SQL039"}, Source: "2.1 SQL 编写规范"},

	{ID: "BP-GEN-008", Title: "深分页改用游标或延迟关联", Category: bpQuery, Severity: "medium",
		Summary:   "OFFSET 越大，扫描并丢弃的行越多。",
		Why:       "LIMIT offset,n 需先扫描 offset+n 行再丢弃前 offset 行。",
		Recommend: "连续翻页用游标 WHERE id>? ；跳页用覆盖索引子查询先定位主键再回表。",
		GoodSQL:   "SELECT * FROM orders WHERE id>1000000 ORDER BY id LIMIT 20;",
		Tags:      []string{"查询", "分页", "延迟关联"}, RuleIDs: []string{"SQL023", "SQL028"}, Source: "2.1 SQL 编写规范"},

	{ID: "BP-GEN-009", Title: "等值多选使用 IN 而非多个 OR", Category: bpQuery, Severity: "low",
		Summary:   "同列等值用 IN 更简洁，优化器更容易走索引。",
		Why:       "多个 OR 条件可能触发索引合并失败或退化为全表扫描。",
		Recommend: "合并为 IN (…)；跨列 OR 拆成 UNION ALL。",
		GoodSQL:   "SELECT * FROM users WHERE status IN (1,2);",
		Tags:      []string{"查询", "IN", "OR"}, RuleIDs: []string{"SQL026", "SQL029"}, Source: "2.1 / 1.1"},

	{ID: "BP-GEN-010", Title: "批量写入合并语句", Category: bpQuery, Severity: "low",
		Summary:   "多值一次提交，减少事务开销与刷盘次数。",
		Why:       "逐条 INSERT 每条都有事务与日志开销，海量导入差距可达数十倍。",
		Recommend: "单批 500~1000 行；海量导入用方言专用批量加载工具。",
		GoodSQL:   "INSERT INTO logs(message) VALUES ('a'),('b'),('c');",
		Tags:      []string{"写入", "批量"}, RuleIDs: []string{"SQL007"}, Source: "2.1 SQL 编写规范"},

	{ID: "BP-GEN-011", Title: "INSERT 显式列清单", Category: bpSchema, Severity: "medium",
		Summary:   "不写列清单的 INSERT 依赖表物理列顺序，加列后会写错列。",
		Why:       "表结构演进后，位置式 VALUES 会把值塞进新列或类型不符的列。",
		Recommend: "始终写明 (col_a, col_b) 列清单。",
		GoodSQL:   "INSERT INTO t (col_a,col_b) VALUES (?,?);",
		Tags:      []string{"结构", "INSERT", "演进"}, RuleIDs: []string{"SQL038"}, Source: "结构质量"},

	{ID: "BP-GEN-012", Title: "金额用精确类型，禁用浮点", Category: bpSchema, Severity: "medium",
		Summary:   "金额/配额用 DECIMAL 或最小货币单位整数，禁止 FLOAT/DOUBLE。",
		Why:       "二进制浮点存在舍入误差，累加与比较会产生分厘偏差。",
		Recommend: "DECIMAL(p,2) 或 BIGINT 存分。",
		Tags:      []string{"结构", "类型", "金额"}, RuleIDs: []string{"SQL032"}, Source: "3.1 数据类型选择"},

	{ID: "BP-GEN-013", Title: "事务短小，外部调用移出事务", Category: bpTransaction, Severity: "high",
		Summary:   "事务内只放数据库操作，HTTP/消息/文件处理一律移出。",
		Why:       "外部调用期间行锁被独占，网络抖动会让所有触碰该行的请求排队。",
		Recommend: "先提交再触发外部调用；需一致性则引入消息表 + 异步投递。",
		Avoid:     "BEGIN → UPDATE → 外部接口(2s) → COMMIT",
		Tags:      []string{"事务", "锁", "短事务"}, Source: "4.1 事务设计原则"},

	{ID: "BP-GEN-014", Title: "大事务拆批提交", Category: bpTransaction, Severity: "medium",
		Summary:   "百万行级 UPDATE/DELETE 拆批，每批间隔短暂 sleep。",
		Why:       "单事务过大导致锁范围大、undo 堆积、主从延迟与回滚成本高。",
		Recommend: "每批几千行以内，带 ORDER BY 主键分批循环。",
		GoodSQL:   "DELETE FROM audit_log WHERE create_time<'2025-01-01' ORDER BY id LIMIT 1000;",
		Tags:      []string{"事务", "分批", "主从延迟"}, Source: "4.1 事务设计原则"},

	{ID: "BP-GEN-015", Title: "NULL 列显式默认值", Category: bpSchema, Severity: "low",
		Summary:   "业务允许时统一 NOT NULL + 默认值。",
		Why:       "NULL 让索引统计和优化器估算复杂化，COUNT/聚合语义易错。",
		Recommend: "CREATE TABLE 时给 NOT NULL DEFAULT。",
		GoodSQL:   "email VARCHAR(255) NOT NULL DEFAULT ''",
		Tags:      []string{"结构", "NULL"}, Source: "3.2 NULL 与范式"},

	{ID: "BP-GEN-016", Title: "上线前过执行计划", Category: bpDiagnosis, Severity: "medium",
		Summary:   "所有 SQL 上线前用 EXPLAIN 校验访问类型与索引命中。",
		Why:       "静态评审无法替代真实数据量下的计划验证。",
		Recommend: "检查访问类型至少 range，无 filesort/temporary（或已评估）。",
		Tags:      []string{"诊断", "EXPLAIN", "上线"}, Source: "2.2 执行计划分析 / 附录 B"},

	// ============================================================ MySQL
	{ID: "BP-MY-001", Title: "主键有序，避免随机 UUID", Category: bpIndex, Severity: "high",
		Dialects:  []string{"mysql"},
		Summary:   "自增主键或雪花 ID 保证顺序插入，避免 B+Tree 页分裂与碎片。",
		Why:       "随机 UUID 使插入分散到不同页，频繁页分裂、碎片化并放大聚簇索引写放大。",
		Recommend: "BIGINT UNSIGNED AUTO_INCREMENT；需全局 ID 用雪花或 UUID_TO_BIN(uuid(),1) 转换。",
		Tags:      []string{"索引", "主键", "InnoDB"}, RuleIDs: []string{"SQL030", "SQL036"}, Source: "1.1 索引设计原则"},

	{ID: "BP-MY-002", Title: "长字符列使用前缀索引", Category: bpIndex, Severity: "medium",
		Dialects:  []string{"mysql"},
		Summary:   "对长 VARCHAR 用列前缀索引，权衡存储与选择性。",
		Why:       "整列索引体积大、写放大高；选够用的最短前缀可显著减小索引。",
		Recommend: "先算不同前缀长度的选择性，再 ADD INDEX (col(n))。",
		GoodSQL:   "ALTER TABLE users ADD INDEX idx_email (email(12));",
		Tags:      []string{"索引", "前缀索引"}, RuleIDs: []string{"SQL036"}, Source: "1.1 索引设计原则"},

	{ID: "BP-MY-003", Title: "控制索引数量（单表 ≤ 5~6）", Category: bpIndex, Severity: "medium",
		Dialects:  []string{"mysql"},
		Summary:   "每个二级索引都增加 DML 维护成本与优化器选错计划的概率。",
		Why:       "索引过多拉低写入吞吐、占用空间，并让计划选择更脆弱。",
		Recommend: "定期用 sys.schema_unused_indexes 找无用索引，用不可见索引灰度下线。",
		GoodSQL:   "SELECT * FROM sys.schema_redundant_indexes;\nALTER TABLE orders ALTER INDEX idx_old INVISIBLE;",
		Tags:      []string{"索引", "维护", "冗余"}, RuleIDs: []string{"SQL021"}, Source: "1.3 索引维护"},

	{ID: "BP-MY-004", Title: "用 ANALYZE 校准统计信息", Category: bpIndex, Severity: "medium",
		Dialects:  []string{"mysql"},
		Summary:   "大批量写入后主动刷新统计，避免计划跳变。",
		Why:       "优化器依赖基数估算行数，统计过期会选错索引。",
		Recommend: "批量导入/更新后执行 ANALYZE TABLE。",
		GoodSQL:   "ANALYZE TABLE orders;",
		Tags:      []string{"索引", "统计信息"}, Source: "1.3 索引维护"},

	{ID: "BP-MY-005", Title: "利用 8.0 索引新特性", Category: bpIndex, Severity: "low",
		Dialects:  []string{"mysql"},
		Summary:   "函数索引、降序索引、不可见索引、ICP 按版本启用。",
		Why:       "函数索引替代「函数导致索引失效」；降序索引免额外排序；不可见索引用于灰度下线。",
		Recommend: "8.0.13+ 用函数索引处理表达式过滤；ORDER BY DESC 配降序索引。",
		GoodSQL:   "ALTER TABLE orders ADD INDEX idx_year ((YEAR(create_time)));\nALTER TABLE t ADD INDEX idx (created_at DESC);",
		Tags:      []string{"索引", "MySQL8", "函数索引"}, Source: "1.4 MySQL 8.0 索引新特性"},

	{ID: "BP-MY-006", Title: "字符集统一 utf8mb4", Category: bpSchema, Severity: "medium",
		Dialects:  []string{"mysql"},
		Summary:   "utf8/utf8mb3 无法存 emoji，且新旧表 JOIN 会隐式转换导致索引失效。",
		Why:       "字符集/排序规则不一致的连接列无法使用索引。",
		Recommend: "统一 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci。",
		Tags:      []string{"结构", "字符集", "utf8mb4"}, RuleIDs: []string{"SQL031"}, Source: "3.1 数据类型选择"},

	{ID: "BP-MY-007", Title: "显式声明 ENGINE 与在线 DDL", Category: bpSchema, Severity: "medium",
		Dialects:  []string{"mysql"},
		Summary:   "显式 ENGINE=InnoDB；结构性 ALTER 声明 ALGORITHM/LOCK。",
		Why:       "依赖实例默认引擎造成环境差异；未声明算法的 ALTER 在大表上可能锁表拷贝。",
		Recommend: "加 ALGORITHM=INPLACE,LOCK=NONE（或 INSTANT）；超大表用 gh-ost/pt-osc。",
		GoodSQL:   "ALTER TABLE t ADD COLUMN c INT, ALGORITHM=INPLACE, LOCK=NONE;",
		Tags:      []string{"结构", "DDL", "在线变更"}, RuleIDs: []string{"SQL034", "SQL020", "SQL021"}, Source: "3.4 大表在线 DDL"},

	{ID: "BP-MY-008", Title: "Buffer Pool 设为物理内存 50%~75%", Category: bpMemory, Severity: "high",
		Dialects:  []string{"mysql"},
		Summary:   "数据集小于内存时加大缓冲池即可消除大部分物理读。",
		Why:       "逻辑读穿透 Buffer Pool 才变成物理 I/O；命中率 <95% 说明内存不足或有全表扫描。",
		Recommend: "专用服务器设 50%~75%；OLTP 命中率应 >99%。支持在线调整。",
		GoodSQL:   "SET GLOBAL innodb_buffer_pool_size = 42949672960; -- 40G",
		Tags:      []string{"内存", "BufferPool", "命中率"}, Source: "5.1 / 5.2 Buffer Pool"},

	{ID: "BP-MY-009", Title: "线程级内存参数保持小默认值", Category: bpMemory, Severity: "high",
		Dialects:  []string{"mysql"},
		Summary:   "sort/join/read 缓冲按连接分配，禁止全局调大。",
		Why:       "峰值内存 ≈ 全局 + max_connections × 会话缓冲；500 连接各 64M 就是 32G，OOM 典型成因。",
		Recommend: "全局保持 256K~4M；特定大查询用 SET SESSION 单独放大。",
		GoodSQL:   "SET SESSION sort_buffer_size=8M; -- 仅当前会话",
		Tags:      []string{"内存", "连接", "OOM"}, Source: "5.3 全局内存与线程内存"},

	{ID: "BP-MY-010", Title: "redo 容量与双 1 刷盘", Category: bpConfig, Severity: "high",
		Dialects:  []string{"mysql"},
		Summary:   "redo log 容纳高峰约 1 小时写入；金融场景 flush_log_at_trx_commit=1 且 sync_binlog=1。",
		Why:       "redo 过小导致频繁 checkpoint 与写入抖动；双 1 保证每次提交刷盘满足 ACID。",
		Recommend: "8.0.30+ 用 innodb_redo_log_capacity；O_DIRECT 避免双重缓存。",
		GoodSQL:   "innodb_redo_log_capacity=2G;\ninnodb_flush_log_at_trx_commit=1;\nsync_binlog=1;",
		Tags:      []string{"配置", "redo", "持久化"}, Source: "6.1 InnoDB 核心参数"},

	{ID: "BP-MY-011", Title: "连接数配合应用连接池", Category: bpConfig, Severity: "medium",
		Dialects:  []string{"mysql"},
		Summary:   "max_connections 只需覆盖连接池总数×实例数+余量。",
		Why:       "连接数不是越大越好：每连接内存叠加 + 上下文切换成本。",
		Recommend: "应用侧用 HikariCP 等收敛并发；wait_timeout/interactive_timeout 清理僵尸连接。",
		GoodSQL:   "max_connections=500;\nwait_timeout=600;\ninteractive_timeout=600;\nskip_name_resolve=ON;",
		Tags:      []string{"配置", "连接", "连接池"}, Source: "6.2 连接与线程参数"},

	{ID: "BP-MY-012", Title: "高并发写入评估 READ COMMITTED", Category: bpTransaction, Severity: "medium",
		Dialects:  []string{"mysql"},
		Summary:   "降为 READ COMMITTED 可减少间隙锁冲突（前提 binlog_format=ROW）。",
		Why:       "RR 下范围当前读用 next-key lock，间隙锁冲突在高并发写入时更严重。",
		Recommend: "transaction_isolation=READ-COMMITTED；binlog_format=ROW。",
		GoodSQL:   "transaction_isolation=READ-COMMITTED;\nbinlog_format=ROW;",
		Tags:      []string{"事务", "隔离级别", "锁"}, Source: "4.2 隔离级别与 MVCC"},

	{ID: "BP-MY-013", Title: "死锁预防：固定访问顺序 + 应用层重试", Category: bpTransaction, Severity: "medium",
		Dialects:  []string{"mysql"},
		Summary:   "多行更新按固定顺序加锁；应用捕获 1213/1205 自动重试。",
		Why:       "死锁无法彻底消除，目标是降低频率并保证自动恢复。",
		Recommend: "批量 UPDATE 先按主键排序；为 UPDATE/DELETE 的 WHERE 建索引缩小锁范围。",
		GoodSQL:   "innodb_print_all_deadlocks=ON;\ninnodb_lock_wait_timeout=5;",
		Tags:      []string{"事务", "死锁", "锁"}, Source: "4.4 死锁预防与排查"},

	{ID: "BP-MY-014", Title: "长事务治理（>60s 告警）", Category: bpTransaction, Severity: "medium",
		Dialects:  []string{"mysql"},
		Summary:   "长事务持有旧 ReadView，导致 undo 无法 purge、Buffer Pool 老化与主从延迟。",
		Why:       "history list 持续增长通常是长事务导致，是「莫名变慢」的根源。",
		Recommend: "排查连接未提交路径；脚本设 autocommit=1；对 innodb_trx >60s 配置告警。",
		GoodSQL:   "SELECT * FROM information_schema.innodb_trx\nWHERE TIMESTAMPDIFF(SECOND,trx_started,NOW())>60;",
		Tags:      []string{"事务", "长事务", "undo"}, Source: "4.5 长事务治理"},

	{ID: "BP-MY-015", Title: "慢查询日志 + pt-query-digest 聚合", Category: bpDiagnosis, Severity: "medium",
		Dialects:  []string{"mysql"},
		Summary:   "开启慢日志并按总耗时聚合，持续治理 TOP SQL。",
		Why:       "慢查询增量持续增长即需关注；按指纹聚合才能定位最耗时的 SQL 模式。",
		Recommend: "long_query_time=1；每周用 pt-query-digest 分析。log_queries_not_using_indexes 谨慎开启。",
		GoodSQL:   "slow_query_log=1;\nlong_query_time=1;\n# pt-query-digest /var/log/mysql/slow.log",
		Tags:      []string{"诊断", "慢查询", "监控"}, Source: "10.3 慢查询日志"},

	{ID: "BP-MY-016", Title: "用 sys / performance_schema 定位瓶颈", Category: bpDiagnosis, Severity: "low",
		Dialects:  []string{"mysql"},
		Summary:   "按指纹看语句耗时、全表扫描、锁等待与文件 I/O。",
		Why:       "sys 库把皮秒计数器翻译成可读格式，是日常诊断首选入口。",
		Recommend: "查 statements_with_runtimes_in_95th_percentile、statements_with_full_table_scans、innodb_lock_waits。",
		GoodSQL:   "SELECT * FROM sys.statements_with_full_table_scans\nORDER BY no_index_used_count DESC LIMIT 10;",
		Tags:      []string{"诊断", "sys", "performance_schema"}, Source: "八 / 九 性能视图"},

	{ID: "BP-MY-017", Title: "分区表先确认查询带分区键", Category: bpArch, Severity: "low",
		Dialects:  []string{"mysql"},
		Summary:   "千万级以上、查询总带分区键、需按分区归档时再分区。",
		Why:       "分区裁剪只在 WHERE 含分区键时生效；所有唯一键必须包含分区键，且不支持外键。",
		Recommend: "不带分区键的查询会扫全部分区；先穷尽索引+缓存+归档。",
		GoodSQL:   "PARTITION BY RANGE (YEAR(create_time)) (...)",
		Tags:      []string{"架构", "分区"}, Source: "3.3 分区表策略"},

	{ID: "BP-MY-018", Title: "读写分离与缓存分层", Category: bpArch, Severity: "low",
		Dialects:  []string{"mysql"},
		Summary:   "ProxySQL/MySQL Router 路由读写；强一致请求强制走主。",
		Why:       "从库延迟下写后立即读会读到旧数据；缓存引入穿透/击穿/雪崩需治理。",
		Recommend: "监控 Seconds_Behind_Source；Cache-Aside 回填+过期兜底，过期时间加随机抖动。",
		Tags:      []string{"架构", "读写分离", "缓存"}, Source: "7.1 / 7.3 架构优化"},

	// ============================================================ PostgreSQL
	{ID: "BP-PG-001", Title: "生产环境建索引用 CONCURRENTLY", Category: bpIndex, Severity: "high",
		Dialects:  []string{"postgres"},
		Summary:   "普通 CREATE INDEX 持有写锁，阻塞 INSERT/UPDATE/DELETE。",
		Why:       "并发建索引不阻塞写入，但不能在事务块内执行，失败会留下 INVALID 索引。",
		Recommend: "迁移脚本用 CREATE INDEX CONCURRENTLY；失败后用 REINDEX INDEX CONCURRENTLY 清理。",
		GoodSQL:   "CREATE INDEX CONCURRENTLY idx_orders_user ON orders(user_id);",
		Tags:      []string{"索引", "在线", "锁"}, RuleIDs: []string{"SQL081"}, Source: "PostgreSQL 在线变更"},

	{ID: "BP-PG-002", Title: "大表改类型走双写/回填，避免表重写", Category: bpSchema, Severity: "high",
		Dialects:  []string{"postgres"},
		Summary:   "ALTER COLUMN TYPE 通常重写整表并持强锁。",
		Why:       "表重写期间读写受阻，大表代价极高。",
		Recommend: "新增兼容列 → 双写回填 → 切换读 → 删旧列；维护窗口评估锁。",
		Tags:      []string{"结构", "DDL", "表重写"}, RuleIDs: []string{"SQL082"}, Source: "PostgreSQL DDL"},

	{ID: "BP-PG-003", Title: "SET NOT NULL 的安全四步", Category: bpSchema, Severity: "medium",
		Dialects:  []string{"postgres"},
		Summary:   "直接 SET NOT NULL 会全表校验持锁，有 NULL 则失败。",
		Why:       "校验阶段持 ACCESS EXCLUSIVE 锁，阻塞所有访问。",
		Recommend: "CHECK IS NOT NULL NOT VALID → VALIDATE CONSTRAINT → SET NOT NULL → 删临时 CHECK。",
		GoodSQL:   "ALTER TABLE t ADD CONSTRAINT c CHECK (col IS NOT NULL) NOT VALID;\nALTER TABLE t VALIDATE CONSTRAINT c;\nALTER TABLE t ALTER COLUMN col SET NOT NULL;",
		Tags:      []string{"结构", "NOT NULL", "锁"}, RuleIDs: []string{"SQL083"}, Source: "PostgreSQL DDL"},

	{ID: "BP-PG-004", Title: "主键用 IDENTITY，SERIAL 仅作兼容", Category: bpSchema, Severity: "low",
		Dialects:  []string{"postgres"},
		Summary:   "SERIAL 隐式建序列，权限与生命周期难管；IDENTITY 是标准写法。",
		Why:       "序列 owner 与权限失控会导致写入失败或越权。",
		Recommend: "新表用 GENERATED ALWAYS AS IDENTITY；兼容旧版时明确序列 owner。",
		GoodSQL:   "id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY",
		Tags:      []string{"结构", "主键", "序列"}, RuleIDs: []string{"SQL084"}, Source: "PostgreSQL 类型"},

	{ID: "BP-PG-005", Title: "JSON 用 JSONB", Category: bpSchema, Severity: "medium",
		Dialects:  []string{"postgres"},
		Summary:   "JSONB 已解析、可索引、查询高效；JSON 仅保留原始文本。",
		Why:       "JSON 保留重复键与空白，比较和索引能力弱。",
		Recommend: "统一 JSONB；高频查询键建表达式 GIN 索引。",
		GoodSQL:   "CREATE INDEX idx ON t USING GIN ((data->'tags'));",
		Tags:      []string{"结构", "JSONB", "GIN"}, RuleIDs: []string{"SQL085"}, Source: "PostgreSQL 类型"},

	{ID: "BP-PG-006", Title: "时间点用 TIMESTAMPTZ", Category: bpSchema, Severity: "low",
		Dialects:  []string{"postgres"},
		Summary:   "绝对时间点用带时区类型，避免跨时区歧义。",
		Why:       "TIMESTAMP WITHOUT TIME ZONE 不存偏移，跨时区写入与比较易错。",
		Recommend: "需要绝对时刻用 TIMESTAMPTZ；明确本地时刻才用无时区类型。",
		Tags:      []string{"结构", "时区", "时间类型"}, RuleIDs: []string{"SQL086"}, Source: "PostgreSQL 类型"},

	{ID: "BP-PG-007", Title: "EXPLAIN (ANALYZE, BUFFERS) 实测", Category: bpDiagnosis, Severity: "medium",
		Dialects:  []string{"postgres"},
		Summary:   "ANALYZE 真正执行输出耗时行数，BUFFERS 看命中/脏读。",
		Why:       "仅有 EXPLAIN 是估算，优化器可能误判。",
		Recommend: "上线前用 EXPLAIN (ANALYZE, BUFFERS) 校验；结合 pg_stat_statements 找 TOP SQL。",
		GoodSQL:   "EXPLAIN (ANALYZE, BUFFERS) SELECT * FROM orders WHERE user_id=100;",
		Tags:      []string{"诊断", "EXPLAIN", "统计"}, RuleIDs: []string{"SQL016"}, Source: "PostgreSQL 性能分析"},

	{ID: "BP-PG-008", Title: "定期 VACUUM ANALYZE", Category: bpMemory, Severity: "medium",
		Dialects:  []string{"postgres"},
		Summary:   "回收死元组并更新统计，控制表膨胀。",
		Why:       "MVCC 更新产生死元组，不清理会导致表膨胀、索引效率下降。",
		Recommend: "调优 autovacuum；大批量写入后手动 VACUUM (ANALYZE)。",
		GoodSQL:   "VACUUM (ANALYZE) orders;",
		Tags:      []string{"内存", "VACUUM", "膨胀"}, Source: "PostgreSQL 维护"},

	{ID: "BP-PG-009", Title: "连接池收敛连接数", Category: bpConfig, Severity: "medium",
		Dialects:  []string{"postgres"},
		Summary:   "PG 每连接是独立进程，连接数过高开销大；用 pgbouncer 收敛。",
		Why:       "大量空闲连接消耗内存与进程调度，超过 max_connections 直接拒绝。",
		Recommend: "事务级复用连接；配合 idle_in_transaction_session_timeout。",
		Tags:      []string{"配置", "连接池", "pgbouncer"}, Source: "PostgreSQL 配置"},

	{ID: "BP-PG-010", Title: "避免 DROP/TRUNCATE CASCADE", Category: bpArch, Severity: "high",
		Dialects:  []string{"postgres"},
		Summary:   "CASCADE 递归删除依赖对象，影响范围常超预期。",
		Why:       "视图、外键、序列等依赖会被一并删除且不可回滚。",
		Recommend: "先查 pg_depend；迁移脚本避免 CASCADE，确需时列出影响清单单独评审。",
		Tags:      []string{"架构", "CASCADE", "依赖"}, RuleIDs: []string{"SQL087"}, Source: "PostgreSQL DDL"},

	// ============================================================ SQL Server
	{ID: "BP-MS-001", Title: "聚簇索引键窄、静态、递增", Category: bpIndex, Severity: "high",
		Dialects:  []string{"sqlserver"},
		Summary:   "聚簇键被所有非聚簇索引引用，应窄、不变、递增。",
		Why:       "宽键或随机键放大所有非聚簇索引并造成页分裂。",
		Recommend: "优先窄整型递增键；避免 GUID 作聚簇键（用顺序 GUID 或放非聚簇）。",
		Tags:      []string{"索引", "聚簇索引", "页分裂"}, RuleIDs: []string{"SQL030", "SQL076"}, Source: "SQL Server 索引设计"},

	{ID: "BP-MS-002", Title: "覆盖索引用 INCLUDE 包含列", Category: bpIndex, Severity: "medium",
		Dialects:  []string{"sqlserver"},
		Summary:   "把 SELECT 列放进 INCLUDE，避免键列变宽即可覆盖查询。",
		Why:       "包含列不参与排序和键大小限制，却能避免回表 key lookup。",
		Recommend: "等值/排序列放键，其余输出列放 INCLUDE。",
		GoodSQL:   "CREATE INDEX idx ON orders(user_id) INCLUDE (amount,status);",
		Tags:      []string{"索引", "INCLUDE", "覆盖"}, RuleIDs: []string{"SQL022"}, Source: "SQL Server 索引设计"},

	{ID: "BP-MS-003", Title: "避免 NOLOCK，改用 RCSI", Category: bpTransaction, Severity: "medium",
		Dialects:  []string{"sqlserver"},
		Summary:   "NOLOCK 会读未提交数据，出现重复行/漏行/页级不一致。",
		Why:       "脏读与不一致结果在报表与对账场景不可接受。",
		Recommend: "移除 NOLOCK；评估 READ_COMMITTED_SNAPSHOT 降低阻塞。",
		Tags:      []string{"事务", "隔离级别", "NOLOCK"}, RuleIDs: []string{"SQL071"}, Source: "SQL Server 隔离级别"},

	{ID: "BP-MS-004", Title: "TOP 必须配 ORDER BY", Category: bpQuery, Severity: "low",
		Dialects:  []string{"sqlserver"},
		Summary:   "TOP 无 ORDER BY 返回哪些行不确定，分批修数易漏行/重复。",
		Why:       "无排序时物理顺序不保证稳定。",
		Recommend: "为 TOP 补唯一稳定排序（通常主键），分批记录边界。",
		GoodSQL:   "SELECT TOP (1000) * FROM t ORDER BY id;",
		Tags:      []string{"查询", "TOP", "分页"}, RuleIDs: []string{"SQL072"}, Source: "SQL Server 查询"},

	{ID: "BP-MS-005", Title: "参数化查询与 sp_executesql", Category: bpQuery, Severity: "medium",
		Dialects:  []string{"sqlserver"},
		Summary:   "动态 SQL 用 sp_executesql 参数化值，标识符用 QUOTENAME。",
		Why:       "字符串拼接既带来注入风险，也让计划缓存膨胀。",
		Recommend: "值走参数，标识符用 QUOTENAME；覆盖所有分支评审。",
		GoodSQL:   "EXEC sp_executesql N'SELECT * FROM t WHERE id=@id',N'@id int',@id=1;",
		Tags:      []string{"查询", "动态 SQL", "计划缓存"}, RuleIDs: []string{"SQL073"}, Source: "SQL Server 动态 SQL"},

	{ID: "BP-MS-006", Title: "及时更新统计信息", Category: bpDiagnosis, Severity: "medium",
		Dialects:  []string{"sqlserver"},
		Summary:   "统计过期导致优化器选错计划；大批量写入后主动更新。",
		Why:       "基数估算依赖统计，过期会造成嵌套循环/哈希连接误选。",
		Recommend: "开启 AUTO_UPDATE_STATISTICS；大批量变更后 UPDATE STATISTICS WITH FULLSCAN。",
		GoodSQL:   "UPDATE STATISTICS orders WITH FULLSCAN;",
		Tags:      []string{"诊断", "统计信息", "执行计划"}, Source: "SQL Server 统计"},

	{ID: "BP-MS-007", Title: "MERGE 用 HOLDLOCK 防竞态", Category: bpTransaction, Severity: "medium",
		Dialects:  []string{"sqlserver"},
		Summary:   "并发下 MERGE 匹配与插入间可能竞态，造成唯一键冲突或重复。",
		Why:       "默认隔离级别下匹配阶段不加范围锁。",
		Recommend: "评估改用 UPDATE+INSERT；保留 MERGE 时用 WITH (HOLDLOCK) 并验证唯一约束。",
		GoodSQL:   "MERGE INTO t WITH (HOLDLOCK) USING src ON ...;",
		Tags:      []string{"事务", "MERGE", "并发"}, RuleIDs: []string{"SQL077"}, Source: "SQL Server 并发"},

	{ID: "BP-MS-008", Title: "多语言文本用 NVARCHAR", Category: bpSchema, Severity: "low",
		Dialects:  []string{"sqlserver"},
		Summary:   "VARCHAR 依赖代码页，跨环境存取中文/emoji 可能乱码。",
		Why:       "非 Unicode 类型受排序规则与代码页限制。",
		Recommend: "多语言用 NVARCHAR/NCHAR；避免 TEXT/NTEXT，改 (N)VARCHAR(MAX)。",
		Tags:      []string{"结构", "Unicode", "类型"}, RuleIDs: []string{"SQL075"}, Source: "SQL Server 类型"},

	{ID: "BP-MS-009", Title: "用 DMV 定位高耗 SQL", Category: bpDiagnosis, Severity: "low",
		Dialects:  []string{"sqlserver"},
		Summary:   "sys.dm_exec_query_stats 按逻辑读/CPU 找 TOP SQL。",
		Why:       "实际执行统计比静态规则更能反映线上瓶颈。",
		Recommend: "结合实际执行计划与 sys.dm_db_index_usage_stats 找缺失/无用索引。",
		GoodSQL:   "SELECT TOP 10 * FROM sys.dm_exec_query_stats\nORDER BY total_logical_reads DESC;",
		Tags:      []string{"诊断", "DMV", "监控"}, Source: "SQL Server 性能视图"},

	{ID: "BP-MS-010", Title: "tempdb 多文件与预分配", Category: bpConfig, Severity: "medium",
		Dialects:  []string{"sqlserver"},
		Summary:   "tempdb 文件数按逻辑 CPU 配置并预分配大小，减少分配争用。",
		Why:       "排序、哈希、临时表都依赖 tempdb，自动增长与单文件会造成闩锁争用。",
		Recommend: "多数据文件（通常 = 逻辑核数，上限 8），统一大小，启用即时文件初始化。",
		Tags:      []string{"配置", "tempdb", "闩锁"}, Source: "SQL Server 配置"},

	// ============================================================ SQLite
	{ID: "BP-LT-001", Title: "用 INTEGER PRIMARY KEY 作 rowid 别名", Category: bpIndex, Severity: "medium",
		Dialects:  []string{"sqlite"},
		Summary:   "INTEGER PRIMARY KEY 即 rowid 别名，自动递增；避免不必要的 AUTOINCREMENT。",
		Why:       "AUTOINCREMENT 额外建 sqlite_sequence 且禁止复用已删 id，多数场景不需要。",
		Recommend: "除非要求 id 永不复用，否则去掉 AUTOINCREMENT；主键保持整型。",
		GoodSQL:   "CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT);",
		Tags:      []string{"索引", "rowid", "主键"}, RuleIDs: []string{"SQL062", "SQL030"}, Source: "SQLite 类型与主键"},

	{ID: "BP-LT-002", Title: "按类型亲和性映射字段", Category: bpSchema, Severity: "info",
		Dialects:  []string{"sqlite"},
		Summary:   "SQLite 只有 TEXT/INTEGER/REAL/BLOB/NUMERIC 亲和性，JSON/DATETIME → TEXT，BOOLEAN → INTEGER。",
		Why:       "VARCHAR(n)/TINYINT(1) 之类不产生约束，容易误以为有校验。",
		Recommend: "按映射规则翻译类型；金额用 INTEGER 存分。",
		Tags:      []string{"结构", "类型亲和性", "映射"}, RuleIDs: []string{"SQL063"}, Source: "SQLite 类型映射"},

	{ID: "BP-LT-003", Title: "启用 WAL 提升并发", Category: bpConfig, Severity: "medium",
		Dialects:  []string{"sqlite"},
		Summary:   "WAL 模式允许读写并发，避免写阻塞读。",
		Why:       "默认 rollback journal 下写事务阻塞所有读。",
		Recommend: "PRAGMA journal_mode=WAL；设 busy_timeout 处理锁竞争；注意 WAL 文件检查点。",
		GoodSQL:   "PRAGMA journal_mode=WAL;\nPRAGMA busy_timeout=5000;",
		Tags:      []string{"配置", "WAL", "并发"}, Source: "SQLite 并发"},

	{ID: "BP-LT-004", Title: "VACUUM 不能放在事务内", Category: bpTransaction, Severity: "medium",
		Dialects:  []string{"sqlite"},
		Summary:   "多数迁移工具默认给每个迁移套事务，VACUUM 会直接失败。",
		Why:       "VACUUM 通过重建数据库文件回收空间，无法在事务中完成。",
		Recommend: "把 VACUUM 移到迁移之外，或用非事务脚本。",
		Tags:      []string{"事务", "VACUUM", "迁移"}, RuleIDs: []string{"SQL064"}, Source: "SQLite 迁移"},

	{ID: "BP-LT-005", Title: "改列类型走 12 步表重建", Category: bpSchema, Severity: "high",
		Dialects:  []string{"sqlite"},
		Summary:   "ALTER TABLE 仅支持改名/加列/删列，改类型约束需建新表搬数据。",
		Why:       "MODIFY/CHANGE COLUMN、ADD CONSTRAINT 等直接语法错误。",
		Recommend: "建新表 → INSERT SELECT 搬数据 → DROP 旧表 → RENAME → 重建索引/触发器；全程注意表不可用窗口。",
		Tags:      []string{"结构", "表重建", "DDL"}, RuleIDs: []string{"SQL060", "SQL065"}, Source: "SQLite 表重建"},

	{ID: "BP-LT-006", Title: "避免 ATTACH 的路径依赖", Category: bpArch, Severity: "low",
		Dialects:  []string{"sqlite"},
		Summary:   "ATTACH DATABASE 引入绝对路径依赖，CI 与生产易不一致。",
		Why:       "相对路径依赖运行目录，换环境常找不到库文件。",
		Recommend: "确认绝对路径与两端文件存在；跨库读写拆成独立通道。",
		Tags:      []string{"架构", "ATTACH", "路径"}, RuleIDs: []string{"SQL066"}, Source: "SQLite 多库"},

	{ID: "BP-LT-007", Title: "ANALYZE 更新查询计划统计", Category: bpDiagnosis, Severity: "low",
		Dialects:  []string{"sqlite"},
		Summary:   "数据分布变化后执行 ANALYZE，让查询 planner 选对索引。",
		Why:       "SQLite 依赖 sqlite_stat1 统计选择索引与连接顺序。",
		Recommend: "批量导入后 ANALYZE；用 EXPLAIN QUERY PLAN 验证索引命中。",
		GoodSQL:   "ANALYZE;\nEXPLAIN QUERY PLAN SELECT * FROM t WHERE user_id=?;",
		Tags:      []string{"诊断", "ANALYZE", "执行计划"}, Source: "SQLite 查询优化"},

	{ID: "BP-LT-008", Title: "迁移脚本不写 USE", Category: bpSchema, Severity: "high",
		Dialects:  []string{"sqlite"},
		Summary:   "SQLite 没有 USE 语句，库由连接串文件路径决定。",
		Why:       "执行到 USE 会直接语法错误。",
		Recommend: "删掉 USE，用库文件路径指定目标库。",
		Tags:      []string{"结构", "迁移", "USE"}, RuleIDs: []string{"SQL061"}, Source: "SQLite 迁移"},
}

// practiceIndex 按 ID 建索引；同时给空 CategoryLabel 填中文标签。
var practiceIndex = func() map[string]Practice {
	m := make(map[string]Practice, len(practiceCatalog))
	for _, p := range practiceCatalog {
		if p.CategoryLabel == "" {
			p.CategoryLabel = practiceCategoryLabels[p.Category]
		}
		if p.Severity == "" {
			p.Severity = "info"
		}
		m[p.ID] = p
	}
	return m
}()

// Practices 返回按方言/分类过滤后的条目（分类为空表示全部）。
func Practices(dialect Dialect, category string) []Practice {
	out := make([]Practice, 0, len(practiceCatalog))
	for _, p := range practiceCatalog {
		if dialect != "" && !p.appliesTo(dialect) {
			continue
		}
		if category != "" && p.Category != category {
			continue
		}
		if p.CategoryLabel == "" {
			p.CategoryLabel = practiceCategoryLabels[p.Category]
		}
		out = append(out, p)
	}
	// 按分类顺序、再按严重程度排序，保证输出稳定。
	catRank := map[string]int{}
	for i, c := range practiceCategoryOrder {
		catRank[c] = i
	}
	sort.SliceStable(out, func(i, j int) bool {
		if catRank[out[i].Category] != catRank[out[j].Category] {
			return catRank[out[i].Category] < catRank[out[j].Category]
		}
		if severityWeight[out[i].Severity] != severityWeight[out[j].Severity] {
			return severityWeight[out[i].Severity] > severityWeight[out[j].Severity]
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// PracticeCategories 返回某方言下实际出现的分类（含条目数）。
func PracticeCategories(dialect Dialect) []map[string]any {
	counts := map[string]int{}
	for _, p := range practiceCatalog {
		if dialect != "" && !p.appliesTo(dialect) {
			continue
		}
		counts[p.Category]++
	}
	out := make([]map[string]any, 0, len(practiceCategoryOrder))
	for _, c := range practiceCategoryOrder {
		if counts[c] == 0 {
			continue
		}
		out = append(out, map[string]any{
			"id": c, "label": practiceCategoryLabels[c], "count": counts[c],
		})
	}
	return out
}

// collectRuleIDs 去重提取命中规则 ID，供 AI 侧交叉引用。
func collectRuleIDs(findings []Finding) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		if f.RuleID != "" && !seen[f.RuleID] {
			seen[f.RuleID] = true
			out = append(out, f.RuleID)
		}
	}
	sort.Strings(out)
	return out
}

// SuggestPractices 根据命中的规则与方言，推荐关联的最佳实践。
// 只返回与命中 ruleId 有关联的条目，避免给无关 SQL 塞满建议。
func SuggestPractices(findings []Finding, dialect Dialect) []Practice {
	if len(findings) == 0 {
		return []Practice{}
	}
	hit := make(map[string]bool, len(findings))
	for _, f := range findings {
		hit[f.RuleID] = true
	}
	seen := map[string]bool{}
	out := make([]Practice, 0, 8)
	for _, p := range practiceCatalog {
		if dialect != "" && !p.appliesTo(dialect) {
			continue
		}
		matched := false
		for _, rid := range p.RuleIDs {
			if hit[rid] {
				matched = true
				break
			}
		}
		if !matched || seen[p.ID] {
			continue
		}
		seen[p.ID] = true
		if p.CategoryLabel == "" {
			p.CategoryLabel = practiceCategoryLabels[p.Category]
		}
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return severityWeight[out[i].Severity] > severityWeight[out[j].Severity]
	})
	if len(out) > 8 {
		out = out[:8]
	}
	return out
}
