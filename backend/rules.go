package main

import (
	"sort"
	"strings"
)

// RuleMeta 是规则的静态定义。判定逻辑在 analyze.go，报告与 UI 只读这张表。
type RuleMeta struct {
	ID              string `json:"id"`
	Title           string `json:"title"`
	Category        string `json:"category"`
	CategoryLabel   string `json:"categoryLabel"`
	DefaultSeverity string `json:"defaultSeverity"`
	Help            string `json:"help"`
	Advice          string `json:"advice"`
	// Dialects 限定适用方言；nil = 所有方言都适用。
	Dialects []string `json:"dialects,omitempty"`
	// Optional 标记「项目约定类规则」：不是通用 SQL 质量问题，
	// 而是某些团队自定的房规（例如要求列必须有 COMMENT）。
	// 这类规则默认关闭，用户可在规则面板里显式开启。
	Optional bool `json:"optional,omitempty"`
}

// appliesTo 判断规则是否参与某方言的判定。
func (m RuleMeta) appliesTo(d Dialect) bool {
	if len(m.Dialects) == 0 {
		return true
	}
	for _, x := range m.Dialects {
		if strings.EqualFold(x, string(d)) {
			return true
		}
	}
	return false
}

const (
	catDestructive = "destructive"
	catMigration   = "migration"
	catPerformance = "performance"
	catSchema      = "schema"
	catSensitive   = "sensitive"
	catConsistency = "consistency"
)

var categoryLabels = map[string]string{
	catDestructive: "破坏性",
	catMigration:   "迁移规范",
	catPerformance: "性能",
	catSchema:      "结构质量",
	catSensitive:   "敏感数据",
	catConsistency: "一致性",
}

