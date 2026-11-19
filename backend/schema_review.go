package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// 架构评审规则引擎（ARCH001–ARCH032）。
//
// 与 SQL0xx 文件级规则完全隔离：架构分是独立评分（设计 §5），
// 不计入既有六维质量分，保证历史报告等级不漂移。
// 全程静态分析：不连库、不执行 SQL、不发网络请求。

// SchemaRuleMeta 是一条架构规则的静态定义。
type SchemaRuleMeta struct {
	ID              string
	Title           string
	Category        string
	DefaultSeverity string
	Advice          string
	PracticeIDs     []string
}

// schemaRuleCatalog 与设计 §3 的规则目录一一对应。
var schemaRuleCatalog = []SchemaRuleMeta{
	// ---- 表设计（T）
	{"ARCH001", "表缺少主键", "schema", "high", "补自增主键或业务唯一键。", []string{"BP-MY-001", "BP-LT-001"}},
	{"ARCH002", "主键非整型/非递增", "schema", "medium", "主键优先使用有序整型（BIGINT/IDENTITY/rowid），避免随机 UUID 主键放大索引分裂。", []string{"BP-MY-001", "BP-PG-004", "BP-MS-001"}},
	{"ARCH003", "跨表主键类型不一致", "schema", "medium", "统一关联键类型，避免 JOIN 隐式转换导致索引失效。", []string{"BP-GEN-001"}},
	{"ARCH004", "表/列缺少 COMMENT", "schema", "info", "补 COMMENT，说明业务含义与取值范围。", nil},
	{"ARCH005", "字符集/排序规则不统一", "schema", "medium", "统一字符集与排序规则（如 utf8mb4 / utf8mb4_unicode_ci），避免跨表 JOIN 隐式转换。", []string{"BP-MY-006"}},
	{"ARCH006", "引擎未声明 / 混用", "schema", "medium", "显式声明引擎（MySQL 统一 InnoDB），避免实例默认值在异构环境产生差异。", []string{"BP-MY-007"}},
	{"ARCH007", "宽表（列数超阈值）", "schema", "low", "按领域拆分宽表，高频列与大字段分离，降低单行宽度与回表成本。", nil},
	{"ARCH008", "表名命名不规范", "schema", "low", "统一小写下划线（snake_case）命名，避免大小写混用造成跨平台差异。", nil},
	{"ARCH009", "标识符过长 / 使用保留字", "schema", "medium", "表/列名控制长度并避开方言保留字，必要时加业务前缀。", nil},

	// ---- 列与类型（C）
	{"ARCH010", "同名逻辑列跨表类型不一致", "schema", "high", "统一关联键/逻辑列类型；先确认是否业务故意，否则按 BIGINT 等一致类型收敛。", []string{"BP-GEN-001"}},
	{"ARCH011", "金额使用浮点类型", "schema", "high", "金额/配额改用 DECIMAL(p,2) 或以最小货币单位存储的 BIGINT。", []string{"BP-GEN-012"}},
	{"ARCH012", "大字段/TEXT 出现在高频查询列", "schema", "low", "TEXT/BLOB/JSON 等大字段拆到扩展表，避免拖慢主表查询与放大回表。", nil},
	{"ARCH013", "可空列无默认值", "schema", "info", "为可空列补显式默认值，避免历史数据与新写入语义不一致。", []string{"BP-GEN-015"}},
	{"ARCH014", "PG 时间类型未统一 TIMESTAMPTZ", "schema", "low", "需要绝对时间点时使用 TIMESTAMPTZ；只有明确本地时刻才用无时区类型。", []string{"BP-PG-006"}},
	{"ARCH015", "PG 使用 JSON 而非 JSONB", "schema", "medium", "除必须逐字保留输入外，统一使用 JSONB，并为高频查询键建表达式 GIN 索引。", []string{"BP-PG-005"}},

	// ---- 索引设计（I）
	{"ARCH016", "同表索引名重复", "index", "high", "同一表内索引名保持唯一，避免重放与维护时混淆。", nil},
	{"ARCH017", "冗余索引（前缀重叠）", "index", "medium", "删除被更长前缀索引覆盖的冗余单列索引，降低写入与存储成本。", []string{"BP-GEN-001"}},
	{"ARCH018", "单列低区分度索引", "index", "low", "状态/性别/类型等低区分度列只作为复合索引后缀，不建单列索引。", []string{"BP-GEN-003"}},
	{"ARCH019", "单表索引过多", "index", "medium", "控制单表索引数量（≤6），合并重叠索引，降低写入放大。", []string{"BP-MY-003"}},
	{"ARCH020", "外键/引用列无索引", "index", "high", "为外键/关联列补索引，避免级联更新与 JOIN 时全表扫描。", []string{"BP-GEN-001"}},
	{"ARCH021", "表达式索引的列被函数包裹", "index", "medium", "避免在索引列上套函数；需要函数过滤时改用表达式索引。", []string{"BP-GEN-004"}},
	{"ARCH022", "复合索引顺序可疑", "index", "low", "等值列放左边、范围列放右边，遵循最左前缀。", []string{"BP-GEN-001"}},
	{"ARCH023", "谓词覆盖缺口", "index", "medium", "为高频 WHERE/JOIN 过滤列评估补索引；先确认真实执行计划，避免盲目加索引。", []string{"BP-GEN-002"}},
	{"ARCH024", "索引命名不规范", "index", "info", "索引名遵循 idx_<table>_<cols> / uk_<table>_<cols> 约定。", nil},

	// ---- 关系完整性（R）
	{"ARCH025", "外键目标表不在批次内", "relation", "medium", "确认目标表是否漏传脚本；跨库/历史表需在评审记录里注明。", nil},
	{"ARCH026", "隐式引用（xxx_id）无 FK 也无索引", "relation", "low", "命名推断的关联列建议补索引；是否落外键按多服务共库约定决定。", nil},
	{"ARCH027", "循环外键", "relation", "high", "解除表间循环依赖，改为应用层校验或中间关联表。", nil},
	{"ARCH028", "外键 ON DELETE CASCADE 风险", "relation", "medium", "确认级联删除的影响范围；高风险表改为应用层显式删除并留痕。", []string{"BP-PG-010"}},

	// ---- 迁移与演进安全（M）
	{"ARCH029", "新增 NOT NULL 列无默认值", "migration", "high", "先补默认值或回填存量数据，再加 NOT NULL 约束，避免迁移失败。", []string{"BP-PG-003"}},
	{"ARCH030", "建索引未用在线方式（汇总）", "migration", "medium", "新建索引声明在线方式（MySQL ALGORITHM/LOCK，PG CONCURRENTLY），超大表用 gh-ost/pt-osc。", []string{"BP-MY-007", "BP-PG-001"}},
	{"ARCH031", "单文件跨多表结构变更", "migration", "low", "按表/主题拆分迁移脚本，提高回滚粒度与失败定位效率。", nil},
	{"ARCH032", "表/索引重复定义", "schema", "high", "同一批次内重复 CREATE 同名表需加 IF NOT EXISTS 或合并为单条定义。", nil},
}

