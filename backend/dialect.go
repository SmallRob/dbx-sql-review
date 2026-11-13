package main

import (
	"fmt"
	"regexp"
	"strings"
)

// Dialect 是 SQL 方言。MySQL、SQLite、SQL Server 与 PostgreSQL 的 DDL 约定
// 差得很远，同一套规则直接混用会大面积误报。所有方言都只做静态分析。
type Dialect string

const (
	DialectMySQL      Dialect = "mysql"
	DialectSQLite     Dialect = "sqlite"
	DialectSQLServer  Dialect = "sqlserver"
	DialectPostgreSQL Dialect = "postgres"
)

// NormalizeDialect 把外部传入的字符串收敛成方言值；空串表示「自动判定」。
func NormalizeDialect(s string) Dialect {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "sqlite", "sqlite3":
		return DialectSQLite
	case "mysql", "mariadb":
		return DialectMySQL
	case "sqlserver", "sql-server", "sql_server", "mssql", "tsql", "t-sql":
		return DialectSQLServer
	case "postgres", "postgresql", "pgsql", "pg":
		return DialectPostgreSQL
	}
	return ""
}

var (
	// MySQL 特征：ENGINE= / DEFAULT CHARSET / AUTO_INCREMENT / 反引号 / USE / information_schema
	mysqlHints = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\bENGINE\s*=`),
		regexp.MustCompile(`(?i)\bDEFAULT\s+CHARSET\b`),
		regexp.MustCompile(`(?i)\bAUTO_INCREMENT\b`),
		regexp.MustCompile(`(?i)\binformation_schema\.`),
		regexp.MustCompile("`"),
		regexp.MustCompile(`(?im)^\s*USE\s+\S`),
	}
	// SQL Server / T-SQL 特征：GO / TOP / IDENTITY / 方括号标识符 / NOLOCK 等。
	sqlServerHints = []*regexp.Regexp{
		regexp.MustCompile(`(?im)^\s*GO(?:\s+\d+)?\s*(?:--.*)?$`),
		regexp.MustCompile(`(?i)\bTOP\s*(?:\(|\d)`),
		regexp.MustCompile(`(?i)\bIDENTITY\s*\(`),
		regexp.MustCompile(`(?i)\bN(?:VAR)?CHAR\s*\(`),
		regexp.MustCompile(`(?i)\bDATETIME2\b`),
		regexp.MustCompile(`(?i)\bNOLOCK\b`),
		regexp.MustCompile(`(?i)\bSP_EXECUTESQL\b`),
		regexp.MustCompile(`(?i)\bMERGE\s+INTO\b`),
		regexp.MustCompile(`\[[A-Za-z_#@][^\]\r\n]*\]`),
	}
	// SQLite 特征：PRAGMA / AUTOINCREMENT / WITHOUT ROWID / strftime / ATTACH
	sqliteHints = []*regexp.Regexp{
		regexp.MustCompile(`(?im)^\s*PRAGMA\s+\w`),
		regexp.MustCompile(`(?i)\bAUTOINCREMENT\b`),
		regexp.MustCompile(`(?i)\bWITHOUT\s+ROWID\b`),
		regexp.MustCompile(`(?i)\bstrftime\s*\(`),
		regexp.MustCompile(`(?i)\bATTACH\s+DATABASE\b`),
		regexp.MustCompile(`(?i)\bjson_extract\s*\(`),
		regexp.MustCompile(`(?i)\bINSERT\s+OR\s+(REPLACE|IGNORE|ABORT|FAIL|ROLLBACK)\b`),
	}
	// PostgreSQL 特征：美元引号 / :: 强转 / JSONB / ILIKE / ON CONFLICT / RETURNING 等。
	postgresHints = []*regexp.Regexp{
		regexp.MustCompile(`\$[A-Za-z_0-9]*\$`),
		regexp.MustCompile(`(?i)\bBIGSERIAL\b|\bSMALLSERIAL\b|\bSERIAL[248]?\b`),
		regexp.MustCompile(`(?i)\bJSONB\b`),
		regexp.MustCompile(`(?i)\bILIKE\b`),
		regexp.MustCompile(`(?i)\bON\s+CONFLICT\b`),
		regexp.MustCompile(`(?i)\bRETURNING\b`),
		regexp.MustCompile(`(?i)\bDISTINCT\s+ON\b`),
		regexp.MustCompile(`(?i)\bCREATE\s+(?:OR\s+REPLACE\s+)?(?:FUNCTION|PROCEDURE|TRIGGER)\b`),
		regexp.MustCompile(`(?i)\bGENERATED\s+(?:ALWAYS|BY\s+DEFAULT)\s+AS\s+IDENTITY\b`),
		regexp.MustCompile(`(?i)\bTIMESTAMPTZ\b|\bTIMETZ\b|\bTO_REGCLASS\b`),
		regexp.MustCompile(`::`),
	}
)

