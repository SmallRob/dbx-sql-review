package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sqlServerResult(t *testing.T, sqlText string, migration bool) *Result {
	t.Helper()
	cfg := defaultConfig().Rules
	return AnalyzeSQL(sqlText, "sqlserver/inline.sql", cfg,
		AnalyzeOptions{Migration: migration, Dialect: DialectSQLServer, NoUseRequired: true})
}

func TestSQLServerDialectDetectionAndLexing(t *testing.T) {
	sqlText := `SET NOCOUNT ON;
GO
CREATE TABLE [dbo].[AuditLog] (
  [Id] BIGINT IDENTITY(1,1) NOT NULL PRIMARY KEY,
  [Note] NVARCHAR(200) NULL,
  #LocalCache INT
);
GO
INSERT INTO [dbo].[AuditLog] ([Note]) VALUES (N'中文');
`
	if got := DetectDialect(sqlText, "deploy/sqlserver/V1__init.sql"); got != DialectSQLServer {
		t.Fatalf("方言识别期望 sqlserver，实际 %s", got)
	}
	stmts := splitStatementsDialect(sqlText, DialectSQLServer)
	if len(stmts) != 3 {
		t.Fatalf("GO 应分成 3 批，实际 %d: %#v", len(stmts), stmts)
	}
	toks := tokenizeDialect(`SELECT [Order Id] FROM [#tmp];`, DialectSQLServer)
	foundBracket, foundTemp := false, false
	for _, tok := range toks {
		if tok.Text == "Order Id" {
			foundBracket = true
		}
		if tok.Text == "#tmp" {
			foundTemp = true
		}
	}
	if !foundBracket || !foundTemp {
		t.Fatalf("T-SQL 方括号/临时表解析失败: %#v", toks)
	}
}

func TestSQLServerRules071To077(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		rule string
	}{
		{"nolock", "SELECT [Id] FROM [dbo].[users] WITH (NOLOCK);", "SQL071"},
		{"top no order", "SELECT TOP (10) [Id] FROM [dbo].[users];", "SQL072"},
		{"dynamic exec", "EXEC sp_executesql N'SELECT 1';", "SQL073"},
		{"select into", "SELECT [Id] INTO [dbo].[users_copy] FROM [dbo].[users];", "SQL074"},
		{"non unicode", "CREATE TABLE [dbo].[users] ([Id] INT PRIMARY KEY, [Name] VARCHAR(100));", "SQL075"},
		{"max key", "CREATE TABLE [dbo].[docs] ([Id] INT, [Payload] NVARCHAR(MAX) PRIMARY KEY);", "SQL076"},
		{"merge no holdlock", "MERGE INTO [dbo].[t] AS tgt USING [dbo].[src] AS src ON tgt.id = src.id WHEN MATCHED THEN UPDATE SET tgt.x = src.x;", "SQL077"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := sqlServerResult(t, c.sql, false)
			f := findRule(res, c.rule)
			if f == nil {
				t.Fatalf("期望命中 %s，实际 %v", c.rule, ruleIDs(res))
			}
			if f.Snippet == "" {
				t.Fatalf("%s 应带脱敏片段", c.rule)
			}
		})
	}
}

func TestSQLServerRule070Optional(t *testing.T) {
	cfg := defaultConfig().Rules
	var kept []string
	for _, id := range cfg.Disabled {
		if id != "SQL070" {
			kept = append(kept, id)
		}
	}
	cfg.Disabled = kept
	res := AnalyzeSQL("CREATE TABLE [dbo].[users] ([Id] INT PRIMARY KEY);\nGO\n",
		"migration/V1__users.sql", cfg,
		AnalyzeOptions{Migration: true, Dialect: DialectSQLServer, NoUseRequired: true})
	if findRule(res, "SQL070") == nil {
		t.Fatalf("开启 SQL070 后应命中非幂等 DDL，实际 %v", ruleIDs(res))
	}
	if findRule(res, "SQL012") != nil {
		t.Fatal("SQL Server 不应误用 MySQL 的 SQL012")
	}
}