var schemaRuleIndex = func() map[string]SchemaRuleMeta {
	m := make(map[string]SchemaRuleMeta, len(schemaRuleCatalog))
	for _, r := range schemaRuleCatalog {
		m[r.ID] = r
	}
	return m
}()

// schemaRulesMeta 把 ARCH 规则目录转成规则面板可渲染的元数据。
// 分类统一为 arch（前端显示「架构评审」），启停与级别复用 RuleConfig.Disabled/Severity。
func schemaRulesMeta(cfg RuleConfig) []RuleMeta {
	out := make([]RuleMeta, 0, len(schemaRuleCatalog))
	for _, r := range schemaRuleCatalog {
		sev := r.DefaultSeverity
		if s, ok := cfg.Severity[r.ID]; ok && s != "" {
			sev = s
		}
		out = append(out, RuleMeta{
			ID: r.ID, Title: r.Title, Category: "arch", CategoryLabel: "架构评审",
			DefaultSeverity: sev, Help: r.Advice,
		})
	}
	return out
}

// SchemaSuggest 是按表聚合的优化建议卡。
type SchemaSuggest struct {
	Table       string   `json:"table"`
	RuleIDs     []string `json:"ruleIds"`
	Title       string   `json:"title"`
	Message     string   `json:"message"`
	Advice      string   `json:"advice"`
	PracticeIDs []string `json:"practiceIds,omitempty"`
	GoodSQL     string   `json:"goodSql,omitempty"`
	BadSQL      string   `json:"badSql,omitempty"`
	Files       []string `json:"files,omitempty"`
}

// ArchitectureStats 是架构报告的客观规模统计。
type ArchitectureStats struct {
	Tables      int `json:"tables"`
	Columns     int `json:"columns"`
	Indexes     int `json:"indexes"`
	References  int `json:"references"`
	Findings    int `json:"findings"`
	ParseErrors int `json:"parseErrors"`
}

// ArchitectureReport 是架构评审的完整结果（独立评分，不并入六维质量分）。
type ArchitectureReport struct {
	Score       int               `json:"score"`
	Grade       string            `json:"grade"`
	GradeLabel  string            `json:"gradeLabel"`
	Findings    []SchemaFinding   `json:"findings"`
	Tables      []TableDef        `json:"tables"`
	Indexes     []IndexDef        `json:"indexes"`
	Suggestions []SchemaSuggest   `json:"suggestions"`
	Stats       ArchitectureStats `json:"stats"`
}

// schemaReviewConfig 是架构评审的可调开关与阈值。
type schemaReviewConfig struct {
	Enabled       bool
	WideTableCols int
	Disabled      map[string]bool
	Severity      map[string]string
}

func loadSchemaConfig(cfg RuleConfig, enabled bool) schemaReviewConfig {
	out := schemaReviewConfig{Enabled: enabled, WideTableCols: 50, Disabled: map[string]bool{}, Severity: cfg.Severity}
	for _, d := range cfg.Disabled {
		out.Disabled[strings.ToUpper(strings.TrimSpace(d))] = true
	}
	return out
}