// ruleCatalog 是通用 SQL 评审规则集。
//
// 与内部 ASDM 版本的关键差别：
//  1. 规则 ID 去掉 ASDM 前缀，改用 SQL 前缀（对外稳定标识）；
//  2. 删掉所有需要连库才能判定的规则（大表行数、EXPLAIN 全表扫描、
//     flyway_schema_history 漂移比对）——通用插件不连数据库；
//  3. 团队房规类规则（幂等守卫、必须写 USE、列注释、显式引擎）
//     降级为 Optional，默认关闭，由用户按自己的仓库约定开启；
//  4. 保留全部「纯静态即可判定」的规则：破坏性、迁移规范、性能、结构、敏感数据。
var ruleCatalog = []RuleMeta{
	// ---- 破坏性
	{ID: "SQL001", Title: "删除表/库/视图", Category: catDestructive,
		Help:   "DROP TABLE / DATABASE / SCHEMA / VIEW 会不可逆地销毁对象与数据。",
		Advice: "迁移脚本里原则上不 DROP；确需删除先归档一个版本，确认无引用后再清理。", DefaultSeverity: "critical"},
	{ID: "SQL002", Title: "TRUNCATE 清表", Category: catDestructive,
		Help:   "TRUNCATE 不可回滚，且会重置自增列。",
		Advice: "改为 DELETE + WHERE 分批清理，或先备份到归档表。", DefaultSeverity: "high"},
	{ID: "SQL003", Title: "UPDATE/DELETE 缺少 WHERE", Category: catDestructive,
		Help:   "没有 WHERE 的 UPDATE/DELETE 会作用于全表。",
		Advice: "补 WHERE 主键/唯一键条件；批量修数走分批脚本并带 LIMIT。", DefaultSeverity: "critical"},
	{ID: "SQL004", Title: "WHERE 恒真", Category: catDestructive,
		Help:   "WHERE 1=1 / TRUE / 'a'='a' 等价于没有 WHERE，常见于拼接 SQL 的残留。",
		Advice: "删除恒真条件，写入真实过滤条件。", DefaultSeverity: "critical"},
	{ID: "SQL005", Title: "ALTER 删除列/索引/约束", Category: catDestructive,
		Help:   "DROP COLUMN / INDEX / FOREIGN KEY 会让依赖这些结构的代码在发布窗口内失效。",
		Advice: "先确认无代码引用（含历史版本回滚能力），必要时先改代码再删结构。", DefaultSeverity: "high"},
	{ID: "SQL006", Title: "权限或实例级变更", Category: catDestructive,
		Help:   "GRANT/REVOKE/CREATE USER/SET GLOBAL 超出应用迁移脚本的职责。",
		Advice: "权限变更走 DBA 工单，不进应用迁移目录。", DefaultSeverity: "high", Dialects: []string{"mysql"}},
	{ID: "SQL007", Title: "批量/覆盖写入", Category: catDestructive,
		Help:   "REPLACE INTO、LOAD DATA、INSERT…SELECT 会放大锁范围与 binlog 体积。",
		Advice: "改为 INSERT … ON DUPLICATE KEY UPDATE 并分批（每批 ≤ 1000 行）。", DefaultSeverity: "low"},
	{ID: "SQL008", Title: "动态 SQL（PREPARE）", Category: catDestructive,
		Help:   "PREPARE/EXECUTE 拼接的语句无法在执行前静态确认，插件只能分析字面量分支。",
		Advice: "至少保证分支覆盖幂等守卫，并在评审记录里注明拼接变量来源。", DefaultSeverity: "medium", Dialects: []string{"mysql"}},

	// ---- 迁移规范
	{ID: "SQL010", Title: "迁移文件命名不规范", Category: catMigration,
		Help:   "迁移文件建议采用 V<版本>__<描述>.sql（可重复脚本用 R__），否则版本管理工具无法识别。",
		Advice: "重命名为 V2.3.41__snake_case_desc.sql 形式。", DefaultSeverity: "high"},
	{ID: "SQL011", Title: "版本号重复", Category: catMigration,
		Help:   "同一目录内版本号重复会让版本管理工具直接失败。",
		Advice: "同一版本只能有一个脚本；新脚本版本取当前最大版本 + 1。", DefaultSeverity: "high"},
	{ID: "SQL012", Title: "非幂等 DDL", Category: catMigration,
		Help:            "裸 ALTER TABLE ADD COLUMN 在重放时会因列已存在而报错。",
		Advice:          "使用 IF NOT EXISTS，或先查询系统目录再条件执行，保证脚本可重复执行。",
		DefaultSeverity: "medium", Dialects: []string{"mysql"}, Optional: true},
	{ID: "SQL013", Title: "多库 USE / 跨库引用", Category: catMigration,
		Help:   "多库迁移时脚本开头的 USE 是正常写法；真正有问题的是脚本中途切到另一个库，或引用了声明目标库之外的对象。",
		Advice: "保持「开头一次 USE、脚本内不切换库」；跨库变更拆成独立脚本。", DefaultSeverity: "info", Dialects: []string{"mysql"}},
	{ID: "SQL014", Title: "单文件语句数过多", Category: catMigration,
		Help:   "单个迁移脚本语句过多会让失败定位与回滚成本变高。",
		Advice: "按变更主题拆分脚本，一个脚本只做一件事。", DefaultSeverity: "low"},
	{ID: "SQL015", Title: "事务与 DDL 混用", Category: catMigration,
		Help:   "MySQL DDL 隐式提交，显式事务包住 DDL 会造成「以为能回滚」的错觉。",
		Advice: "DDL 脚本内不写 START TRANSACTION/COMMIT；DML 迁移单独成脚本。", DefaultSeverity: "medium", Dialects: []string{"mysql"}},
	{ID: "SQL016", Title: "DELIMITER 不配对", Category: catMigration,
		Help:   "DELIMITER 切换后未切回 ; 会导致后续语句被当成一条。",
		Advice: "过程/触发器定义结束后补 DELIMITER ;。", DefaultSeverity: "medium", Dialects: []string{"mysql"}},
	{ID: "SQL017", Title: "编码/BOM/末尾换行", Category: catMigration,
		Help:   "BOM 或 CRLF 会影响校验和与部分客户端解析；文件末尾缺换行会让 diff 噪声增大。",
		Advice: "统一 UTF-8 无 BOM + LF。", DefaultSeverity: "info"},
	{ID: "SQL018", Title: "缺少库上下文声明（USE）", Category: catMigration,
		Help:            "脚本没有以 USE <目标库>; 开头时，裸表名全部落到连接/客户端的默认库；换库执行或在 IDE 里手跑时极易作用到错误的库。",
		Advice:          "首行（注释之后）加 `USE <目标库>;`，或对每个对象写全限定名 `<库>.<table>`。",
		DefaultSeverity: "high", Dialects: []string{"mysql"}, Optional: true},

	// ---- 性能
	{ID: "SQL020", Title: "ALTER 未声明算法", Category: catPerformance,
		Help:   "结构性 ALTER 未声明 ALGORITHM/LOCK 时，在大表上可能锁表或长时间拷贝。",
		Advice: "显式 ALGORITHM=INPLACE, LOCK=NONE（或 INSTANT），并在低峰执行。", DefaultSeverity: "high", Dialects: []string{"mysql"}},
	{ID: "SQL021", Title: "新增索引未声明在线方式", Category: catPerformance,
		Help:   "新建索引若未声明 LOCK=NONE，可能阻塞写入。",
		Advice: "加 ALGORITHM=INPLACE, LOCK=NONE；超大表用 gh-ost/pt-osc。", DefaultSeverity: "medium", Dialects: []string{"mysql"}},
	{ID: "SQL022", Title: "SELECT *", Category: catPerformance,
		Help:   "SELECT * 会随表结构变化返回多余列，破坏覆盖索引。",
		Advice: "显式列出需要的列。", DefaultSeverity: "low"},
	{ID: "SQL023", Title: "无谓词全表扫描", Category: catPerformance,
		Help:   "SELECT/UPDATE/DELETE 缺少 WHERE 或 JOIN 谓词，会扫全表。",
		Advice: "补过滤条件；确需全量读取时加 LIMIT 并说明用途。", DefaultSeverity: "medium"},
	{ID: "SQL024", Title: "前导通配 LIKE", Category: catPerformance,
		Help:   "LIKE '%x' 无法使用 B+Tree 索引，必然全表扫。",
		Advice: "改为前缀匹配 LIKE 'x%'，或引入倒排/全文索引。", DefaultSeverity: "medium"},
	{ID: "SQL025", Title: "索引列套函数", Category: catPerformance,
		Help:   "WHERE DATE(created_at)=… 这类写法会让索引失效。",
		Advice: "改写为范围条件：created_at >= ? AND created_at < ?。", DefaultSeverity: "medium"},
	{ID: "SQL026", Title: "OR 条件 / NOT IN 子查询", Category: catPerformance,
		Help:   "OR 与 NOT IN 常导致索引合并失败或物化子查询。",
		Advice: "拆成 UNION ALL 或改写为 EXISTS / LEFT JOIN … IS NULL。", DefaultSeverity: "low"},
	{ID: "SQL027", Title: "FROM 逗号隐式连接", Category: catPerformance,
		Help:   "FROM a, b 是隐式连接，连接条件只能写在 WHERE 里，漏写就会产生笛卡尔积。",
		Advice: "改用显式 JOIN … ON；确实要隐式连接时，确保 WHERE 覆盖所有表的连接条件。", DefaultSeverity: "medium"},

	// ---- 结构质量
	{ID: "SQL030", Title: "建表缺少主键", Category: catSchema,
		Help:   "无主键的表会退化为隐藏 rowid，复制与在线 DDL 都更脆弱。",
		Advice: "补自增主键或业务唯一键。", DefaultSeverity: "medium"},
	{ID: "SQL031", Title: "字符集不是 utf8mb4", Category: catSchema,
		Help:   "utf8/utf8mb3 无法存 emoji 与部分生僻字，且新旧表 JOIN 会隐式转换。",
		Advice: "统一 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci。", DefaultSeverity: "medium", Dialects: []string{"mysql"}},
	{ID: "SQL032", Title: "浮点类型存金额", Category: catSchema,
		Help:   "FLOAT/DOUBLE 存在二进制舍入误差，不能用于金额/配额。",
		Advice: "改用 DECIMAL(p,2) 或以最小货币单位的 BIGINT 存储。", DefaultSeverity: "medium"},
	{ID: "SQL033", Title: "缺少 COMMENT", Category: catSchema,
		Help:            "表与列缺少注释会让 ER 图与排障成本变高。",
		Advice:          "补 COMMENT，说明业务含义与取值范围。",
		DefaultSeverity: "info", Dialects: []string{"mysql"}, Optional: true},
	{ID: "SQL034", Title: "未显式指定引擎", Category: catSchema,
		Help:   "依赖实例默认引擎会在异构环境（dev/sit/prod）产生差异。",
		Advice: "显式 ENGINE=InnoDB。", DefaultSeverity: "low", Dialects: []string{"mysql"}, Optional: true},
	{ID: "SQL035", Title: "命名不符合 snake_case", Category: catSchema,
		Help:   "列名/表名混用大小写会因 lower_case_table_names 造成跨平台差异。",
		Advice: "统一小写下划线命名。", DefaultSeverity: "low"},
	{ID: "SQL036", Title: "可变长主键/大字段索引", Category: catSchema,
		Help:   "VARCHAR(255) 主键或对 TEXT 建索引会显著放大索引体积与写入成本。",
		Advice: "主键用 BIGINT；长字段索引用前缀索引并限制长度。", DefaultSeverity: "medium", Dialects: []string{"mysql"}},
	{ID: "SQL037", Title: "外键约束", Category: catSchema,
		Help:   "外键在多服务共库场景下会引入跨服务锁等待。",
		Advice: "确认必要性；如保留，注明级联行为。", DefaultSeverity: "low"},
	{ID: "SQL038", Title: "INSERT 未指定列清单", Category: catSchema,
		Help:   "INSERT INTO t VALUES (…) 依赖表的物理列顺序；目标表新增列或调整顺序后，数据会写错列甚至类型不匹配。",
		Advice: "显式写列清单：INSERT INTO t (col_a, col_b) VALUES (…)。", DefaultSeverity: "medium"},

	// ---- 敏感数据
	{ID: "SQL040", Title: "敏感列", Category: catSensitive,
		Help:   "列名命中 password/token/secret/salt/身份证/手机/邮箱/银行卡等敏感词。",
		Advice: "确认以哈希/加密存储，日志与导出做脱敏。", DefaultSeverity: "info"},

	// ---- SQLite 方言（只在 dialect=sqlite 时参与）
	{ID: "SQL060", Title: "SQLite 不支持的语句", Category: catMigration,
		Help:            "SQLite 的 ALTER TABLE 只支持 RENAME TABLE / RENAME COLUMN / ADD COLUMN / DROP COLUMN(3.35+)；MODIFY/CHANGE COLUMN、ADD CONSTRAINT、RENAME INDEX、TRUNCATE、GRANT、PREPARE、DELIMITER 会直接报语法错误。",
		Advice:          "改列类型/约束走「建新表 → INSERT…SELECT 搬数据 → DROP 旧表 → RENAME」的 12 步重建；权限与实例级操作不进应用迁移。",
		DefaultSeverity: "high", Dialects: []string{"sqlite"}},
	{ID: "SQL061", Title: "SQLite 脚本出现 USE", Category: catMigration,
		Help:            "SQLite 没有 USE 语句，执行到这一句直接语法错误；库由连接串里的文件路径决定。",
		Advice:          "删掉 USE，用库文件路径指定目标库。",
		DefaultSeverity: "high", Dialects: []string{"sqlite"}},
	{ID: "SQL062", Title: "不必要的 AUTOINCREMENT", Category: catSchema,
		Help:            "INTEGER PRIMARY KEY 本身就是 rowid 别名、自动递增；加 AUTOINCREMENT 会额外建 sqlite_sequence 表，并禁止复用已删除的 id。",
		Advice:          "除非业务明确要求「id 永不复用」，否则去掉 AUTOINCREMENT。",
		DefaultSeverity: "low", Dialects: []string{"sqlite"}},
	{ID: "SQL063", Title: "类型未按 SQLite 映射", Category: catSchema,
		Help:            "SQLite 只有类型亲和性（TEXT/INTEGER/REAL/BLOB/NUMERIC），JSON/DATETIME/VARCHAR(n)/TINYINT(1) 之类不产生任何约束，容易让人误以为有校验。",
		Advice:          "按映射规则翻译：JSON/DATETIME/CHAR(n)/VARCHAR(n) → TEXT，TINYINT(1)/BOOLEAN → INTEGER，金额 → INTEGER(分)。",
		DefaultSeverity: "info", Dialects: []string{"sqlite"}},
	{ID: "SQL064", Title: "事务内不可执行的语句", Category: catMigration,
		Help:            "VACUUM 不能在事务里执行，而多数迁移工具默认给每个迁移套事务，迁移会直接失败。",
		Advice:          "把 VACUUM 移到迁移之外，或用非事务脚本。",
		DefaultSeverity: "medium", Dialects: []string{"sqlite"}},
	{ID: "SQL065", Title: "表重建模式", Category: catMigration,
		Help:            "检测到「DROP 旧表 + RENAME 新表回原名」的 12 步重建写法；这类 DROP 不是真删表。",
		Advice:          "确认顺序为 建新表 → 搬数据 → DROP 旧表 → RENAME → 重建索引/触发器；被 DROP 的表在重建全程不可用，注意发布窗口。",
		DefaultSeverity: "info", Dialects: []string{"sqlite"}},
	{ID: "SQL066", Title: "ATTACH 第二个库文件", Category: catMigration,
		Help:            "ATTACH DATABASE 会引入第二个库文件，路径依赖运行目录，CI 与生产容易不一致。",
		Advice:          "确认绝对路径与两端文件存在；跨库读写建议拆成两个迁移通道。",
		DefaultSeverity: "low", Dialects: []string{"sqlite"}},

	// ---- SQL Server / T-SQL 方言（纯静态评审，只在 dialect=sqlserver 时参与）
	{ID: "SQL070", Title: "非幂等 DDL（SQL Server）", Category: catMigration,
		Help:            "裸 CREATE/ALTER/DROP 在迁移重放时可能因对象已存在或不存在而失败。",
		Advice:          "用 OBJECT_ID、COL_LENGTH、sys.objects/sys.columns 或 INFORMATION_SCHEMA 编写 IF 守卫；不要用 MySQL 的 PREPARE 写法。",
		DefaultSeverity: "medium", Dialects: []string{"sqlserver"}, Optional: true},
	{ID: "SQL071", Title: "NOLOCK / READ UNCOMMITTED", Category: catPerformance,
		Help:            "NOLOCK 与 READ UNCOMMITTED 会读取未提交数据，并可能出现重复行、漏行或页级不一致。",
		Advice:          "移除 NOLOCK；确实需要降低阻塞时评估 RCSI/快照隔离，并确认业务能接受的隔离语义。",
		DefaultSeverity: "medium", Dialects: []string{"sqlserver"}},
	{ID: "SQL072", Title: "TOP 缺少 ORDER BY", Category: catPerformance,
		Help:            "TOP 没有 ORDER BY 时返回哪些行不确定，分批修数时尤其容易漏行或重复。",
		Advice:          "为 TOP 补唯一且稳定的 ORDER BY（通常为主键）；分批处理时记录上一批边界。",
		DefaultSeverity: "low", Dialects: []string{"sqlserver"}},
	{ID: "SQL073", Title: "动态 SQL（EXEC）", Category: catDestructive,
		Help:            "EXEC/EXECUTE/sp_executesql 拼接的语句无法在执行前完整静态确认。",
		Advice:          "优先使用显式语句；必须动态执行时用 sp_executesql 参数化值，标识符用 QUOTENAME，并覆盖所有分支评审。",
		DefaultSeverity: "medium", Dialects: []string{"sqlserver"}},
	{ID: "SQL074", Title: "SELECT INTO 隐式建表写入", Category: catDestructive,
		Help:            "SELECT INTO 会按查询结果隐式建表并批量写入，目标已存在时直接失败，且可能产生长事务。",
		Advice:          "迁移中优先显式 CREATE TABLE + INSERT (列清单) SELECT，并分批；确认目标表不存在与锁/日志影响。",
		DefaultSeverity: "medium", Dialects: []string{"sqlserver"}},
	{ID: "SQL075", Title: "非 Unicode 字符类型", Category: catSchema,
		Help:            "VARCHAR/CHAR/TEXT 依赖排序规则与代码页，跨环境存取中文或 emoji 可能出现转换问题。",
		Advice:          "需要多语言文本时使用 NVARCHAR/NCHAR；新表避免已不推荐使用的 TEXT/NTEXT，改用 (N)VARCHAR(MAX)。",
		DefaultSeverity: "info", Dialects: []string{"sqlserver"}},
	{ID: "SQL076", Title: "MAX 类型用于主键/索引", Category: catSchema,
		Help:            "(N)VARCHAR(MAX)/VARBINARY(MAX) 不能作为 SQL Server 索引键，主键或唯一约束会创建失败。",
		Advice:          "键列改为有明确上限且满足索引键字节限制的类型；大文本仅作为包含列或单独存储。",
		DefaultSeverity: "high", Dialects: []string{"sqlserver"}},
	{ID: "SQL077", Title: "MERGE 缺少 HOLDLOCK", Category: catDestructive,
		Help:            "并发下 MERGE 的匹配与插入之间可能出现竞态，造成唯一键冲突或重复数据。",
		Advice:          "评估改用显式 UPDATE/INSERT；保留 MERGE 时使用 WITH (HOLDLOCK) 并验证唯一约束与并发行为。",
		DefaultSeverity: "medium", Dialects: []string{"sqlserver"}},

	// ---- 一致性（不连库也能判定的部分）
	{ID: "SQL050", Title: "目录内版本重复", Category: catConsistency,
		Help:   "同一版本号出现在多个脚本中，版本管理工具会直接失败。",
		Advice: "合并或重命名脚本，保证版本号唯一。", DefaultSeverity: "high"},
	{ID: "SQL051", Title: "目录内版本号不连续", Category: catConsistency,
		Help:   "版本号之间存在缺口，通常是脚本遗漏或被误删。",
		Advice: "确认缺口是刻意跳过还是漏提交；漏提交会补一条补丁脚本。", DefaultSeverity: "info"},
	{ID: "SQL052", Title: "迁移文件校验和重复", Category: catConsistency,
		Help:   "两个不同版本的文件内容完全相同，通常是复制粘贴后忘了改。",
		Advice: "确认第二个脚本确实是独立变更；否则合并或删除。", DefaultSeverity: "medium"},
}