func TestSQLServerGuardedDDL(t *testing.T) {
	res := sqlServerResult(t, `IF OBJECT_ID(N'dbo.users', N'U') IS NULL
CREATE TABLE [dbo].[users] ([Id] INT PRIMARY KEY);
GO`, true)
	if findRule(res, "SQL070") != nil {
		t.Fatalf("OBJECT_ID 守卫已存在，不应命中 SQL070: %v", ruleIDs(res))
	}
}

func TestRuleCatalogTotalAndManifestTools(t *testing.T) {
	if got := len(Rules(defaultConfig().Rules)); got != 53 {
		t.Fatalf("规则总数应为 53，实际 %d", got)
	}
	raw, err := os.ReadFile(filepath.Join("..", "manifest.json"))
	if err != nil {
		t.Fatalf("读取 manifest 失败: %v", err)
	}
	var manifest struct {
		Version       string `json:"version"`
		Contributions []struct {
			Type  string `json:"type"`
			Tools map[string]struct {
				Access string `json:"access"`
			} `json:"tools"`
		} `json:"contributions"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("manifest JSON 解析失败: %v", err)
	}
	if manifest.Version != pluginVersion {
		t.Fatalf("manifest 版本 %s != pluginVersion %s", manifest.Version, pluginVersion)
	}
	for _, c := range manifest.Contributions {
		if c.Type != "mcp" {
			continue
		}
		if len(c.Tools) != len(mcpTools()) {
			t.Fatalf("manifest 工具数 %d != mcpTools %d", len(c.Tools), len(mcpTools()))
		}
		for name, spec := range c.Tools {
			if spec.Access != "read" {
				t.Fatalf("%s 必须只读，实际 %s", name, spec.Access)
			}
			if _, ok := findMcpTool(name); !ok {
				t.Fatalf("manifest 声明的工具 %s 未在 mcpTools 中实现", name)
			}
		}
	}
}

func TestReviewResultAndOptimizeTools(t *testing.T) {
	p := &plugin{reviews: newReviewStore()}
	review, err := p.dispatchTool("sql_review", map[string]any{
		"sql": "SELECT [Id] FROM [dbo].[users] WITH (NOLOCK);", "dialect": "sqlserver", "migration": true,
	})
	if err != nil {
		t.Fatalf("sql_review: %v", err)
	}
	m := review.(map[string]any)
	id, _ := m["reviewId"].(string)
	if id == "" {
		t.Fatal("sql_review 应返回 reviewId")
	}
	out, err := p.dispatchTool("sql_review_result", map[string]any{
		"reviewId": id, "severity": "medium", "ruleId": "SQL071", "limit": float64(10),
	})
	if err != nil {
		t.Fatalf("sql_review_result: %v", err)
	}
	result := out.(map[string]any)
	if _, leaked := result["sql"]; leaked {
		t.Fatal("评审结果不得返回原始 SQL")
	}
	if result["total"] != float64(1) && result["total"] != 1 {
		t.Fatalf("按规则过滤后应为 1 条，实际 %v", result["total"])
	}

	opt, err := p.dispatchTool("sql_optimize", map[string]any{
		"sql": "SELECT [Id] FROM [dbo].[users] WITH (NOLOCK);", "dialect": "sqlserver",
	})
	if err != nil {
		t.Fatalf("sql_optimize: %v", err)
	}
	om := opt.(map[string]any)
	if om["changed"] != false || om["optimizedSql"] == "" {
		t.Fatalf("optimize 必须 changed=false 且保留原 SQL: %#v", om)
	}
	if items, ok := om["optimizations"].([]optimizationItem); !ok || len(items) == 0 {
		t.Fatalf("optimize 应返回结构化建议: %#v", om["optimizations"])
	}
}

func ruleIDs(res *Result) string {
	ids := make([]string, 0, len(res.Findings))
	for _, f := range res.Findings {
		ids = append(ids, f.RuleID)
	}
	return strings.Join(ids, ",")
}