// RunSchemaReview 在 VDD 上跑全部架构规则，产出独立评分的报告。
// enabled 来自配置 schema.enabled：批量链路跟随开关；schema/review 与 MCP
// sql_schema_review 属显式调用，恒为 true。
func RunSchemaReview(model *SchemaModel, cfg RuleConfig, enabled bool) *ArchitectureReport {
	sc := loadSchemaConfig(cfg, enabled)
	eng := &schemaEngine{model: model, cfg: sc, findings: []SchemaFinding{}}

	if sc.Enabled {
		eng.runRules()
	}
	eng.dedupeAndCap(50)
	sortFindings(eng.findings)

	report := &ArchitectureReport{Findings: eng.findings}
	report.Score, report.Grade, report.GradeLabel = scoreSchema(eng.findings)
	report.Tables = modelTableList(model)
	report.Indexes = model.Indexes
	report.Suggestions = eng.buildSuggestions()
	report.Stats = ArchitectureStats{
		Tables:      len(model.Tables),
		Indexes:     len(model.Indexes),
		References:  len(model.Refs),
		Findings:    len(eng.findings),
		ParseErrors: len(model.ParseErrors),
	}
	for _, t := range model.Tables {
		report.Stats.Columns += len(t.Columns)
	}
	return report
}

type schemaEngine struct {
	model    *SchemaModel
	cfg      schemaReviewConfig
	findings []SchemaFinding
}

func (e *schemaEngine) add(ruleID, msg, advice string, tables, files []string, ev map[string]any) {
	if e.cfg.Disabled[strings.ToUpper(ruleID)] {
		return
	}
	meta, ok := schemaRuleIndex[ruleID]
	if !ok {
		return
	}
	sev := meta.DefaultSeverity
	if s, ok := e.cfg.Severity[ruleID]; ok && s != "" {
		sev = s
	}
	f := SchemaFinding{
		RuleID: ruleID, Severity: sev, Category: meta.Category, Title: meta.Title,
		Message: msg, Advice: advice, Tables: tables, Files: files,
		Evidence: ev, PracticeIDs: meta.PracticeIDs,
	}
	e.findings = append(e.findings, f)
}

func (e *schemaEngine) runRules() {
	e.checkTables()
	e.checkColumns()
	e.checkIndexes()
	e.checkRelations()
	e.checkMigration()
}

// ---------------- 表设计 ARCH001–009

func (e *schemaEngine) checkTables() {
	wide := e.cfg.WideTableCols
	if wide <= 0 {
		wide = 50
	}
	for name, t := range e.model.Tables {
		files := []string{t.SourceFile}

		if len(t.PrimaryKey) == 0 {
			e.add("ARCH001", fmt.Sprintf("表 %s 未定义主键", t.QualifiedName),
				schemaRuleIndex["ARCH001"].Advice, []string{name}, files,
				map[string]any{"table": name})
		} else {
			e.checkPKType(name, t, files)
		}

		if t.Comment == "" && !hasColumnComment(t) {
			e.add("ARCH004", fmt.Sprintf("表 %s 与列缺少 COMMENT", t.QualifiedName),
				schemaRuleIndex["ARCH004"].Advice, []string{name}, files,
				map[string]any{"table": name})
		}

		if e.model.Dialect == DialectMySQL {
			if t.Charset != "" && t.Charset != "utf8mb4" {
				e.add("ARCH005", fmt.Sprintf("表 %s 字符集为 %s，与 utf8mb4 不一致", t.QualifiedName, t.Charset),
					schemaRuleIndex["ARCH005"].Advice, []string{name}, files,
					map[string]any{"table": name, "charset": t.Charset})
			}
			if t.Engine != "" && t.Engine != "innodb" {
				e.add("ARCH006", fmt.Sprintf("表 %s 使用引擎 %s（建议统一 InnoDB）", t.QualifiedName, t.Engine),
					schemaRuleIndex["ARCH006"].Advice, []string{name}, files,
					map[string]any{"table": name, "engine": t.Engine})
			}
		}

		if len(t.Columns) > wide {
			e.add("ARCH007", fmt.Sprintf("宽表 %s：%d 列（阈值 %d）", t.QualifiedName, len(t.Columns), wide),
				schemaRuleIndex["ARCH007"].Advice, []string{name}, files,
				map[string]any{"table": name, "columns": len(t.Columns), "threshold": wide})
		}

		if !snakeCase(name) {
			e.add("ARCH008", fmt.Sprintf("表名 %s 不符合 snake_case", t.QualifiedName),
				schemaRuleIndex["ARCH008"].Advice, []string{name}, files,
				map[string]any{"table": name})
		}
		if isReservedWord(name) {
			e.add("ARCH009", fmt.Sprintf("表名 %s 命中方言保留字", t.QualifiedName),
				schemaRuleIndex["ARCH009"].Advice, []string{name}, files,
				map[string]any{"table": name})
		}
	}

	// ARCH005：跨表字符集不一致（MySQL）
	if e.model.Dialect == DialectMySQL {
		charsetSet := map[string]string{}
		for name, t := range e.model.Tables {
			if t.Charset != "" {
				charsetSet[t.Charset] = name
			}
		}
		if len(charsetSet) > 1 {
			for cs, tn := range charsetSet {
				e.add("ARCH005", fmt.Sprintf("批次内字符集混用：%s 出现在 %s 等表", cs, tn),
					schemaRuleIndex["ARCH005"].Advice, nil, nil,
					map[string]any{"charsets": keysOf(charsetSet)})
				break
			}
		}
		// ARCH006：引擎混用
		engineSet := map[string]string{}
		for name, t := range e.model.Tables {
			if t.Engine != "" {
				engineSet[t.Engine] = name
			}
		}
		if len(engineSet) > 1 {
			for eng, tn := range engineSet {
				e.add("ARCH006", fmt.Sprintf("批次内引擎混用：%s 出现在 %s 等表", eng, tn),
					schemaRuleIndex["ARCH006"].Advice, nil, nil,
					map[string]any{"engines": keysOf(engineSet)})
				break
			}
		}
	}

	// ARCH003：跨表主键类型不一致（同主键列名，类型不同）
	pkByCol := map[string][]pkUsage{}
	for name, t := range e.model.Tables {
		for _, pk := range t.PrimaryKey {
			if c := t.Column(pk); c != nil {
				pkByCol[strings.ToLower(pk)] = append(pkByCol[strings.ToLower(pk)], pkUsage{table: name, typ: columnTypeNormalize(c.Type), raw: c.Type, file: t.SourceFile})
			}
		}
	}
	for col, uses := range pkByCol {
		if len(uses) < 2 {
			continue
		}
		types := map[string]string{}
		for _, u := range uses {
			types[u.typ] = u.raw
		}
		if len(types) > 1 {
			var files []string
			for _, u := range uses {
				files = append(files, u.file)
			}
			e.add("ARCH003", fmt.Sprintf("主键列 %s 跨表类型不一致：%s", col, describeUsages(uses)),
				schemaRuleIndex["ARCH003"].Advice, nil, files,
				map[string]any{"column": col, "usages": usageMap(uses)})
		}
	}
}

