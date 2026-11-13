package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ---------------------------------------------------------------- 数据结构

type Finding struct {
	RuleID    string         `json:"ruleId"`
	Title     string         `json:"title"`
	Category  string         `json:"category"`
	Severity  string         `json:"severity"`
	Message   string         `json:"message"`
	Advice    string         `json:"advice"`
	File      string         `json:"file,omitempty"`
	Line      int            `json:"line"`
	StmtIndex int            `json:"stmtIndex"`
	Snippet   string         `json:"snippet"`
	Dynamic   bool           `json:"dynamic,omitempty"`
	Evidence  map[string]any `json:"evidence,omitempty"`
	// Optional 标记这是「项目约定类」命中，默认关闭的规则不会出现在这里。
	Optional bool `json:"optional,omitempty"`
}

type Result struct {
	File        string         `json:"file"`
	Version     string         `json:"version,omitempty"`
	Description string         `json:"description,omitempty"`
	Statements  int            `json:"statements"`
	Tables      []string       `json:"tables"`
	Score       int            `json:"score"`
	Level       string         `json:"level"`
	Findings    []Finding      `json:"findings"`
	Counts      map[string]int `json:"counts"`
	Dynamic     int            `json:"dynamicStatements"`
	Dialect     string         `json:"dialect"`
	Note        string         `json:"note,omitempty"`
	// Stats 是客观规模统计；Metrics 是由命中 + Stats 推出的质量画像（见 metrics.go）。
	Stats   QualityStats   `json:"stats"`
	Metrics QualityMetrics `json:"metrics"`
}

// refresh 在外部往 Findings 里补了命中（例如批量为文件名补 SQL010）之后重算派生字段。
func (r *Result) refresh() {
	r.Counts = map[string]int{}
	for _, f := range r.Findings {
		r.Counts[f.Severity]++
	}
	r.Score, r.Level = scoreOf(r.Findings)
	r.Metrics = ComputeQuality(r.Findings, r.Stats)
}

// AnalyzeOptions 控制一次评审的口径。
type AnalyzeOptions struct {
	// Migration 表示按迁移脚本规则评审（开启幂等/命名类规则）。
	Migration bool
	// Dialect 为空时按内容自动判定（见 dialect.go）。
	Dialect Dialect
	// NoUseRequired 关闭「必须写 USE」这条约定。
	// 通用插件默认关闭它——是否要求 USE 属于项目约定，不是通用 SQL 质量问题。
	NoUseRequired bool
}

type stmtInfo struct {
	Tokens    []Token
	Raw       string
	Line      int
	Index     int
	Kind      string
	Obj       string
	Tables    []string
	HasWhere  bool
	HasLimit  bool
	AlwaysTru bool
	Dynamic   bool
	Parent    int
}

type analyzer struct {
	cfg       RuleConfig
	opts      AnalyzeOptions
	file      string
	findings  []Finding
	stmts     []stmtInfo
	guarded   bool
	nonIdemLn []int
	dynCount  int
	// declaredDB 是脚本自身声明的目标库（首个 USE）。
	declaredDB string
	// dialect 决定哪些规则参与判定。
	dialect Dialect
}

// ---------------------------------------------------------------- 入口

// AnalyzeSQL 对一段 SQL 做静态规则评审。
//
// 全程不连接数据库、不执行 SQL：只做词法分析与规则匹配。
func AnalyzeSQL(sql, file string, cfg RuleConfig, opts AnalyzeOptions) *Result {
	a := &analyzer{cfg: cfg, opts: opts, file: file}
	a.dialect = resolveDialect(opts.Dialect, sql, file)
	raw := sql
	if strings.HasPrefix(raw, "\uFEFF") {
		raw = strings.TrimPrefix(raw, "\uFEFF")
		a.add("SQL017", 1, 0, "文件带 UTF-8 BOM",
			"去掉 BOM（编辑器或 .gitattributes 固定 UTF-8 无 BOM）。", nil, false)
	}
	if strings.Contains(raw, "\r\n") && strings.Contains(strings.ReplaceAll(raw, "\r\n", ""), "\n") {
		a.add("SQL017", 1, 0, "文件混用 CRLF 与 LF 换行",
			"统一为 LF，避免校验和与 diff 噪声。", nil, false)
	}
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	a.guarded = strings.Contains(strings.ToUpper(raw), "INFORMATION_SCHEMA")
	if a.dialect == DialectSQLServer {
		upper := strings.ToUpper(raw)
		a.guarded = a.guarded || strings.Contains(upper, "OBJECT_ID") ||
			strings.Contains(upper, "COL_LENGTH") || strings.Contains(upper, "SYS.OBJECTS") ||
			strings.Contains(upper, "SYS.COLUMNS") || strings.Contains(upper, "IF EXISTS") ||
			strings.Contains(upper, "IF NOT EXISTS")
	}

	stmts := splitStatementsDialect(raw, a.dialect)
	for _, s := range stmts {
		info := classifyDialect(s.Text, s.Line, s.Index, a.dialect)
		a.stmts = append(a.stmts, info)
	}
	a.checkFileLevel(raw)
	a.checkLibraryContext()
	for i := range a.stmts {
		a.checkStatement(&a.stmts[i])
	}
	a.checkDynamic(raw)
	a.emitFileSummaries()
	a.adjustSQLite()
	a.adjustConstraintSwap()

	tables := a.tableList()
	res := &Result{
		File:       file,
		Statements: len(stmts),
		Tables:     tables,
		Findings:   a.finalize(),
		Dialect:    string(a.dialect),
		Stats:      CollectStats(raw, a.stmts, tables),
	}
	if v, d, ok := parseVersionedName(file); ok {
		res.Version, res.Description = v, d
	}
	for _, f := range res.Findings {
		if f.Dynamic {
			res.Dynamic++
		}
	}
	res.Stats.Dynamic = res.Dynamic
	res.refresh()
	if len(res.Findings) == 0 {
		res.Note = "未命中任何规则"
	}
	return res
}

// ---------------------------------------------------------------- 规则落点

func (a *analyzer) add(ruleID string, line, stmtIdx int, msg, advice string, ev map[string]any, dynamic bool) {
	a.addSev(ruleID, "", line, stmtIdx, msg, advice, ev, dynamic)
}