// DetectDialect 猜方言：特征计数多者胜；都判不出来按 MySQL（覆盖面更广）。
// 文件名/目录名可给出明确提示，例如 deploy/sqlite/sql/、deploy/sqlserver/sql/
// 与 migrations/postgres/。
func DetectDialect(sql, file string) Dialect {
	lowerFile := strings.ToLower(file)
	if strings.Contains(lowerFile, "sqlite") {
		return DialectSQLite
	}
	for _, hint := range []string{"sqlserver", "sql-server", "mssql", "tsql"} {
		if strings.Contains(lowerFile, hint) {
			return DialectSQLServer
		}
	}
	for _, hint := range []string{"postgres", "postgresql", "pgsql"} {
		if strings.Contains(lowerFile, hint) {
			return DialectPostgreSQL
		}
	}
	m, s, q, p := 0, 0, 0, 0
	for _, re := range mysqlHints {
		if re.MatchString(sql) {
			m++
		}
	}
	for _, re := range sqliteHints {
		if re.MatchString(sql) {
			s++
		}
	}
	for _, re := range sqlServerHints {
		if re.MatchString(sql) {
			q++
		}
	}
	for _, re := range postgresHints {
		if re.MatchString(sql) {
			p++
		}
	}
	// 四种特征同时为零时保持历史默认 MySQL。
	if m == 0 && s == 0 && q == 0 && p == 0 {
		return DialectMySQL
	}
	if p > 0 && p >= q && p >= s && p >= m {
		return DialectPostgreSQL
	}
	if q > 0 && q >= s && q >= p && q >= m {
		return DialectSQLServer
	}
	if s > 0 && s >= m && s >= p && s >= q {
		return DialectSQLite
	}
	return DialectMySQL
}

// resolveDialect 把「显式指定 > 自动判定」收敛成最终方言。
func resolveDialect(explicit Dialect, sql, file string) Dialect {
	if explicit != "" {
		return explicit
	}
	return DetectDialect(sql, file)
}

// ---------------------------------------------------------------- SQLite 检查

var (
	// SQLite 的 ALTER TABLE 只支持 RENAME TABLE / RENAME COLUMN / ADD COLUMN / DROP COLUMN(3.35+)
	sqliteBadAlter = []string{
		"MODIFY COLUMN", "CHANGE COLUMN", "ALTER COLUMN", "RENAME INDEX",
		"ADD CONSTRAINT", "DROP FOREIGN KEY", "DROP PRIMARY KEY", "ALTER INDEX",
	}
	sqliteBadStmt = []string{"TRUNCATE", "CREATE USER", "GRANT", "REVOKE", "PREPARE", "DELIMITER"}

	// 这些类型名在 SQLite 里要么无效要么只是「亲和性名字」，翻译成 TEXT/INTEGER 才算到位
	sqliteLooseTypes = []string{
		"VARCHAR", "CHAR(", "NVARCHAR", "DECIMAL", "NUMERIC(", "ENUM(", "SET(",
		"JSON", "BOOLEAN", "BOOL", "DATETIME", "TIMESTAMP", "DATE", "TIME",
		"DOUBLE", "FLOAT", "TINYINT", "SMALLINT", "MEDIUMINT", "BIGINT",
		"BLOB(", "VARBINARY",
	}
)