type pkUsage struct {
	table string
	typ   string
	raw   string
	file  string
}

func describeUsages(uses []pkUsage) string {
	parts := make([]string, 0, len(uses))
	for _, u := range uses {
		parts = append(parts, fmt.Sprintf("%s.%s", u.table, u.raw))
	}
	return strings.Join(parts, "，")
}

func usageMap(uses []pkUsage) []map[string]string {
	out := make([]map[string]string, 0, len(uses))
	for _, u := range uses {
		out = append(out, map[string]string{"table": u.table, "type": u.raw, "file": u.file})
	}
	return out
}

func (e *schemaEngine) checkPKType(name string, t *TableDef, files []string) {
	for _, pk := range t.PrimaryKey {
		c := t.Column(pk)
		if c == nil {
			continue
		}
		typ := strings.ToLower(strings.ReplaceAll(c.Type, " ", ""))
		ok := false
		switch e.model.Dialect {
		case DialectMySQL:
			ok = strings.HasPrefix(typ, "bigint") || strings.HasPrefix(typ, "int") ||
				strings.HasPrefix(typ, "smallint") || strings.HasPrefix(typ, "tinyint") ||
				strings.HasPrefix(typ, "mediumint")
		case DialectPostgreSQL:
			ok = strings.HasPrefix(typ, "bigint") || strings.HasPrefix(typ, "int") ||
				c.Identity || strings.Contains(strings.ToLower(c.Default), "sequence")
		case DialectSQLServer:
			ok = strings.HasPrefix(typ, "bigint") || strings.HasPrefix(typ, "int") ||
				strings.HasPrefix(typ, "smallint")
		case DialectSQLite:
			ok = strings.HasPrefix(typ, "integer") || strings.HasPrefix(typ, "int")
		}
		if !ok {
			e.add("ARCH002", fmt.Sprintf("表 %s 主键 %s 类型为 %s（非有序整型）", t.QualifiedName, c.Name, c.Type),
				schemaRuleIndex["ARCH002"].Advice, []string{name}, files,
				map[string]any{"table": name, "column": c.Name, "type": c.Type})
		}
	}
}

func hasColumnComment(t *TableDef) bool {
	for _, c := range t.Columns {
		if c.Comment != "" {
			return true
		}
	}
	return false
}

// pkMember 判断列是否属于表主键（大小写不敏感）。
func pkMember(t *TableDef, col string) bool {
	for _, pk := range t.PrimaryKey {
		if strings.EqualFold(pk, col) {
			return true
		}
	}
	return false
}

// ---------------- 列与类型 ARCH010–015