func (a *analyzer) addSev(ruleID, sevOverride string, line, stmtIdx int, msg, advice string, ev map[string]any, dynamic bool) {
	if a.ruleDisabled(ruleID) {
		return
	}
	meta, ok := ruleIndex[ruleID]
	if !ok {
		return
	}
	if !meta.appliesTo(a.dialect) {
		return // 该规则不适用于当前方言（例如 ENGINE/CHARSET 之于 SQLite）
	}
	// 房规类规则（Optional）的启用状态只由 Disabled 列表决定：
	// 默认配置把它们放进去，用户在规则面板勾选后前端会移除。
	sev := meta.DefaultSeverity
	if sevOverride != "" {
		sev = sevOverride
	}
	if s, ok := a.cfg.Severity[ruleID]; ok && s != "" {
		sev = s
	}
	adv := meta.Advice
	if advice != "" {
		adv = advice
	}
	lineNo := line
	if lineNo <= 0 {
		lineNo = 1
	}
	f := Finding{
		RuleID: ruleID, Title: meta.Title, Category: meta.Category, Severity: sev,
		Message: msg, Advice: adv, File: a.file, Line: lineNo, StmtIndex: stmtIdx,
		Dynamic: dynamic, Evidence: ev, Optional: meta.Optional,
	}
	if stmtIdx > 0 && stmtIdx <= len(a.stmts) {
		f.Snippet = snippet(a.stmts[stmtIdx-1].Raw, 200)
	}
	a.findings = append(a.findings, f)
}

// ruleDisabled 判断规则是否被禁用。
//
// 房规类规则（Optional）的语义是「默认关闭，用户可开启」：
// 默认配置把它们放进 Disabled；用户在规则面板勾选后，前端会把它们从
// Disabled 里移除，此时规则就该生效——所以这里对 Optional 规则不再额外拦截。
func (a *analyzer) ruleDisabled(id string) bool {
	for _, d := range a.cfg.Disabled {
		if strings.EqualFold(d, id) {
			return true
		}
	}
	return false
}

func (a *analyzer) finalize() []Finding {
	// 去重：同一规则 + 同一语句 + 同一消息只留一条
	seen := map[string]struct{}{}
	out := make([]Finding, 0, len(a.findings))
	for _, f := range a.findings {
		key := fmt.Sprintf("%s|%d|%d|%s", f.RuleID, f.StmtIndex, f.Line, f.Message)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, f)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := severityRank[out[i].Severity], severityRank[out[j].Severity]
		if ri != rj {
			return ri > rj
		}
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		return out[i].RuleID < out[j].RuleID
	})
	return out
}

func scoreOf(fs []Finding) (int, string) {
	score := 0
	level := "pass"
	for _, f := range fs {
		score += severityWeight[f.Severity]
		if severityRank[f.Severity] > severityRank[level] {
			level = f.Severity
		}
	}
	if score > 100 {
		score = 100
	}
	if len(fs) > 0 && level == "pass" {
		level = "info"
	}
	return score, level
}