// checkSQLiteStatement 检查 SQLite 轨道下语句级的问题。
func (a *analyzer) checkSQLiteStatement(s *stmtInfo) {
	up := " " + strings.ToUpper(s.Raw) + " "

	switch s.Kind {
	case "USE":
		a.add("SQL061", s.Line, s.Index,
			"SQLite 没有 USE 语句，执行会直接报语法错误",
			"去掉 USE：SQLite 的库就是映射的文件，由连接串里的路径决定。",
			nil, s.Dynamic)
	case "VACUUM":
		a.add("SQL064", s.Line, s.Index,
			"VACUUM 不能在事务里执行",
			"多数迁移工具会给每个迁移套事务；把 VACUUM 挪到迁移之外，或拆成非事务脚本。",
			nil, s.Dynamic)
	case "ATTACH":
		a.add("SQL066", s.Line, s.Index,
			"ATTACH DATABASE 引入了第二个库文件",
			"确认路径为绝对路径且两端存在；迁移脚本里跨库操作容易在 CI 环境失效。",
			nil, s.Dynamic)
	}

	for _, bad := range sqliteBadAlter {
		if strings.Contains(up, " "+bad+" ") || strings.Contains(up, " "+bad+",") {
			a.add("SQL060", s.Line, s.Index,
				fmt.Sprintf("SQLite 不支持 `%s`", titleCase(bad)),
				"SQLite 只支持 RENAME TABLE / RENAME COLUMN / ADD COLUMN / DROP COLUMN(3.35+)；其余改动走「建新表 → 搬数据 → 换名」的 12 步重建。",
				map[string]any{"clause": bad}, s.Dynamic)
			break
		}
	}
	for _, bad := range sqliteBadStmt {
		if strings.HasPrefix(strings.TrimSpace(up), " "+bad+" ") {
			a.add("SQL060", s.Line, s.Index,
				fmt.Sprintf("SQLite 不支持 `%s` 语句", bad),
				"SQLite 没有实例级账号/权限/存储引擎概念；这类变更不进迁移脚本。",
				map[string]any{"statement": bad}, s.Dynamic)
			break
		}
	}

	if s.Kind == "CREATE" && sqliteAutoincRe.MatchString(s.Raw) {
		tbl := ""
		if len(s.Tables) > 0 {
			tbl = bareTable(s.Tables[0])
		}
		a.add("SQL062", s.Line, s.Index,
			fmt.Sprintf("%s 使用了 AUTOINCREMENT", tbl),
			"INTEGER PRIMARY KEY 已是 rowid 别名、自动递增；AUTOINCREMENT 会多建 sqlite_sequence 且禁止复用 id。",
			map[string]any{"table": tbl}, s.Dynamic)
	}
}

// checkSQLiteColumn 检查列定义是否符合 SQLite 方言映射（JSON→TEXT、TINYINT(1)→INTEGER 等）。
func (a *analyzer) checkSQLiteColumn(table string, c colDef, line, idx int, dynamic bool) {
	typ := strings.ToUpper(strings.TrimSpace(c.Type))
	for _, bad := range sqliteLooseTypes {
		if strings.HasPrefix(typ, bad) {
			a.add("SQL063", line, idx,
				fmt.Sprintf("%s.%s 用了 %s：SQLite 只按亲和性存储，类型名不产生约束", table, c.Name, c.Type),
				"按映射规则改写：JSON/DATETIME/CHAR(n)/VARCHAR(n) → TEXT，TINYINT(1)/BOOLEAN → INTEGER，金额 → INTEGER(分) 或 TEXT。",
				map[string]any{"table": table, "column": c.Name, "type": c.Type}, dynamic)
			break
		}
	}
}

var sqliteAutoincRe = regexp.MustCompile(`(?i)\bAUTOINCREMENT\b`)

