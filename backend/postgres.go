package main

import (
	"fmt"
	"strings"
)

// checkPostgresStatement 检查 PostgreSQL 专属迁移风险；只读词法，不连接数据库。
func (a *analyzer) checkPostgresStatement(s *stmtInfo) {
	u := upperTokens(s.Tokens)

	if a.opts.Migration && pgDDLNeedsGuard(s, u) {
		a.nonIdemLn = append(a.nonIdemLn, s.Line)
	}

	if s.Kind == "CREATE" && s.Obj == "INDEX" && !strings.Contains(u, " CONCURRENTLY ") {
		tbl := indexTargetTable(s.Tokens)
		a.add("SQL081", s.Line, s.Index,
			fmt.Sprintf("CREATE INDEX 未使用 CONCURRENTLY：%s", tbl),
			"生产迁移使用 CREATE INDEX CONCURRENTLY；它不能在事务块内执行，失败后检查并重建 INVALID 索引。",
			map[string]any{"table": tbl}, s.Dynamic)
	}

	if s.Kind == "ALTER" && strings.Contains(u, " ALTER COLUMN ") &&
		(strings.Contains(u, " TYPE ") || strings.Contains(u, " SET DATA TYPE ")) {
		tbl := ""
		if len(s.Tables) > 0 {
			tbl = bareTable(s.Tables[0])
		}
		a.add("SQL082", s.Line, s.Index,
			"ALTER COLUMN TYPE 通常会重写整表并持有强锁："+tbl,
			"大表使用「新增兼容列 → 双写/回填 → 切换读 → 删除旧列」；原地改类型需安排维护窗口。",
			map[string]any{"table": tbl}, s.Dynamic)
	}

	if s.Kind == "ALTER" && strings.Contains(u, " SET NOT NULL ") && !strings.Contains(u, " CHECK ") {
		tbl := ""
		if len(s.Tables) > 0 {
			tbl = bareTable(s.Tables[0])
		}
		a.add("SQL083", s.Line, s.Index,
			"SET NOT NULL 前缺少 CHECK 约束校验："+tbl,
			"先 ADD CONSTRAINT ... CHECK (col IS NOT NULL) NOT VALID，VALIDATE 后再 SET NOT NULL 并删除临时 CHECK。",
			map[string]any{"table": tbl}, s.Dynamic)
	}

	if (s.Kind == "DROP" || s.Kind == "TRUNCATE") && strings.Contains(u, " CASCADE ") {
		a.add("SQL087", s.Line, s.Index,
			s.Kind+" ... CASCADE 会递归影响依赖对象",
			"先查询 pg_depend/依赖对象，列出完整影响清单；迁移脚本原则上避免 CASCADE。",
			map[string]any{"statement": s.Kind}, s.Dynamic)
	}
}

// pgDDLNeedsGuard 只判断单条 DDL 自身的重放守卫，避免一条脚本里的
// IF NOT EXISTS 掩盖其它裸 DDL。
func pgDDLNeedsGuard(s *stmtInfo, u string) bool {
	switch s.Kind {
	case "DROP":
		return !strings.Contains(u, " IF EXISTS ")
	case "ALTER":
		if strings.Contains(u, " IF NOT EXISTS ") ||
			strings.Contains(u, " DROP COLUMN IF EXISTS ") ||
			strings.Contains(u, " DROP CONSTRAINT IF EXISTS ") {
			return false
		}
		return true
	case "CREATE":
		if strings.Contains(u, " OR REPLACE ") || strings.Contains(u, " IF NOT EXISTS ") {
			return false
		}
		switch s.Obj {
		case "TABLE", "INDEX", "SCHEMA", "EXTENSION", "VIEW", "SEQUENCE", "TYPE":
			return true
		}
	}
	return false
}

// checkPostgresColumn 检查 PostgreSQL 列类型约定。
func (a *analyzer) checkPostgresColumn(table string, c colDef, line, idx int, dynamic bool) {
	typ := strings.ToLower(strings.TrimSpace(c.Type))
	base := typ
	if i := strings.IndexAny(base, "( "); i >= 0 {
		base = base[:i]
	}

	switch base {
	case "serial", "bigserial", "smallserial", "serial2", "serial4", "serial8":
		a.add("SQL084", line, idx,
			fmt.Sprintf("%s.%s 使用 %s 伪自增类型", table, c.Name, c.Type),
			"新表使用 GENERATED ALWAYS AS IDENTITY，避免隐式序列的权限与生命周期问题。",
			map[string]any{"table": table, "column": c.Name, "type": c.Type}, dynamic)
	}

	if base == "json" {
		a.add("SQL085", line, idx,
			fmt.Sprintf("%s.%s 使用 JSON 而非 JSONB", table, c.Name),
			"除必须逐字保留输入文本外统一使用 JSONB，并为高频查询键建表达式 GIN 索引。",
			map[string]any{"table": table, "column": c.Name, "type": c.Type}, dynamic)
	}

	if base == "timestamp" || base == "time" {
		if !strings.Contains(typ, "with time zone") {
			a.add("SQL086", line, idx,
				fmt.Sprintf("%s.%s 使用无时区类型 %s", table, c.Name, c.Type),
				"需要绝对时间点时使用 TIMESTAMPTZ 或 TIME WITH TIME ZONE；只有明确本地时刻才使用无时区类型。",
				map[string]any{"table": table, "column": c.Name, "type": c.Type}, dynamic)
		}
	}
}

// PostgreSQL 因历史原因允许 DOUBLE PRECISION 表示通用浮点；
// SQL032 只在列名明确是金额/价格时提示，避免把科学计算列误报为金额。
func pgMoneyColumn(name string) bool {
	n := strings.ToLower(name)
	for _, key := range []string{"amount", "price", "cost", "fee", "balance", "money", "金额", "价格", "费用"} {
		if strings.Contains(n, key) {
			return true
		}
	}
	return false
}