func (e *schemaEngine) checkColumns() {
	// 同名列跨表类型索引：col -> []colUsage
	byName := map[string][]colUsage{}
	for name, t := range e.model.Tables {
		for _, c := range t.Columns {
			byName[strings.ToLower(c.Name)] = append(byName[strings.ToLower(c.Name)],
				colUsage{table: name, col: c, file: t.SourceFile, line: c.Line})
		}
	}

	for col, uses := range byName {
		if len(uses) < 2 {
			continue
		}
		// ARCH010：逻辑关联列（xxx_id / 主键名）跨表类型不一致
		rel := strings.HasSuffix(col, "_id") || col == "id"
		types := map[string][]colUsage{}
		for _, u := range uses {
			types[columnTypeNormalize(u.col.Type)] = append(types[columnTypeNormalize(u.col.Type)], u)
		}
		if len(types) > 1 && rel {
			var files []string
			var tables []string
			for _, u := range uses {
				files = append(files, u.file)
				tables = append(tables, u.table)
			}
			e.add("ARCH010", fmt.Sprintf("关联列 %s 跨表类型不一致：%s", col, describeColUsages(uses)),
				schemaRuleIndex["ARCH010"].Advice, tables, files,
				map[string]any{"column": col, "usages": colUsageMap(uses)})
		}
	}

	for name, t := range e.model.Tables {
		for _, c := range t.Columns {
			typ := strings.ToUpper(strings.ReplaceAll(c.Type, " ", ""))
			files := []string{t.SourceFile}

			if isMoneyColumn(c.Name) && (strings.HasPrefix(typ, "FLOAT") || strings.HasPrefix(typ, "DOUBLE") || strings.HasPrefix(typ, "REAL")) {
				e.add("ARCH011", fmt.Sprintf("金额列 %s.%s 使用浮点类型 %s", t.QualifiedName, c.Name, c.Type),
					schemaRuleIndex["ARCH011"].Advice, []string{name}, files,
					map[string]any{"table": name, "column": c.Name, "type": c.Type})
			}

			if isLargeType(typ) {
				e.add("ARCH012", fmt.Sprintf("大字段 %s.%s 类型 %s 出现在主表", t.QualifiedName, c.Name, c.Type),
					schemaRuleIndex["ARCH012"].Advice, []string{name}, files,
					map[string]any{"table": name, "column": c.Name, "type": c.Type})
			}

			if c.Nullable && c.Default == "" {
				e.add("ARCH013", fmt.Sprintf("列 %s.%s 可空且无默认值", t.QualifiedName, c.Name),
					schemaRuleIndex["ARCH013"].Advice, []string{name}, files,
					map[string]any{"table": name, "column": c.Name})
			}

			if e.model.Dialect == DialectPostgreSQL {
				colType := strings.ToLower(c.Type)
				if strings.Contains(colType, "timestamp") &&
					!strings.Contains(colType, "timestamptz") &&
					!strings.Contains(colType, "with time zone") {
					e.add("ARCH014", fmt.Sprintf("PG 时间列 %s.%s 使用无时区类型 %s", t.QualifiedName, c.Name, c.Type),
						schemaRuleIndex["ARCH014"].Advice, []string{name}, files,
						map[string]any{"table": name, "column": c.Name, "type": c.Type})
				}
				if strings.EqualFold(columnBase(c.Type), "json") {
					e.add("ARCH015", fmt.Sprintf("PG 列 %s.%s 使用 JSON（建议 JSONB）", t.QualifiedName, c.Name),
						schemaRuleIndex["ARCH015"].Advice, []string{name}, files,
						map[string]any{"table": name, "column": c.Name})
				}
			}
		}
	}
}

type colUsage struct {
	table string
	col   ColumnDef
	file  string
	line  int
}

func describeColUsages(uses []colUsage) string {
	parts := make([]string, 0, len(uses))
	for _, u := range uses {
		parts = append(parts, fmt.Sprintf("%s.%s %s", u.table, u.col.Name, u.col.Type))
	}
	return strings.Join(parts, "，")
}

func colUsageMap(uses []colUsage) []map[string]string {
	out := make([]map[string]string, 0, len(uses))
	for _, u := range uses {
		out = append(out, map[string]string{"table": u.table, "column": u.col.Name, "type": u.col.Type, "file": u.file})
	}
	return out
}

func isMoneyColumn(name string) bool {
	n := strings.ToLower(name)
	for _, k := range []string{"amount", "price", "fee", "balance", "salary", "money", "cost", "quota", "total"} {
		if strings.Contains(n, k) {
			return true
		}
	}
	return false
}

func isLargeType(typ string) bool {
	for _, k := range []string{"TEXT", "BLOB", "CLOB", "JSON", "VARCHAR(MAX)", "NVARCHAR(MAX)"} {
		if strings.Contains(typ, k) {
			return true
		}
	}
	return false
}