var sqliteRebuildName = regexp.MustCompile(`(?is)ALTER\s+TABLE\s+[` + "`" + `"\[` + "`" + `]?([\w.]+)[` + "`" + `"\]` + "`" + `]?\s+RENAME\s+TO\s+[` + "`" + `"\[` + "`" + `]?([\w.]+)`)

// adjustSQLite 在语句分析完之后做一次「按上下文降噪」：
// SQLite 里改列类型/约束的唯一办法是 12 步表重建（建新表→搬数据→DROP 旧表→RENAME），
// 这种 DROP 不该按「破坏性删表」报 critical。
func (a *analyzer) adjustSQLite() {
	if a.dialect != DialectSQLite {
		return
	}
	rebuilt := map[string]bool{}
	for _, s := range a.stmts {
		if s.Kind != "ALTER" {
			continue
		}
		if m := sqliteRebuildName.FindStringSubmatch(s.Raw); m != nil {
			rebuilt[strings.ToLower(m[2])] = true
		}
	}
	if len(rebuilt) == 0 {
		return
	}
	for i := range a.findings {
		f := &a.findings[i]
		if f.RuleID != "SQL001" {
			continue
		}
		tables, _ := f.Evidence["tables"].([]string)
		for _, t := range tables {
			if !rebuilt[strings.ToLower(t)] {
				continue
			}
			f.Severity = "medium"
			f.Message += "（属表重建：同文件里已 RENAME 回原名）"
			f.Advice = "12 步重建的 DROP 是必要步骤；确认搬数据的 INSERT…SELECT 已在上一步执行、且 RENAME 紧随其后。"
			a.add("SQL065", f.Line, f.StmtIndex,
				fmt.Sprintf("检测到表重建模式（%s）", t),
				"SQLite 改约束需重建表；确认顺序为 建新表 → 搬数据 → DROP 旧表 → RENAME → 重建索引/触发器。",
				map[string]any{"table": t}, f.Dynamic)
			break
		}
	}
}

var (
	dropCheckRe = regexp.MustCompile(`(?is)DROP\s+(?:CHECK|CONSTRAINT)\s+[` + "`" + `"]?(\w+)`)
	addConsRe   = regexp.MustCompile(`(?is)ADD\s+CONSTRAINT\s+[` + "`" + `"]?(\w+)`)
)

// adjustConstraintSwap 处理 MySQL 的「DROP CHECK x; 紧接着 ADD CONSTRAINT x CHECK (更宽的条件)」：
// 这是放宽约束的常规写法（同文件里重建同名约束），不该按「删约束」报 high。
func (a *analyzer) adjustConstraintSwap() {
	if a.dialect != DialectMySQL {
		return
	}
	readded := map[string]bool{}
	for _, s := range a.stmts {
		for _, m := range addConsRe.FindAllStringSubmatch(s.Raw, -1) {
			readded[strings.ToLower(m[1])] = true
		}
	}
	if len(readded) == 0 {
		return
	}
	for i := range a.findings {
		f := &a.findings[i]
		if f.RuleID != "SQL005" || !strings.Contains(f.Message, "删除约束") {
			continue
		}
		name, _ := f.Evidence["constraint"].(string)
		if name == "" || !readded[strings.ToLower(name)] {
			continue
		}
		f.Severity = "medium"
		f.Message = fmt.Sprintf("删除并重建同名约束 %s（放宽/收紧定义）", name)
		f.Advice = "确认新条件覆盖存量数据（否则 ADD CONSTRAINT 会失败），并在低峰执行。"
	}
}

func titleCase(s string) string {
	parts := strings.Fields(strings.ToLower(s))
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

// joinDialects 把批次里出现过的方言拼成一个展示值（全同时就是该方言，混合则 mixed）。
func joinDialects(set map[Dialect]bool) string {
	if len(set) == 0 {
		return ""
	}
	if len(set) == 1 {
		for d := range set {
			return string(d)
		}
	}
	return "mixed"
}