func (a *analyzer) tableList() []string {
	set := map[string]struct{}{}
	var out []string
	for _, s := range a.stmts {
		for _, t := range s.Tables {
			if _, ok := set[t]; ok {
				continue
			}
			set[t] = struct{}{}
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------- 文件级

func (a *analyzer) checkFileLevel(raw string) {
	max := a.cfg.Thresholds.MaxStatements
	if max <= 0 {
		max = 40
	}
	if len(a.stmts) > max {
		a.add("SQL014", 1, 0, fmt.Sprintf("单文件 %d 条语句（阈值 %d）", len(a.stmts), max),
			"按主题拆分脚本，一个脚本只做一件事。", map[string]any{"statements": len(a.stmts), "threshold": max}, false)
	}

	if strings.Contains(strings.ToUpper(raw), "DELIMITER") &&
		countWord(raw, "DELIMITER")%2 == 1 {
		a.add("SQL016", 1, 0, "DELIMITER 指令未成对出现",
			"过程/触发器定义结束后补 DELIMITER ;。", nil, false)
	}

	hasTx, hasDDL := false, false
	for _, s := range a.stmts {
		switch s.Kind {
		case "BEGIN", "COMMIT", "ROLLBACK", "START":
			hasTx = true
		case "ALTER", "CREATE", "DROP", "TRUNCATE", "RENAME":
			hasDDL = true
		}
	}
	if hasTx && hasDDL {
		a.add("SQL015", 1, 0, "脚本内同时出现显式事务与 DDL",
			"DDL 会隐式提交，事务包住 DDL 无法回滚；把 DML 与 DDL 拆成不同脚本。", nil, false)
	}
}

// checkLibraryContext 校验脚本的库上下文（仅 MySQL 方言有意义）。
//
// 这里只做「脚本自身是否自洽」的判断，不涉及任何真实数据库：
//  1. 脚本中途切换到另一个库 → 前后语句落不同库（SQL013 high）
//  2. USE 出现在第一个对象操作之后 → 前面语句会落到默认库（SQL018 high）
//
// SQL018 是「项目约定类」规则，默认关闭。
func (a *analyzer) checkLibraryContext() {
	if a.dialect == DialectSQLite {
		return
	}

	type useRef struct {
		idx, line int
		db        string
	}
	var uses []useRef
	for i := range a.stmts {
		if a.stmts[i].Kind != "USE" {
			continue
		}
		if db := normalizeDBName(a.stmts[i].Raw); db != "" {
			uses = append(uses, useRef{idx: i, line: a.stmts[i].Line, db: db})
		}
	}
	if len(uses) > 0 {
		a.declaredDB = uses[0].db
	}
	if len(uses) == 0 {
		return
	}

	// ① 中途切换库：同一脚本里出现两个不同的库，才是真正的跨库风险
	first := uses[0].db
	for _, u := range uses[1:] {
		if strings.EqualFold(u.db, first) {
			continue
		}
		a.addSev("SQL013", "high", u.line, u.idx,
			fmt.Sprintf("脚本内从 `%s` 切换到 `%s`", first, u.db),
			"一个迁移脚本只作用于一个库；跨库变更拆成独立脚本，避免中途切换导致前后语句落错库。",
			map[string]any{"from": first, "to": u.db}, false)
		return
	}

	// ② USE 之前已有裸表操作 → 那些语句已在默认库执行
	for i := 0; i < uses[0].idx && i < len(a.stmts); i++ {
		s := a.stmts[i]
		if s.Kind == "USE" || s.Kind == "SET" || len(bareTables(s)) == 0 {
			continue
		}
		if !a.opts.NoUseRequired {
			a.addSev("SQL018", "high", uses[0].line, uses[0].idx,
				fmt.Sprintf("USE `%s`; 出现在第 %d 条语句，第 %d 行的对象操作已会落到默认库", uses[0].db, uses[0].idx+1, s.Line),
				fmt.Sprintf("把 USE `%s`; 提到第一个对象操作之前。", uses[0].db),
				map[string]any{"declared": uses[0].db, "before": s.Line}, false)
		}
		return
	}
}

// emitFileSummaries 汇总那些「一条就够」的结论，避免每个语句刷一条。
func (a *analyzer) emitFileSummaries() {
	if a.dynCount > 0 {
		a.add("SQL008", 1, 0,
			fmt.Sprintf("脚本使用动态 SQL（PREPARE/EXECUTE %d 处）", a.dynCount),
			"确认拼接变量的取值分支都可重放；必要时拆成显式 DDL。",
			map[string]any{"count": a.dynCount}, false)
	}
	if a.opts.Migration && len(a.nonIdemLn) > 0 && (a.dialect == DialectPostgreSQL || !a.guarded) {
		switch a.dialect {
		case DialectPostgreSQL:
			a.add("SQL080", 1, 0,
				fmt.Sprintf("结构变更缺少幂等守卫（%d 处）", len(a.nonIdemLn)),
				"CREATE 使用 IF NOT EXISTS，DROP 使用 IF EXISTS；复杂变更用 to_regclass()/pg_catalog 或 DO 块前置判断。",
				map[string]any{"lines": a.nonIdemLn}, false)
		case DialectSQLServer:
			a.add("SQL070", 1, 0,
				fmt.Sprintf("结构变更缺少 OBJECT_ID/COL_LENGTH 等幂等守卫（%d 处）", len(a.nonIdemLn)),
				"用 OBJECT_ID、COL_LENGTH、sys.objects/sys.columns 或 INFORMATION_SCHEMA 编写 IF 守卫，保证迁移可重放。",
				map[string]any{"lines": a.nonIdemLn}, false)
		default:
			a.add("SQL012", 1, 0,
				fmt.Sprintf("结构变更未使用幂等守卫（%d 处）", len(a.nonIdemLn)),
				"使用 IF NOT EXISTS，或先查询系统目录再条件执行，保证脚本可重复执行。",
				map[string]any{"lines": a.nonIdemLn}, false)
		}
	}
}

func countWord(s, w string) int {
	return strings.Count(strings.ToUpper(s), w)
}

func upperTokens(toks []Token) string {
	var b strings.Builder
	b.WriteString(" ")
	for _, t := range toks {
		if t.Kind == "word" || t.Kind == "ident" {
			b.WriteString(strings.ToUpper(t.Text))
		} else {
			b.WriteString(t.Text)
		}
		b.WriteString(" ")
	}
	return strings.Join(strings.Fields(b.String()), " ") + " "
}

// ---------------------------------------------------------------- 语句级

func (a *analyzer) checkStatement(s *stmtInfo) {
	if a.dialect == DialectSQLite {
		a.checkSQLiteStatement(s)
	}
	if a.dialect == DialectSQLServer {
		a.checkSQLServerStatement(s)
	}
	if a.dialect == DialectPostgreSQL {
		a.checkPostgresStatement(s)
	}
	switch s.Kind {
	case "DROP":
		switch s.Obj {
		case "TABLE", "VIEW", "DATABASE", "SCHEMA":
			a.add("SQL001", s.Line, s.Index,
				fmt.Sprintf("DROP %s %s", s.Obj, strings.Join(s.Tables, ", ")),
				"迁移脚本原则上不 DROP；先归档一个版本，确认无引用后再清理。",
				map[string]any{"object": s.Obj, "tables": s.Tables}, s.Dynamic)
		case "INDEX", "FOREIGN":
			a.add("SQL005", s.Line, s.Index, "删除索引/约束",
				"先确认无查询依赖该索引，再在低峰执行。", nil, s.Dynamic)
		case "USER":
			a.add("SQL006", s.Line, s.Index, "删除数据库账号",
				"账号变更走 DBA 工单。", nil, s.Dynamic)
		}
	case "TRUNCATE":
		a.add("SQL002", s.Line, s.Index,
			fmt.Sprintf("TRUNCATE %s", strings.Join(s.Tables, ", ")),
			"改为 DELETE + WHERE 分批清理，或先备份到归档表。",
			map[string]any{"tables": s.Tables}, s.Dynamic)
	case "UPDATE", "DELETE":
		batchAdvice := "补主键/唯一键条件；修数走分批脚本并带 LIMIT。"
		joinAdvice := "回填脚本建议分批 + LIMIT，并在低峰执行；确认 JOIN 谓词能走索引。"
		if a.dialect == DialectSQLServer {
			batchAdvice = "补主键/唯一键条件；修数使用 TOP + 稳定 ORDER BY 分批。"
			joinAdvice = "回填脚本建议使用 TOP 分批并在低峰执行；确认 JOIN 谓词能走索引。"
		}
		if !s.HasWhere {
			if hasJoin(s.Tokens) {
				a.addSev("SQL003", "medium", s.Line, s.Index,
					fmt.Sprintf("%s 通过 JOIN 回填，没有独立 WHERE（只更新匹配行，但仍是全表扫描）", s.Kind),
					joinAdvice,
					map[string]any{"tables": s.Tables, "hasLimit": s.HasLimit}, s.Dynamic)
			} else if s.Kind == "UPDATE" && isNullBackfill(s.Tokens) {
				// 典型写法：迁移里先把 NULL 归一再 MODIFY NOT NULL。
				// 「没有 WHERE」是有意的，代价是全表锁，不是误覆盖数据 → 降为中。
				a.addSev("SQL003", "medium", s.Line, s.Index,
					fmt.Sprintf("UPDATE 全表回填 %s（无 WHERE 是有意的：SET 只做 IFNULL/COALESCE 归一，不会覆盖已有值）",
						strings.Join(s.Tables, ", ")),
					"回填会锁全表：大表请确认行数与执行窗口，必要时按主键分批。",
					map[string]any{"tables": s.Tables, "backfill": true}, s.Dynamic)
			} else {
				a.add("SQL003", s.Line, s.Index,
					fmt.Sprintf("%s 未带 WHERE，将作用于全表 %s", s.Kind, strings.Join(s.Tables, ", ")),
					batchAdvice,
					map[string]any{"tables": s.Tables, "hasLimit": s.HasLimit}, s.Dynamic)
			}
		} else if s.AlwaysTru {
			a.add("SQL004", s.Line, s.Index,
				fmt.Sprintf("%s 的 WHERE 条件恒真", s.Kind),
				"删除恒真条件，写入真实过滤条件。", nil, s.Dynamic)
		}
		a.checkCrossDB(s)
	case "SELECT":
		a.checkSelect(s)
		a.checkCrossDB(s)
		a.checkImplicitJoin(s)
	case "ALTER":
		a.checkAlter(s)
	case "CREATE":
		a.checkCreate(s)
	case "INSERT", "REPLACE":
		a.checkInsert(s)
	case "LOAD":
		a.add("SQL007", s.Line, s.Index, "LOAD DATA 批量导入",
			"导入走运维通道并分批，避免长事务与主从延迟。", nil, s.Dynamic)
	case "CALL":
		a.add("SQL008", s.Line, s.Index, "调用存储过程，内部逻辑无法静态审计",
			"评审时附上过程定义，或把关键变更改为显式 DDL。", nil, s.Dynamic)
	case "GRANT", "REVOKE":
		a.add("SQL006", s.Line, s.Index, s.Kind+" 权限变更",
			"权限变更走 DBA 工单，不进应用迁移目录。", nil, s.Dynamic)
	case "SET":
		if hasGlobalSet(s.Tokens) {
			a.add("SQL006", s.Line, s.Index, "SET GLOBAL/会话级实例参数",
				"实例参数变更走 DBA 工单。", nil, s.Dynamic)
		}
	case "PREPARE":
		a.dynCount++
	}
}

// bareTables 取语句里没有库前缀的表名。
// 带库前缀（db.tbl）执行目标明确，不依赖连接默认库。
func bareTables(s stmtInfo) []string {
	var out []string
	for _, t := range s.Tables {
		if strings.LastIndex(t, ".") >= 0 {
			continue
		}
		if isSystemSchema(t) {
			continue
		}
		out = append(out, t)
	}
	return out
}

func normalizeDBName(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimSuffix(s, ";")
	if len(s) >= 3 && strings.EqualFold(s[:3], "use") {
		s = s[3:]
	}
	s = strings.TrimSpace(s)
	return strings.Trim(s, "`\"'[] ")
}

func hasCreateLike(toks []Token) bool {
	// 只认 CREATE TABLE <name> LIKE <other>：LIKE 必须出现在第一个 "(" 之前
	for _, t := range toks {
		if t.Kind == "op" && t.Text == "(" {
			return false
		}
		if t.Kind == "word" && t.Upper == "LIKE" {
			return true
		}
	}
	return false
}

func hasGlobalSet(toks []Token) bool {
	for _, t := range toks {
		if t.Kind == "var" && strings.HasPrefix(t.Text, "@@global") {
			return true
		}
		if t.Kind == "word" && (t.Upper == "GLOBAL" || t.Upper == "PERSIST") {
			return true
		}
	}
	return false
}

var sqlServerTopStarRe = regexp.MustCompile(`(?is)^\s*SELECT\s+(?:DISTINCT\s+)?TOP\s*(?:\([^)]*\)|\d+)(?:\s+PERCENT)?(?:\s+WITH\s+TIES)?\s+(?:[\w$#]+|\[[^\]]+\])?\s*\.?\s*\*`)

func (a *analyzer) checkSelect(s *stmtInfo) {
	toks := s.Tokens
	// SELECT *（T-SQL 额外识别 SELECT TOP (n) *）。
	starFound := false
	for i := 0; i < len(toks) && i < 6; i++ {
		if toks[i].Kind == "op" && toks[i].Text == "*" && i > 0 && toks[i-1].Upper == "SELECT" {
			a.add("SQL022", s.Line, s.Index, "SELECT * 返回全部列",
				"显式列出需要的列，避免破坏覆盖索引。", nil, s.Dynamic)
			starFound = true
			break
		}
		if i > 0 && toks[i-1].Upper == "DISTINCT" && toks[i].Kind == "op" && toks[i].Text == "*" {
			a.add("SQL022", s.Line, s.Index, "SELECT DISTINCT * 返回全部列",
				"显式列出需要的列。", nil, s.Dynamic)
			starFound = true
		}
	}
	if a.dialect == DialectSQLServer && !starFound && sqlServerTopStarRe.MatchString(s.Raw) {
		a.add("SQL022", s.Line, s.Index, "SELECT TOP * 返回全部列",
			"显式列出需要的列，避免破坏覆盖索引。", nil, s.Dynamic)
	}
	if len(s.Tables) > 0 && !s.HasWhere && !s.HasLimit {
		message := "SELECT 缺少 WHERE/LIMIT，将扫描全表 " + strings.Join(s.Tables, ", ")
		advice := "补过滤条件；全量读取加 LIMIT 并说明用途。"
		if a.dialect == DialectSQLServer {
			message = "SELECT 缺少 WHERE/TOP，将扫描全表 " + strings.Join(s.Tables, ", ")
			advice = "补过滤条件；全量读取使用 TOP + 稳定 ORDER BY 并说明用途。"
		}
		a.add("SQL023", s.Line, s.Index, message, advice,
			map[string]any{"tables": s.Tables}, s.Dynamic)
	}
	a.checkLikeAndFunctions(s)
}

func (a *analyzer) checkLikeAndFunctions(s *stmtInfo) {
	toks := s.Tokens
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t.Kind == "word" && (t.Upper == "LIKE" || t.Upper == "ILIKE") && i+1 < len(toks) && toks[i+1].Kind == "str" {
			v := toks[i+1].Text
			if strings.HasPrefix(v, "%") {
				a.add("SQL024", s.Line, s.Index, fmt.Sprintf("LIKE '%s' 前导通配", truncStr(v, 24)),
					"改为前缀匹配 'x%'，或使用全文/倒排索引。",
					map[string]any{"pattern": truncStr(v, 40)}, s.Dynamic)
			}
		}
		if t.Kind == "op" && t.Text == "(" && i > 0 && i+2 < len(toks) {
			fn := toks[i-1].Upper
			if isWrappingFunc(fn) && (toks[i+1].Kind == "word" || toks[i+1].Kind == "ident") {
				// 只报 WHERE 区域的函数包裹
				if inWhereRegion(toks, i) {
					a.add("SQL025", s.Line, s.Index, fmt.Sprintf("%s(%s) 包裹索引列", fn, toks[i+1].Text),
						"改写为范围条件，让索引可用。",
						map[string]any{"function": fn, "column": toks[i+1].Text}, s.Dynamic)
				}
			}
		}
		if t.Kind == "word" && t.Upper == "OR" && inWhereRegion(toks, i) && depthAt(toks, i) == 0 {
			a.add("SQL026", s.Line, s.Index, "WHERE 中使用 OR 条件",
				"拆成 UNION ALL，或确认可走 index merge。", nil, s.Dynamic)
		}
		if t.Kind == "word" && t.Upper == "NOT" && i+1 < len(toks) && toks[i+1].Upper == "IN" {
			a.add("SQL026", s.Line, s.Index, "使用 NOT IN 子查询",
				"改写为 NOT EXISTS / LEFT JOIN … IS NULL（注意 NULL 语义）。", nil, s.Dynamic)
		}
	}
}

func isWrappingFunc(fn string) bool {
	switch fn {
	case "DATE", "DATE_FORMAT", "YEAR", "MONTH", "DAY", "HOUR", "SUBSTR", "SUBSTRING",
		"LOWER", "UPPER", "TRIM", "CAST", "CONVERT", "IFNULL", "COALESCE", "FROM_UNIXTIME",
		"UNIX_TIMESTAMP", "LEFT", "RIGHT", "REPLACE", "CONCAT":
		return true
	}
	return false
}

func inWhereRegion(toks []Token, idx int) bool {
	where := -1
	for i, t := range toks {
		if t.Kind == "word" && t.Upper == "WHERE" && depthAt(toks, i) == 0 {
			where = i
			break
		}
	}
	if where < 0 || idx < where {
		return false
	}
	for i := where; i < idx; i++ {
		if toks[i].Kind == "word" && (toks[i].Upper == "ORDER" || toks[i].Upper == "GROUP" || toks[i].Upper == "LIMIT") && depthAt(toks, i) == 0 {
			return false
		}
	}
	return true
}

func depthAt(toks []Token, idx int) int {
	d := 0
	for i := 0; i < idx && i < len(toks); i++ {
		if toks[i].Kind == "op" {
			switch toks[i].Text {
			case "(":
				d++
			case ")":
				if d > 0 {
					d--
				}
			}
		}
	}
	return d
}

func (a *analyzer) checkAlter(s *stmtInfo) {
	u := upperTokens(s.Tokens)
	tbl := ""
	if len(s.Tables) > 0 {
		tbl = bareTable(s.Tables[0])
	}

	if a.dialect == DialectMySQL && a.opts.Migration && !a.guarded {
		if strings.Contains(u, " ADD ") || strings.Contains(u, " MODIFY ") ||
			strings.Contains(u, " CHANGE ") || strings.Contains(u, " DROP COLUMN ") {
			a.nonIdemLn = append(a.nonIdemLn, s.Line)
		}
	}

	// 注意：只认 `DROP COLUMN`。写成 `DROP … && … COLUMN …` 会把
	// 「DROP PRIMARY KEY, ADD COLUMN x」这种同句多动作误判成删列。
	if strings.Contains(u, " DROP COLUMN ") || strings.Contains(u, " DROP COLUMNS ") {
		a.add("SQL005", s.Line, s.Index, "ALTER TABLE 删除列 "+tbl,
			"先确认代码不再引用该列（含回滚版本），再删除。", map[string]any{"table": tbl}, s.Dynamic)
	}
	if strings.Contains(u, " DROP PRIMARY KEY ") || strings.Contains(u, " DROP PRIMARY KEY,") {
		a.add("SQL005", s.Line, s.Index, "ALTER TABLE 删除/替换主键 "+tbl,
			"主键变更会让依赖它的外键与唯一性判断失效；确认同一句里已重新 ADD PRIMARY KEY，并在低峰执行。",
			map[string]any{"table": tbl}, s.Dynamic)
	}
	if strings.Contains(u, " DROP CHECK ") || strings.Contains(u, " DROP CONSTRAINT ") {
		ev := map[string]any{"table": tbl}
		if m := dropCheckRe.FindStringSubmatch(s.Raw); m != nil {
			ev["constraint"] = m[1]
		}
		a.add("SQL005", s.Line, s.Index, "ALTER TABLE 删除约束 "+tbl,
			"放开约束前确认应用层已接管校验，并记录原因。", ev, s.Dynamic)
	}
	if strings.Contains(u, " DROP INDEX ") || strings.Contains(u, " DROP KEY ") {
		a.add("SQL005", s.Line, s.Index, "ALTER TABLE 删除索引 "+tbl,
			"确认无查询依赖该索引。", map[string]any{"table": tbl}, s.Dynamic)
	}
	if strings.Contains(u, " DROP FOREIGN KEY ") {
		a.add("SQL005", s.Line, s.Index, "ALTER TABLE 删除外键 "+tbl,
			"确认应用层已接管约束。", map[string]any{"table": tbl}, s.Dynamic)
	}
	// 未声明 ALGORITHM/LOCK：通用插件拿不到真实行数，
	// 因此对结构性 ALTER 一律提示，而不是「仅当大表才报」。
	if strings.Contains(u, " ADD ") || strings.Contains(u, " MODIFY ") || strings.Contains(u, " CHANGE ") ||
		strings.Contains(u, " DROP COLUMN ") {
		if !strings.Contains(u, " ALGORITHM ") && !strings.Contains(u, " LOCK ") {
			a.add("SQL020", s.Line, s.Index,
				fmt.Sprintf("ALTER TABLE %s 未声明 ALGORITHM/LOCK", tbl),
				"显式 ALGORITHM=INPLACE, LOCK=NONE（或 INSTANT），并在低峰执行。",
				map[string]any{"table": tbl}, s.Dynamic)
		}
	}
	if (strings.Contains(u, " ADD INDEX ") || strings.Contains(u, " ADD KEY ") || strings.Contains(u, " ADD UNIQUE ")) &&
		!strings.Contains(u, " LOCK=NONE") && !strings.Contains(u, " LOCK = NONE ") {
		a.add("SQL021", s.Line, s.Index,
			fmt.Sprintf("%s 新增索引未声明 LOCK=NONE", tbl),
			"加 ALGORITHM=INPLACE, LOCK=NONE；超大表用 gh-ost/pt-osc。",
			map[string]any{"table": tbl}, s.Dynamic)
	}
	if strings.Contains(u, " ADD CONSTRAINT ") && strings.Contains(u, "FOREIGN KEY") {
		a.add("SQL037", s.Line, s.Index, "新增外键约束 "+tbl,
			"多服务共库场景建议在应用层保证一致性。", map[string]any{"table": tbl}, s.Dynamic)
	}

	// ALTER TABLE ... ADD COLUMN 的列级检查
	if strings.Contains(u, " COLUMN ") || strings.Contains(u, " ADD ") {
		for _, col := range parseAlterColumns(s.Tokens) {
			a.checkColumn(tbl, col, s.Line, s.Index, s.Dynamic, false)
		}
	}
}

func (a *analyzer) checkCreate(s *stmtInfo) {
	switch s.Obj {
	case "TABLE":
		tbl := ""
		if len(s.Tables) > 0 {
			tbl = bareTable(s.Tables[0])
		}
		if hasCreateLike(s.Tokens) {
			break // CREATE TABLE … LIKE 继承父表结构，不重复检查
		}
		ct := parseCreateTable(s.Tokens)
		if !ct.HasPK {
			if a.dialect == DialectSQLite {
				a.addSev("SQL030", "info", s.Line, s.Index, "建表未定义主键："+tbl,
					"SQLite 会退化为 rowid 表；业务表仍建议显式 PRIMARY KEY，避免依赖隐式 rowid。",
					map[string]any{"table": tbl}, s.Dynamic)
			} else {
				a.add("SQL030", s.Line, s.Index, "建表未定义主键："+tbl,
					"补自增主键或业务唯一键。", map[string]any{"table": tbl}, s.Dynamic)
			}
		}
		cs := strings.ToLower(ct.Charset)
		if cs == "" {
			a.add("SQL031", s.Line, s.Index, "建表未显式指定字符集："+tbl,
				"补 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci。",
				map[string]any{"table": tbl}, s.Dynamic)
		} else if cs != "utf8mb4" {
			a.add("SQL031", s.Line, s.Index, fmt.Sprintf("建表字符集为 %s，不是 utf8mb4：%s", ct.Charset, tbl),
				"改为 utf8mb4，避免 emoji 与跨表 JOIN 隐式转换。",
				map[string]any{"table": tbl, "charset": ct.Charset}, s.Dynamic)
		}
		if ct.Engine == "" {
			a.add("SQL034", s.Line, s.Index, "建表未显式指定 ENGINE："+tbl,
				"显式 ENGINE=InnoDB。", map[string]any{"table": tbl}, s.Dynamic)
		}
		// 只对「新建表的列」检查 COMMENT（见 checkColumn 的 strictComment）。
		for _, col := range ct.Columns {
			a.checkColumn(tbl, col, s.Line, s.Index, s.Dynamic, true)
		}
	case "INDEX":
		tbl := indexTargetTable(s.Tokens)
		if tbl != "" {
			a.add("SQL021", s.Line, s.Index,
				fmt.Sprintf("新建索引：%s", tbl),
				"显式 ALGORITHM=INPLACE, LOCK=NONE；超大表用 gh-ost/pt-osc。",
				map[string]any{"table": tbl}, s.Dynamic)
		}
	case "DATABASE", "SCHEMA":
		u := upperTokens(s.Tokens)
		if !strings.Contains(u, "UTF8MB4") {
			a.add("SQL031", s.Line, s.Index, "建库未指定 utf8mb4",
				"补 DEFAULT CHARACTER SET utf8mb4。", nil, s.Dynamic)
		}
	}
}

func (a *analyzer) checkInsert(s *stmtInfo) {
	u := upperTokens(s.Tokens)
	if s.Kind == "REPLACE" {
		a.add("SQL007", s.Line, s.Index, "REPLACE INTO 覆盖写入",
			"改为 INSERT … ON DUPLICATE KEY UPDATE，避免删除+插入放大 binlog。", nil, s.Dynamic)
	}
	if strings.Contains(u, " SELECT ") && strings.Index(u, " SELECT ") > strings.Index(u, " INSERT ") {
		a.add("SQL007", s.Line, s.Index, "INSERT … SELECT 批量写入",
			"分批写入（每批 ≤ 1000 行），避免长事务。", nil, s.Dynamic)
	}
	if n := strings.Count(u, "),("); n >= 500 {
		a.add("SQL007", s.Line, s.Index, fmt.Sprintf("单条 INSERT 约 %d 组 VALUES", n+1),
			"拆分为多批，控制单事务体积。", map[string]any{"groups": n + 1}, s.Dynamic)
	}
	if !hasInsertColumnList(s.Tokens) {
		a.add("SQL038", s.Line, s.Index, "INSERT 未指定列清单，依赖目标表的列顺序",
			"写成 INSERT INTO t (col_a, col_b) VALUES (…)，否则表加列或调序后会写错字段。", nil, s.Dynamic)
	}
}

// hasInsertColumnList 判断 INSERT/REPLACE 是否在表名后紧跟列清单。
// INSERT INTO t (a, b) VALUES …  → true；INSERT INTO t VALUES … / SELECT … → false。
func hasInsertColumnList(toks []Token) bool {
	into := -1
	for i, t := range toks {
		if t.Kind == "word" && t.Upper == "INTO" {
			into = i
			break
		}
	}
	if into < 0 {
		return true // 认不出来就不报，避免误伤
	}
	j := into + 1
	// 表名（可能是 db.table 两段）
	for j < len(toks) && toks[j].Kind != "word" && toks[j].Kind != "ident" {
		if toks[j].Kind == "op" && toks[j].Text == "(" {
			return true
		}
		j++
	}
	if j >= len(toks) {
		return true
	}
	j++
	for j+1 < len(toks) && toks[j].Kind == "op" && toks[j].Text == "." {
		j += 2
	}
	for j < len(toks) {
		t := toks[j]
		if t.Kind == "op" && t.Text == "(" {
			return true
		}
		if t.Kind == "op" && t.Text == "." {
			j++
			continue
		}
		if t.Kind == "word" && (t.Upper == "VALUES" || t.Upper == "VALUE" ||
			t.Upper == "SELECT" || t.Upper == "SET" || t.Upper == "WITH") {
			return false
		}
		if t.Kind == "word" || t.Kind == "ident" {
			// 形如 INSERT INTO t AS alias 的写法：继续往后看
			j++
			continue
		}
		return false
	}
	return true
}

func (a *analyzer) checkCrossDB(s *stmtInfo) {
	for _, t := range s.Tables {
		idx := strings.LastIndex(t, ".")
		if idx < 0 {
			continue
		}
		prefix := t[:idx]
		if isSystemSchema(prefix) {
			continue // information_schema 等系统库是幂等守卫的正当用法
		}
		base := a.declaredDB
		if base != "" && strings.EqualFold(prefix, base) {
			continue // 与脚本声明的目标库一致，不算跨库
		}
		a.addSev("SQL013", "info", s.Line, s.Index, "引用了其他库对象："+t,
			"多库架构下属正常用法；确认执行账号对该库有权限，且脚本开头已用 USE 声明目标库。",
			map[string]any{"table": t}, s.Dynamic)
	}
}

func bareTable(t string) string {
	if i := strings.LastIndex(t, "."); i >= 0 {
		return t[i+1:]
	}
	return t
}

func isSystemSchema(s string) bool {
	switch strings.ToLower(s) {
	case "information_schema", "mysql", "performance_schema", "sys":
		return true
	}
	return false
}

func hasJoin(toks []Token) bool {
	for _, t := range toks {
		if t.Kind == "word" && (t.Upper == "JOIN" || t.Upper == "STRAIGHT_JOIN") {
			return true
		}
	}
	return false
}

// checkImplicitJoin 找出 FROM a, b 这种逗号隐式连接：连接条件漏写就是笛卡尔积。
func (a *analyzer) checkImplicitJoin(s *stmtInfo) {
	depth, inFrom, commas := 0, false, 0
	for _, t := range s.Tokens {
		if t.Kind == "op" {
			switch t.Text {
			case "(":
				depth++
			case ")":
				if depth > 0 {
					depth--
				}
			case ",":
				if inFrom && depth == 0 {
					commas++
				}
			}
			continue
		}
		if t.Kind != "word" {
			continue
		}
		switch t.Upper {
		case "FROM":
			inFrom = true
		case "WHERE", "GROUP", "ORDER", "HAVING", "LIMIT", "UNION":
			inFrom = false
		}
	}
	if commas == 0 {
		return
	}
	sev := "medium"
	if !s.HasWhere || s.AlwaysTru {
		sev = "high"
	}
	a.addSev("SQL027", sev, s.Line, s.Index,
		fmt.Sprintf("FROM 中使用 %d 处逗号隐式连接", commas),
		"改用 JOIN … ON 显式连接；隐式连接漏写条件就是笛卡尔积。", nil, s.Dynamic)
}

func (a *analyzer) checkColumn(table string, c colDef, line, idx int, dynamic bool, strictComment bool) {
	name := strings.ToLower(c.Name)
	typ := strings.ToUpper(c.Type)
	if a.dialect == DialectSQLite {
		a.checkSQLiteColumn(table, c, line, idx, dynamic)
	}
	if a.dialect == DialectSQLServer {
		a.checkSQLServerColumn(table, c, line, idx, dynamic)
	}
	if a.dialect == DialectPostgreSQL {
		a.checkPostgresColumn(table, c, line, idx, dynamic)
	}
	if isSensitiveColumnTyped(name, typ) {
		a.add("SQL040", line, idx, fmt.Sprintf("敏感列 %s.%s", table, c.Name),
			"确认以哈希/加密存储，日志与导出脱敏。",
			map[string]any{"table": table, "column": c.Name, "type": c.Type}, dynamic)
	}
	if (strings.HasPrefix(typ, "FLOAT") || strings.HasPrefix(typ, "DOUBLE") || strings.HasPrefix(typ, "REAL")) &&
		(a.dialect != DialectPostgreSQL || pgMoneyColumn(name)) {
		a.add("SQL032", line, idx, fmt.Sprintf("%s.%s 使用浮点类型 %s", table, c.Name, c.Type),
			"金额/配额改用 DECIMAL(p,2) 或 BIGINT 最小单位。",
			map[string]any{"table": table, "column": c.Name, "type": c.Type}, dynamic)
	}
	if strings.Contains(typ, "VARCHAR") && c.IsPK && strings.Contains(strings.ReplaceAll(typ, " ", ""), "VARCHAR(255)") {
		a.add("SQL036", line, idx, fmt.Sprintf("%s.%s 以 VARCHAR(255) 作主键", table, c.Name),
			"主键用 BIGINT；业务唯一键放 UNIQUE 索引。",
			map[string]any{"table": table, "column": c.Name}, dynamic)
	}
	if c.Indexed && (strings.HasPrefix(typ, "TEXT") || strings.HasPrefix(typ, "BLOB") || strings.HasPrefix(typ, "JSON")) {
		a.add("SQL036", line, idx, fmt.Sprintf("%s.%s 对 %s 建索引", table, c.Name, c.Type),
			"改用前缀索引并限制长度，或引入生成列。",
			map[string]any{"table": table, "column": c.Name}, dynamic)
	}
	if c.Name != "" && c.Name != strings.ToLower(c.Name) {
		a.add("SQL035", line, idx, fmt.Sprintf("列名 %s 含大写", c.Name),
			"统一小写下划线命名。", map[string]any{"column": c.Name}, dynamic)
	}
	if strictComment && a.opts.Migration && !c.HasComment {
		a.add("SQL033", line, idx, fmt.Sprintf("%s.%s 缺少 COMMENT", table, c.Name),
			"补 COMMENT，说明业务含义与取值范围。",
			map[string]any{"table": table, "column": c.Name}, dynamic)
	}
}

func isSensitiveColumn(n string) bool {
	n = strings.ToLower(n)
	keys := []string{"password", "passwd", "pwd", "secret", "token", "salt", "credential",
		"id_card", "idcard", "身份证", "phone", "mobile", "email", "bank_card", "bankcard",
		"private_key", "api_key", "access_key"}
	for _, k := range keys {
		if strings.Contains(n, k) {
			return true
		}
	}
	return false
}

// token 类列名里有相当一部分是「数量」而不是「凭据」：max_tokens / prompt_tokens / token_count。
// 只看名字会误报（model_providers.max_tokens 是 BIGINT 配额），所以带类型再判一次。
func isSensitiveColumnTyped(name, typ string) bool {
	if !isSensitiveColumn(name) {
		return false
	}
	n := strings.ToLower(name)
	if tokenCountNameRe.MatchString(n) && isNumericType(strings.ToLower(typ)) {
		return false
	}
	return true
}

var tokenCountNameRe = regexp.MustCompile(`(^|_)tokens?($|_)`)

func isNumericType(t string) bool {
	return strings.HasPrefix(t, "int") || strings.HasPrefix(t, "bigint") || strings.HasPrefix(t, "smallint") ||
		strings.HasPrefix(t, "tinyint") || strings.HasPrefix(t, "decimal") || strings.HasPrefix(t, "numeric") ||
		strings.HasPrefix(t, "double") || strings.HasPrefix(t, "float")
}

// ---------------------------------------------------------------- 动态 SQL

var ddlWordRe = regexp.MustCompile(`(?i)\b(alter|create|drop|truncate|rename|insert|update|delete)\b`)

func (a *analyzer) checkDynamic(raw string) {
	// SET @sql := '…DDL…' 形式：把字面量也按语句审一遍（标记 dynamic）
	for _, s := range a.stmts {
		if s.Kind != "SET" {
			continue
		}
		if !strings.Contains(strings.ToUpper(s.Raw), "PREPARE") && !strings.Contains(s.Raw, "':=") &&
			!strings.Contains(s.Raw, ":=") {
			continue
		}
		for _, t := range s.Tokens {
			if t.Kind != "str" || !ddlWordRe.MatchString(t.Text) {
				continue
			}
			a.analyzeDynamicLiteral(t.Text, s.Line, s.Index)
		}
	}
}

func (a *analyzer) analyzeDynamicLiteral(sqlText string, line, parentIdx int) {
	sub := splitStatementsDialect(sqlText, a.dialect)
	for _, st := range sub {
		info := classifyDialect(st.Text, line, parentIdx, a.dialect)
		info.Dynamic = true
		info.Parent = parentIdx
		before := len(a.findings)
		a.checkStatement(&info)
		// 动态分支里的危险语句按同一规则记录，但行号回落到父语句
		for i := before; i < len(a.findings); i++ {
			a.findings[i].Line = line
			a.findings[i].Dynamic = true
		}
	}
}

// ---------------------------------------------------------------- 分类与解析

func classify(text string, line, idx int) stmtInfo {
	return classifyDialect(text, line, idx, "")
}

func classifyDialect(text string, line, idx int, dialect Dialect) stmtInfo {
	toks := tokenizeDialect(text, dialect)
	s := stmtInfo{Tokens: toks, Raw: text, Line: line, Index: idx, Kind: "OTHER"}
	if len(toks) == 0 {
		return s
	}
	i := 0
	for i < len(toks) && toks[i].Kind == "op" && toks[i].Text == "(" {
		i++
	}
	if i >= len(toks) {
		return s
	}
	w := toks[i].Upper
	switch w {
	case "SELECT", "WITH":
		s.Kind = "SELECT"
	case "INSERT", "REPLACE":
		s.Kind = w
	case "UPDATE":
		s.Kind = "UPDATE"
	case "DELETE":
		s.Kind = "DELETE"
	case "ALTER":
		s.Kind = "ALTER"
		s.Obj = nextObject(i+1, toks)
	case "CREATE":
		s.Kind = "CREATE"
		s.Obj = nextObject(i+1, toks)
	case "DROP":
		s.Kind = "DROP"
		s.Obj = nextObject(i+1, toks)
	case "TRUNCATE":
		s.Kind = "TRUNCATE"
		s.Obj = "TABLE"
	case "RENAME":
		s.Kind = "RENAME"
	case "USE":
		s.Kind = "USE"
	case "PRAGMA", "VACUUM", "REINDEX":
		s.Kind = w
	case "ATTACH", "DETACH":
		s.Kind = w
	case "SET":
		s.Kind = "SET"
	case "PREPARE", "EXEC", "EXECUTE", "DEALLOCATE":
		s.Kind = w
	case "MERGE":
		s.Kind = w
	case "GRANT", "REVOKE":
		s.Kind = w
	case "LOAD":
		s.Kind = "LOAD"
	case "CALL":
		s.Kind = "CALL"
	case "START", "BEGIN", "COMMIT", "ROLLBACK":
		s.Kind = w
	}
	s.Tables = collectTables(toks)
	s.HasWhere = hasTopLevel(toks, "WHERE")
	s.HasLimit = hasTopLevel(toks, "LIMIT")
	if dialect == DialectSQLServer {
		s.HasLimit = hasTopLevel(toks, "TOP") || hasTopLevel(toks, "FETCH")
	}
	if s.HasWhere {
		s.AlwaysTru = alwaysTrueWhere(toks)
	}
	return s
}

func nextObject(from int, toks []Token) string {
	skip := map[string]bool{"OR": true, "REPLACE": true, "TEMPORARY": true, "UNIQUE": true,
		"FULLTEXT": true, "SPATIAL": true, "DEFINER": true, "ONLINE": true, "OFFLINE": true,
		"ALGORITHM": true, "IF": true, "NOT": true, "EXISTS": true}
	for i := from; i < len(toks) && i < from+8; i++ {
		t := toks[i]
		if t.Kind != "word" {
			continue
		}
		if skip[t.Upper] {
			continue
		}
		return t.Upper
	}
	return ""
}

func collectTables(toks []Token) []string {
	var out []string
	seen := map[string]bool{}
	depth := 0
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t.Kind == "op" {
			switch t.Text {
			case "(":
				depth++
			case ")":
				if depth > 0 {
					depth--
				}
			}
			continue
		}
		if depth != 0 || t.Kind != "word" {
			continue
		}
		kw := t.Upper
		if kw != "FROM" && kw != "JOIN" && kw != "INTO" && kw != "UPDATE" && kw != "TABLE" && kw != "TRUNCATE" {
			continue
		}
		skip := map[string]bool{"IF": true, "NOT": true, "EXISTS": true, "ONLY": true, "LOW_PRIORITY": true, "IGNORE": true}
		// 取后续标识符，支持 db.tbl
		j := i + 1
		name := ""
		for j < len(toks) && j < i+8 {
			tt := toks[j]
			if tt.Kind == "op" && tt.Text == "(" {
				break // 子查询
			}
			if tt.Kind == "word" || tt.Kind == "ident" {
				up := strings.ToUpper(tt.Text)
				if skip[up] {
					j++
					continue
				}
				if up == "SELECT" || up == "ON" {
					break
				}
				if name == "" {
					name = strings.ToLower(tt.Text)
				} else {
					name = name + "." + strings.ToLower(tt.Text)
				}
				if j+1 < len(toks) && toks[j+1].Kind == "op" && toks[j+1].Text == "." {
					j += 2
					continue
				}
				break
			}
			break
		}
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

func hasTopLevel(toks []Token, word string) bool {
	depth := 0
	for _, t := range toks {
		if t.Kind == "op" {
			switch t.Text {
			case "(":
				depth++
			case ")":
				if depth > 0 {
					depth--
				}
			}
			continue
		}
		if depth == 0 && t.Kind == "word" && t.Upper == word {
			return true
		}
	}
	return false
}

func alwaysTrueWhere(toks []Token) bool {
	wi := -1
	depth := 0
	for i, t := range toks {
		if t.Kind == "op" {
			switch t.Text {
			case "(":
				depth++
			case ")":
				if depth > 0 {
					depth--
				}
			}
			continue
		}
		if depth == 0 && t.Kind == "word" && t.Upper == "WHERE" {
			wi = i
			break
		}
	}
	if wi < 0 {
		return false
	}
	rest := toks[wi+1:]
	// 截到 ORDER/GROUP/LIMIT
	end := len(rest)
	d := 0
	for i, t := range rest {
		if t.Kind == "op" {
			switch t.Text {
			case "(":
				d++
			case ")":
				if d > 0 {
					d--
				}
			}
			continue
		}
		if d == 0 && t.Kind == "word" && (t.Upper == "ORDER" || t.Upper == "GROUP" || t.Upper == "LIMIT") {
			end = i
			break
		}
	}
	rest = rest[:end]
	var parts []string
	for _, t := range rest {
		if t.Kind == "op" && (t.Text == "(" || t.Text == ")" || t.Text == ";" || t.Text == ",") {
			continue
		}
		if t.Kind == "str" {
			parts = append(parts, "'"+strings.ToUpper(t.Text)+"'")
			continue
		}
		parts = append(parts, strings.ToUpper(t.Text))
	}
	s := strings.Join(parts, " ")
	switch s {
	case "1", "TRUE", "1 = 1", "1=1", "'A' = 'A'", "1 = 1 AND 1 = 1":
		return true
	}
	return false
}

func truncStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// isNullBackfill 判断「无 WHERE 的 UPDATE」是否只是把 NULL 归一的回填：
// SET 里的每个赋值都用 IFNULL/COALESCE 包住原列，不会覆盖已有值。
// 这类写法在「先回填、再 MODIFY NOT NULL」的迁移里是标准做法，不该按误覆盖报严重级。
func isNullBackfill(toks []Token) bool {
	assigns, guarded := 0, 0
	depth := 0
	inSet := false
	for _, t := range toks {
		if t.Kind == "op" && t.Text == "(" {
			depth++
			continue
		}
		if t.Kind == "op" && t.Text == ")" {
			if depth > 0 {
				depth--
			}
			continue
		}
		switch {
		case t.Kind == "word" && t.Upper == "SET" && depth == 0:
			inSet = true
		case t.Kind == "word" && (t.Upper == "WHERE" || t.Upper == "ORDER" || t.Upper == "LIMIT") && depth == 0:
			inSet = false
		case inSet && t.Kind == "op" && t.Text == "=" && depth == 0:
			assigns++
		case inSet && t.Kind == "word" && (t.Upper == "IFNULL" || t.Upper == "COALESCE"):
			guarded++
		}
	}
	return assigns > 0 && guarded >= assigns
}
