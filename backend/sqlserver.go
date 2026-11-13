package main

import (
	"fmt"
	"strings"
)

// checkSQLServerStatement 检查 T-SQL 专属风险；只做词法分析，不连接数据库。
func (a *analyzer) checkSQLServerStatement(s *stmtInfo) {
	u := upperTokens(s.Tokens)
	if strings.Contains(u, " NOLOCK ") || strings.Contains(u, " READ UNCOMMITTED ") {
		a.add("SQL071", s.Line, s.Index,
			"使用 NOLOCK / READ UNCOMMITTED，可能读到未提交或不一致的数据",
			"移除 NOLOCK；若目标是降低阻塞，评估 RCSI/快照隔离并验证业务语义。",
			nil, s.Dynamic)
	}
	if hasTopLevel(s.Tokens, "TOP") && !hasTopLevel(s.Tokens, "ORDER") {
		a.add("SQL072", s.Line, s.Index,
			"TOP 未配 ORDER BY，返回行与分批边界不确定",
			"补唯一、稳定的 ORDER BY（通常使用主键），分批处理时记录上一批边界。",
			nil, s.Dynamic)
	}
	if s.Kind == "EXEC" || s.Kind == "EXECUTE" || strings.Contains(u, " SP_EXECUTESQL ") {
		a.add("SQL073", s.Line, s.Index,
			"EXEC/sp_executesql 动态语句存在静态审计盲区",
			"优先改成显式语句；值用 sp_executesql 参数化，标识符用 QUOTENAME，并评审所有动态分支。",
			nil, s.Dynamic)
	}
	if s.Kind == "SELECT" && hasTopLevel(s.Tokens, "INTO") {
		a.add("SQL074", s.Line, s.Index,
			"SELECT INTO 会隐式建表并批量写入",
			"迁移中优先显式 CREATE TABLE + INSERT (列清单) SELECT，并评估分批、锁与事务日志影响。",
			nil, s.Dynamic)
	}
	if s.Kind == "MERGE" && !strings.Contains(u, " HOLDLOCK ") {
		a.add("SQL077", s.Line, s.Index,
			"MERGE 未声明 HOLDLOCK，并发匹配与写入之间可能出现竞态",
			"评估显式 UPDATE/INSERT；保留 MERGE 时使用 WITH (HOLDLOCK)，并验证唯一约束与并发行为。",
			nil, s.Dynamic)
	}
	if a.opts.Migration && !a.guarded {
		switch s.Kind {
		case "CREATE", "ALTER", "DROP":
			a.nonIdemLn = append(a.nonIdemLn, s.Line)
		}
	}
}

func (a *analyzer) checkSQLServerColumn(table string, c colDef, line, idx int, dynamic bool) {
	typ := strings.ToUpper(strings.ReplaceAll(c.Type, " ", ""))
	if strings.HasPrefix(typ, "VARCHAR") || strings.HasPrefix(typ, "CHAR") || typ == "TEXT" {
		a.add("SQL075", line, idx,
			fmt.Sprintf("%s.%s 使用非 Unicode 类型 %s", table, c.Name, c.Type),
			"多语言文本改用 NVARCHAR/NCHAR；新表避免 TEXT，长文本使用 NVARCHAR(MAX)。",
			map[string]any{"table": table, "column": c.Name, "type": c.Type}, dynamic)
	}
	if (c.IsPK || c.Indexed) && strings.Contains(typ, "(MAX)") &&
		(strings.HasPrefix(typ, "VARCHAR") || strings.HasPrefix(typ, "NVARCHAR") || strings.HasPrefix(typ, "VARBINARY")) {
		a.add("SQL076", line, idx,
			fmt.Sprintf("%s.%s 以 %s 作为主键/索引键", table, c.Name, c.Type),
			"MAX 类型不能作为 SQL Server 索引键；改为有明确长度上限的键列，大文本仅作为包含列。",
			map[string]any{"table": table, "column": c.Name, "type": c.Type}, dynamic)
	}
}