var ruleIndex = func() map[string]RuleMeta {
	m := make(map[string]RuleMeta, len(ruleCatalog))
	for _, r := range ruleCatalog {
		if r.DefaultSeverity == "" {
			r.DefaultSeverity = "medium"
		}
		r.CategoryLabel = categoryLabels[r.Category]
		m[r.ID] = r
	}
	return m
}()

// Rules 返回按类别排序的规则清单（含生效级别）。
func Rules(cfg RuleConfig) []RuleMeta {
	out := make([]RuleMeta, 0, len(ruleCatalog))
	for _, r := range ruleCatalog {
		if r.DefaultSeverity == "" {
			r.DefaultSeverity = "medium"
		}
		r.CategoryLabel = categoryLabels[r.Category]
		if s, ok := cfg.Severity[r.ID]; ok && s != "" {
			r.DefaultSeverity = s
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Category != out[j].Category {
			return categoryOrder(out[i].Category) < categoryOrder(out[j].Category)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// defaultDisabled 返回「默认关闭的规则」列表：全部 Optional 规则。
//
// Optional 是项目约定类规则，对通用项目默认不适用；
// 用户在规则面板里显式开启后即生效。
func defaultDisabled() []string {
	var out []string
	for _, r := range ruleCatalog {
		if r.Optional {
			out = append(out, r.ID)
		}
	}
	sort.Strings(out)
	return out
}

func categoryOrder(c string) int {
	switch c {
	case catDestructive:
		return 1
	case catMigration:
		return 2
	case catPerformance:
		return 3
	case catSchema:
		return 4
	case catSensitive:
		return 5
	case catConsistency:
		return 6
	default:
		return 7
	}
}

var severityWeight = map[string]int{
	"critical": 40,
	"high":     20,
	"medium":   8,
	"low":      3,
	"info":     0,
}

var severityRank = map[string]int{
	"critical": 5,
	"high":     4,
	"medium":   3,
	"low":      2,
	"info":     1,
}