func columnBase(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if i := strings.Index(s, "("); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// ---------------- 索引 ARCH016–024

func (e *schemaEngine) checkIndexes() {
	// ARCH016：同表索引名重复（MySQL 索引名本就允许跨表同名，按表判唯一）
	nameCount := map[string][]IndexDef{}
	for _, idx := range e.model.Indexes {
		if idx.Name != "" {
			k := strings.ToLower(idx.Table) + "\x00" + strings.ToLower(idx.Name)
			nameCount[k] = append(nameCount[k], idx)
		}
	}
	for _, group := range nameCount {
		if len(group) > 1 {
			var files []string
			for _, g := range group {
				files = append(files, g.SourceFile)
			}
			e.add("ARCH016", fmt.Sprintf("表 %s 索引名 %s 重复 %d 次", group[0].Table, group[0].Name, len(group)),
				schemaRuleIndex["ARCH016"].Advice, []string{group[0].Table}, files,
				map[string]any{"table": group[0].Table, "index": group[0].Name, "count": len(group)})
		}
	}

	byTable := map[string][]IndexDef{}
	for _, idx := range e.model.Indexes {
		byTable[idx.Table] = append(byTable[idx.Table], idx)
	}

	for table, idxs := range byTable {
		// ARCH017：前缀重叠冗余索引
		for i, a := range idxs {
			for j, b := range idxs {
				if i == j || len(a.Columns) == 0 || len(b.Columns) == 0 {
					continue
				}
				if prefixOf(a.Columns, b.Columns) && !a.Unique {
					e.add("ARCH017", fmt.Sprintf("索引 %s(%s) 被 %s(%s) 前缀覆盖，存在冗余",
						a.Name, strings.Join(a.Columns, ","), b.Name, strings.Join(b.Columns, ",")),
						schemaRuleIndex["ARCH017"].Advice, []string{table}, []string{a.SourceFile, b.SourceFile},
						map[string]any{"table": table, "redundant": a.Name, "covering": b.Name})
				}
			}
		}

		// ARCH018：单列低区分度
		for _, idx := range idxs {
			if len(idx.Columns) == 1 && lowSelectivityCol(idx.Columns[0]) {
				e.add("ARCH018", fmt.Sprintf("单列低区分度索引 %s(%s)", idx.Name, idx.Columns[0]),
					schemaRuleIndex["ARCH018"].Advice, []string{table}, []string{idx.SourceFile},
					map[string]any{"table": table, "index": idx.Name, "column": idx.Columns[0]})
			}
		}

		// ARCH019：索引过多
		if len(idxs) > 6 {
			e.add("ARCH019", fmt.Sprintf("表 %s 索引 %d 个（阈值 6）", table, len(idxs)),
				schemaRuleIndex["ARCH019"].Advice, []string{table}, nil,
				map[string]any{"table": table, "indexes": len(idxs)})
		}

		// ARCH024：索引命名
		for _, idx := range idxs {
			if idx.Name != "" && !indexNameConvention(idx.Name, table) {
				e.add("ARCH024", fmt.Sprintf("索引名 %s 不符合 idx_<table>_<cols> 约定", idx.Name),
					schemaRuleIndex["ARCH024"].Advice, []string{table}, []string{idx.SourceFile},
					map[string]any{"table": table, "index": idx.Name})
			}
		}
	}

	// ARCH020：外键/引用列无索引（多列外键按「全部列被同一索引覆盖」近似 MySQL
	// 最左前缀要求）
	for _, ref := range e.model.Refs {
		if ref.Kind != "FK" || ref.FromColumns == "" {
			continue
		}
		if !fkIndexCovered(e.model.Indexes, ref.FromTable, ref.FromColumns) {
			e.add("ARCH020", fmt.Sprintf("外键列 %s.%s 缺少索引", ref.FromTable, ref.FromColumns),
				schemaRuleIndex["ARCH020"].Advice, []string{ref.FromTable}, []string{ref.SourceFile},
				map[string]any{"table": ref.FromTable, "column": ref.FromColumns})
		}
	}

	// ARCH023：谓词覆盖缺口
	e.checkPredicateCoverage()
}

func prefixOf(a, b []string) bool {
	if len(a) >= len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i], b[i]) {
			return false
		}
	}
	return true
}

func lowSelectivityCol(col string) bool {
	n := strings.ToLower(col)
	for _, k := range []string{"status", "state", "type", "gender", "sex", "level", "is_", "enabled", "flag", "category"} {
		if strings.Contains(n, k) {
			return true
		}
	}
	return false
}

func indexCovers(idxs []IndexDef, table, col string) bool {
	for _, idx := range idxs {
		if !strings.EqualFold(idx.Table, table) {
			continue
		}
		for _, c := range idx.Columns {
			if strings.EqualFold(c, col) {
				return true
			}
		}
	}
	return false
}

// fkIndexCovered 判断外键的每一列（逗号分隔清单）是否被同一索引的列清单覆盖。
func fkIndexCovered(idxs []IndexDef, table, colsCSV string) bool {
	for _, idx := range idxs {
		if !strings.EqualFold(idx.Table, table) {
			continue
		}
		ok := true
		for _, c := range strings.Split(colsCSV, ",") {
			if !indexCovers([]IndexDef{idx}, table, strings.TrimSpace(c)) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func indexNameConvention(name, table string) bool {
	if strings.HasPrefix(name, "idx_") || strings.HasPrefix(name, "uk_") || strings.HasPrefix(name, "uq_") {
		return true
	}
	// 保留系统生成名等宽松场景：只对明显反例（纯中文/过长）提示，这里默认通过。
	return true
}

func (e *schemaEngine) checkPredicateCoverage() {
	if len(e.model.Predicates) == 0 {
		return
	}
	// 汇总每张表上被过滤/连接的列频次。
	colFreq := map[string]map[string]int{} // table -> col -> count
	for _, p := range e.model.Predicates {
		if p.Table == "" {
			continue // 动态/无法确定表名的引用跳过，避免误报
		}
		// 别名限定（FROM orders o … WHERE o.status）会把别名当表名收集进来；
		// 别名不在 VDD 表集合里，直接跳过，避免对不存在的表报 ARCH023。
		if _, ok := e.model.Tables[p.Table]; !ok {
			continue
		}
		if colFreq[p.Table] == nil {
			colFreq[p.Table] = map[string]int{}
		}
		colFreq[p.Table][p.Column]++
	}
	for table, cols := range colFreq {
		for col, n := range cols {
			if n < 2 {
				continue // 偶发过滤不提示
			}
			if !indexCovers(e.model.Indexes, table, col) {
				e.add("ARCH023", fmt.Sprintf("表 %s 过滤/连接列 %s 出现 %d 次但无索引", table, col, n),
					schemaRuleIndex["ARCH023"].Advice, []string{table}, nil,
					map[string]any{"table": table, "column": col, "hits": n})
			}
		}
	}
}

// ---------------- 关系完整性 ARCH025–028

func (e *schemaEngine) checkRelations() {
	// ARCH025：外键目标表不在批次内
	for _, ref := range e.model.Refs {
		if _, ok := e.model.Tables[ref.ToTable]; !ok {
			e.add("ARCH025", fmt.Sprintf("外键 %s.%s 指向批次内不存在的表 %s", ref.FromTable, ref.FromColumns, ref.ToTable),
				schemaRuleIndex["ARCH025"].Advice, []string{ref.FromTable}, []string{ref.SourceFile},
				map[string]any{"from": ref.FromTable, "to": ref.ToTable})
		}
	}

	// ARCH026：隐式引用 xxx_id 无 FK 也无索引
	for name, t := range e.model.Tables {
		for _, c := range t.Columns {
			col := strings.ToLower(c.Name)
			if !strings.HasSuffix(col, "_id") || col == "id" {
				continue
			}
			if refExists(e.model.Refs, name, col) {
				continue
			}
			if !indexCovers(e.model.Indexes, name, col) {
				e.add("ARCH026", fmt.Sprintf("隐式引用列 %s.%s 无外键也无索引", t.QualifiedName, c.Name),
					schemaRuleIndex["ARCH026"].Advice, []string{name}, []string{t.SourceFile},
					map[string]any{"table": name, "column": col})
			}
		}
	}

	// ARCH027：循环外键（DFS 邻接表）
	e.detectCycleFK()

	// ARCH028：CASCADE 风险
	for _, ref := range e.model.Refs {
		if strings.EqualFold(ref.OnDelete, "CASCADE") {
			e.add("ARCH028", fmt.Sprintf("外键 %s.%s ON DELETE CASCADE，级联删除影响范围需确认", ref.FromTable, ref.FromColumns),
				schemaRuleIndex["ARCH028"].Advice, []string{ref.FromTable}, []string{ref.SourceFile},
				map[string]any{"table": ref.FromTable, "column": ref.FromColumns})
		}
	}
}

func refExists(refs []Reference, table, col string) bool {
	for _, r := range refs {
		if strings.EqualFold(r.FromTable, table) && strings.EqualFold(r.FromColumns, col) {
			return true
		}
	}
	return false
}

func (e *schemaEngine) detectCycleFK() {
	graph := map[string][]string{}
	for _, r := range e.model.Refs {
		if r.Kind == "FK" {
			graph[r.FromTable] = append(graph[r.FromTable], r.ToTable)
		}
	}
	state := map[string]int{} // 0 unvisited,1 visiting,2 done
	var dfs func(node string) bool
	dfs = func(node string) bool {
		state[node] = 1
		for _, next := range graph[node] {
			if state[next] == 1 {
				return true
			}
			if state[next] == 0 && dfs(next) {
				return true
			}
		}
		state[node] = 2
		return false
	}
	for node := range graph {
		if state[node] == 0 && dfs(node) {
			e.add("ARCH027", "外键关系存在循环依赖",
				schemaRuleIndex["ARCH027"].Advice, nil, nil, nil)
			return
		}
	}
}

// ---------------- 迁移与演进安全 ARCH029–032

func (e *schemaEngine) checkMigration() {
	// ARCH029：新增 NOT NULL 列无默认值。
	// 只判 ALTER ADD/MODIFY/CHANGE 引入的列：CREATE TABLE 的 NOT NULL 列
	// 本就没有存量数据（表是新建的），报「存量数据无法填充」属于误报；
	// 主键/自增/IDENTITY 列也豁免。
	for name, t := range e.model.Tables {
		for _, c := range t.Columns {
			if !c.AddedByAlter {
				continue
			}
			if c.Nullable || c.Default != "" || c.AutoIncrement || c.Identity || pkMember(t, c.Name) {
				continue
			}
			e.add("ARCH029", fmt.Sprintf("列 %s.%s 为 NOT NULL 且无默认值，存量数据无法填充", t.QualifiedName, c.Name),
				schemaRuleIndex["ARCH029"].Advice, []string{name}, []string{t.SourceFile},
				map[string]any{"table": name, "column": c.Name})
		}
	}

	// ARCH032：重复 CREATE TABLE（无 IF NOT EXISTS 守卫由调用方在词法层近似；
	// 这里按同一表名被多次登记为 CREATE 判定）。
	for key, srcs := range e.model.seenCreate {
		if len(srcs) > 1 {
			var files []string
			for _, s := range srcs {
				files = append(files, s.File)
			}
			e.add("ARCH032", fmt.Sprintf("表 %s 在 %d 个脚本中重复定义", key, len(srcs)),
				schemaRuleIndex["ARCH032"].Advice, []string{key}, files,
				map[string]any{"table": key, "files": files})
		}
	}
}

// ---------------- 去重 / 上限 / 排序 / 建议聚合

func (e *schemaEngine) dedupeAndCap(perRule int) {
	seen := map[string]struct{}{}
	out := make([]SchemaFinding, 0, len(e.findings))
	count := map[string]int{}
	for _, f := range e.findings {
		key := f.RuleID + "|" + strings.Join(f.Tables, ",") + "|" + strings.Join(f.Files, ",") + "|" + f.Message
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		count[f.RuleID]++
		if count[f.RuleID] > perRule {
			continue
		}
		out = append(out, f)
	}
	e.findings = out
}

func sortFindings(fs []SchemaFinding) {
	sort.SliceStable(fs, func(i, j int) bool {
		ri, rj := severityRank[fs[i].Severity], severityRank[fs[j].Severity]
		if ri != rj {
			return ri > rj
		}
		return fs[i].RuleID < fs[j].RuleID
	})
}

func modelTableList(m *SchemaModel) []TableDef {
	out := make([]TableDef, 0, len(m.Tables))
	for _, t := range m.Tables {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// buildSuggestions 按表聚合建议卡，关联规则与最佳实践。
func (e *schemaEngine) buildSuggestions() []SchemaSuggest {
	byTable := map[string][]SchemaFinding{}
	for _, f := range e.findings {
		if len(f.Tables) == 0 {
			continue
		}
		for _, t := range f.Tables {
			byTable[t] = append(byTable[t], f)
		}
	}
	out := []SchemaSuggest{}
	for table, fs := range byTable {
		ruleIDs := map[string]bool{}
		practiceIDs := map[string]bool{}
		var files []string
		var msgs []string
		for _, f := range fs {
			ruleIDs[f.RuleID] = true
			for _, p := range f.PracticeIDs {
				practiceIDs[p] = true
			}
			files = append(files, f.Files...)
			msgs = append(msgs, f.Message)
		}
		sg := SchemaSuggest{
			Table:   table,
			Title:   fmt.Sprintf("表 %s 优化建议", table),
			Message: strings.Join(msgs, "；"),
			Advice:  fs[0].Advice,
			Files:   dedupStrings(files),
		}
		for r := range ruleIDs {
			sg.RuleIDs = append(sg.RuleIDs, r)
		}
		for p := range practiceIDs {
			sg.PracticeIDs = append(sg.PracticeIDs, p)
		}
		sort.Strings(sg.RuleIDs)
		sort.Strings(sg.PracticeIDs)
		// 关联最佳实践的正反 DDL 片段
		if p := firstPractice(fs[0].PracticeIDs); p != nil {
			sg.GoodSQL = p.GoodSQL
			sg.BadSQL = p.BadSQL
		}
		out = append(out, sg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Table < out[j].Table })
	return out
}

func firstPractice(ids []string) *Practice {
	for _, id := range ids {
		for i := range practiceCatalog {
			if practiceCatalog[i].ID == id {
				return &practiceCatalog[i]
			}
		}
	}
	return nil
}

func dedupStrings(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------- 评分（独立，不改动六维质量分）

// scoreSchema 复用 v0.1.32 同规则递减计分：首次全额、其后 25%、单规则封顶 2×。
func scoreSchema(fs []SchemaFinding) (int, string, string) {
	seen := map[string]int{}
	acc := map[string]float64{}
	total := 0.0
	level := "pass"
	for _, f := range fs {
		if severityRank[f.Severity] > severityRank[level] {
			level = f.Severity
		}
		w := float64(severityWeight[f.Severity])
		add := w
		if seen[f.RuleID] > 0 {
			add = w * 0.25
		}
		cap := 2 * w
		if acc[f.RuleID]+add > cap {
			add = cap - acc[f.RuleID]
		}
		if add < 0 {
			add = 0
		}
		acc[f.RuleID] += add
		total += add
		seen[f.RuleID]++
	}
	score := 100 - int(math.Round(total))
	if score < 0 {
		score = 0
	}
	if len(fs) > 0 && level == "pass" {
		level = "info"
	}
	grade, label, _ := gradeOf(score, level)
	return score, grade, label
}

// ---------------- 命名与保留字

func snakeCase(s string) bool {
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			return false
		}
	}
	return true
}

var reservedWords = map[string]bool{
	"order": true, "group": true, "user": true, "select": true, "from": true,
	"table": true, "index": true, "key": true, "primary": true, "foreign": true,
	"default": true, "check": true, "unique": true, "column": true, "view": true,
	"schema": true, "database": true, "grant": true, "references": true, "where": true,
}

func isReservedWord(name string) bool {
	return reservedWords[strings.ToLower(name)]
}
