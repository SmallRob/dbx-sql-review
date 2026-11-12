package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// findRule 检查一次评审结果里是否命中了某条规则。
func findRule(res *Result, ruleID string) *Finding {
	for i := range res.Findings {
		if res.Findings[i].RuleID == ruleID {
			return &res.Findings[i]
		}
	}
	return nil
}

func analyze(t *testing.T, sql string, opts AnalyzeOptions) *Result {
	t.Helper()
	cfg := defaultConfig().Rules
	return AnalyzeSQL(sql, "inline.sql", cfg, opts)
}

func TestDestructiveRules(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		rule string
	}{
		{"drop table", "DROP TABLE users;", "SQL001"},
		{"truncate", "TRUNCATE TABLE users;", "SQL002"},
		{"update without where", "UPDATE users SET name = 'x';", "SQL003"},
		{"delete without where", "DELETE FROM users;", "SQL003"},
		{"always true where", "DELETE FROM users WHERE 1=1;", "SQL004"},
		{"alter drop column", "ALTER TABLE users DROP COLUMN name;", "SQL005"},
		{"grant", "GRANT ALL ON *.* TO 'x'@'%';", "SQL006"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := analyze(t, c.sql, AnalyzeOptions{NoUseRequired: true})
			if findRule(res, c.rule) == nil {
				t.Fatalf("期望命中 %s，实际 findings=%v", c.rule, res.Findings)
			}
		})
	}
}

func TestPerformanceRules(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		rule string
	}{
		{"select star", "SELECT * FROM users;", "SQL022"},
		{"select no where", "SELECT id FROM users;", "SQL023"},
		{"leading wildcard", "SELECT id FROM users WHERE name LIKE '%x';", "SQL024"},
		{"function on column", "SELECT id FROM users WHERE DATE(created_at) = '2024-01-01';", "SQL025"},
		{"not in", "SELECT id FROM users WHERE id NOT IN (SELECT uid FROM orders);", "SQL026"},
		{"implicit join", "SELECT a.id FROM a, b WHERE a.x = 1;", "SQL027"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := analyze(t, c.sql, AnalyzeOptions{NoUseRequired: true})
			if findRule(res, c.rule) == nil {
				t.Fatalf("期望命中 %s，实际 findings=%v", c.rule, res.Findings)
			}
		})
	}
}

func TestSchemaRules(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		rule string
	}{
		{"no primary key", "CREATE TABLE users (id BIGINT, name VARCHAR(64));", "SQL030"},
		{"charset", "CREATE TABLE users (id BIGINT PRIMARY KEY) DEFAULT CHARSET=latin1;", "SQL031"},
		{"float money", "CREATE TABLE users (id BIGINT PRIMARY KEY, price FLOAT);", "SQL032"},
		{"varchar pk", "CREATE TABLE users (id VARCHAR(255) PRIMARY KEY);", "SQL036"},
		{"insert no columns", "INSERT INTO users VALUES (1, 'a');", "SQL038"},
		{"uppercase column", "CREATE TABLE users (UserId BIGINT PRIMARY KEY);", "SQL035"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := analyze(t, c.sql, AnalyzeOptions{NoUseRequired: true})
			if findRule(res, c.rule) == nil {
				t.Fatalf("期望命中 %s，实际 findings=%v", c.rule, res.Findings)
			}
		})
	}
}

// TestOptionalRulesDisabledByDefault 验证「项目约定类」规则默认不参与判定。
//
// 这是通用版与内部版最重要的差别：通用项目不该被「必须写 USE」「必须有 COMMENT」
// 这类团队房规打扰。
func TestOptionalRulesDisabledByDefault(t *testing.T) {
	res := analyze(t, "CREATE TABLE users (id BIGINT PRIMARY KEY, name VARCHAR(64));",
		AnalyzeOptions{NoUseRequired: true, Migration: true})
	if f := findRule(res, "SQL033"); f != nil {
		t.Fatalf("SQL033（列 COMMENT）是房规类规则，默认不应命中，实际命中：%v", f)
	}
	if f := findRule(res, "SQL034"); f != nil {
		t.Fatalf("SQL034（显式 ENGINE）是房规类规则，默认不应命中，实际命中：%v", f)
	}
}

// TestOptionalRulesEnabledWhenOptIn 验证用户在规则面板开启后房规规则立即生效。
//
// 真实语义：用户在 UI 勾选某条房规规则，前端把它从 Disabled 列表里移除，
// 保存后该规则就参与判定。测试按同样方式构造配置。
func TestOptionalRulesEnabledWhenOptIn(t *testing.T) {
	cfg := defaultConfig().Rules
	// 模拟用户在规则面板勾选了 SQL033 与 SQL034。
	var kept []string
	for _, id := range cfg.Disabled {
		if id != "SQL033" && id != "SQL034" {
			kept = append(kept, id)
		}
	}
	cfg.Disabled = kept

	res := AnalyzeSQL("CREATE TABLE users (id BIGINT PRIMARY KEY, name VARCHAR(64));",
		"inline.sql", cfg, AnalyzeOptions{NoUseRequired: true, Migration: true})
	if findRule(res, "SQL033") == nil {
		t.Fatal("用户开启 SQL033 后应命中（列缺 COMMENT）")
	}
	if findRule(res, "SQL034") == nil {
		t.Fatal("用户开启 SQL034 后应命中（未显式 ENGINE）")
	}
}

func TestSensitiveColumn(t *testing.T) {
	res := analyze(t, "CREATE TABLE users (id BIGINT PRIMARY KEY, password VARCHAR(128));",
		AnalyzeOptions{NoUseRequired: true})
	if findRule(res, "SQL040") == nil {
		t.Fatal("期望命中 SQL040（敏感列）")
	}
}

// TestTokenCountNotSensitive 验证「数量类 token 列」不误报为敏感列。
func TestTokenCountNotSensitive(t *testing.T) {
	res := analyze(t, "CREATE TABLE providers (id BIGINT PRIMARY KEY, max_tokens BIGINT);",
		AnalyzeOptions{NoUseRequired: true})
	if findRule(res, "SQL040") != nil {
		t.Fatal("max_tokens 是配额列，不应报敏感列")
	}
}

func TestSQLiteDialect(t *testing.T) {
	res := analyze(t, "USE app;\nCREATE TABLE t (id INTEGER PRIMARY KEY AUTOINCREMENT);",
		AnalyzeOptions{NoUseRequired: true, Dialect: DialectSQLite})
	if findRule(res, "SQL061") == nil {
		t.Fatal("SQLite 轨道应命中 SQL061（出现 USE）")
	}
	if findRule(res, "SQL062") == nil {
		t.Fatal("SQLite 轨道应命中 SQL062（AUTOINCREMENT）")
	}
}

func TestDialectDetection(t *testing.T) {
	if got := DetectDialect("CREATE TABLE t (id INT) ENGINE=InnoDB;", "a.sql"); got != DialectMySQL {
		t.Fatalf("期望 mysql，实际 %s", got)
	}
	if got := DetectDialect("PRAGMA foreign_keys=ON;", "b.sql"); got != DialectSQLite {
		t.Fatalf("期望 sqlite，实际 %s", got)
	}
}

func TestSeverityOverride(t *testing.T) {
	cfg := defaultConfig().Rules
	cfg.Severity = map[string]string{"SQL003": "low"}
	res := AnalyzeSQL("UPDATE users SET name='x';", "inline.sql", cfg, AnalyzeOptions{NoUseRequired: true})
	f := findRule(res, "SQL003")
	if f == nil {
		t.Fatal("期望命中 SQL003")
	}
	if f.Severity != "low" {
		t.Fatalf("级别覆盖未生效，期望 low，实际 %s", f.Severity)
	}
}

func TestDisabledRule(t *testing.T) {
	cfg := defaultConfig().Rules
	cfg.Disabled = []string{"SQL003"}
	res := AnalyzeSQL("UPDATE users SET name='x';", "inline.sql", cfg, AnalyzeOptions{NoUseRequired: true})
	if findRule(res, "SQL003") != nil {
		t.Fatal("被禁用的规则不应命中")
	}
}

// TestNoDatabaseDependency 是这个插件面向市场的核心保证：
// 评审器绝不连库、绝不下发任何 SQL 到外部。
//
// 这里用「不提供任何连接配置也能完成评审」来证明——
// AnalyzeOptions 里根本没有 DB 字段，Config 里也没有任何连接信息。
func TestNoDatabaseDependency(t *testing.T) {
	cfg := defaultConfig()
	// 配置结构里不应存在任何连接相关字段。
	blob := fmt.Sprintf("%+v", cfg)
	for _, banned := range []string{"Host", "Password", "User", "Port", "DSN"} {
		if strings.Contains(blob, banned) {
			t.Fatalf("默认配置不应包含连接字段 %s，实际：%s", banned, blob)
		}
	}
	res := analyze(t, "SELECT * FROM users;", AnalyzeOptions{NoUseRequired: true})
	if res == nil || len(res.Findings) == 0 {
		t.Fatal("无连接配置时也必须能完成评审")
	}
}

// TestDefaultConfigDisablesOptionalRules 锁定「房规类规则默认关闭」这一核心设计。
//
// 回归风险：任何一处把 Disabled 置空数组，都会让通用项目被团队房规淹没
// （例如对每个建表都报「缺 COMMENT」「未写 USE」）。这条测试专门盯住它。
func TestDefaultConfigDisablesOptionalRules(t *testing.T) {
	cfg := defaultConfig()
	disabled := map[string]bool{}
	for _, id := range cfg.Rules.Disabled {
		disabled[id] = true
	}
	for _, r := range ruleCatalog {
		if !r.Optional {
			continue
		}
		if !disabled[r.ID] {
			t.Fatalf("房规规则 %s（%s）应默认关闭", r.ID, r.Title)
		}
	}
	if len(cfg.Rules.Disabled) == 0 {
		t.Fatal("默认配置应至少禁用全部 Optional 规则")
	}
}

// TestNormalizeRulesKeepsDisabled 验证保存配置时不会把房规规则意外打开。
func TestNormalizeRulesKeepsDisabled(t *testing.T) {
	fallback := defaultConfig().Rules
	// 模拟前端只回传 severity、没回传 disabled 的情况。
	rc := normalizeRules(RuleConfig{Severity: map[string]string{"SQL003": "low"}}, fallback)
	if len(rc.Disabled) != len(fallback.Disabled) {
		t.Fatalf("normalizeRules 不应丢掉已禁用的规则，期望 %d 条，实际 %d 条",
			len(fallback.Disabled), len(rc.Disabled))
	}
	for _, id := range fallback.Disabled {
		found := false
		for _, d := range rc.Disabled {
			if d == id {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("房规规则 %s 在保存后丢失了禁用状态", id)
		}
	}
}

// TestNoFilesystemPollution 验证没有数据目录时不会往当前工作目录写文件。
//
// 回归风险：早期实现在 DataDir 为空时退回到 "."，
// 会在用户项目目录里留下 config.json 与 audit.jsonl。
func TestNoFilesystemPollution(t *testing.T) {
	if dir := os.Getenv("DBX_PLUGIN_DATA_DIR"); dir != "" {
		t.Skip("测试环境提供了数据目录，跳过")
	}
	// 触发一次配置保存与一次审计写入。
	_ = saveConfig(defaultConfig())
	_ = appendAudit("review", "inline.sql", "pass", 0, 0, "tester")

	for _, name := range []string{"config.json", "audit.jsonl"} {
		if _, err := os.Stat(name); err == nil {
			t.Fatalf("不应在工作目录生成 %s", name)
		}
	}
}

func TestQualityMetrics(t *testing.T) {
	res := analyze(t, "DROP TABLE users;", AnalyzeOptions{NoUseRequired: true})
	if res.Metrics.Quality >= 100 {
		t.Fatalf("含 critical 命中时质量分不应为满分，实际 %d", res.Metrics.Quality)
	}
	if res.Metrics.Grade != "E" {
		t.Fatalf("含 critical 命中时等级应为 E，实际 %s", res.Metrics.Grade)
	}
}

func TestConsistencyFindings(t *testing.T) {
	locals := []localFile{
		{Path: "V1.0.0__a.sql", Version: "1.0.0", OK: true, Sum: 111},
		{Path: "V1.0.0__b.sql", Version: "1.0.0", OK: true, Sum: 222},
		{Path: "V1.0.5__c.sql", Version: "1.0.5", OK: true, Sum: 333},
		{Path: "V1.0.7__d.sql", Version: "1.0.7", OK: true, Sum: 111},
	}
	fs, summary := consistencyFindings(locals, defaultConfig().Rules)
	if findRuleIn(fs, "SQL050") == nil {
		t.Fatalf("应命中 SQL050（版本重复），实际 %v", fs)
	}
	if findRuleIn(fs, "SQL051") == nil {
		t.Fatalf("应命中 SQL051（版本缺口 1.0.0→1.0.5），实际 %v", fs)
	}
	if findRuleIn(fs, "SQL052") == nil {
		t.Fatalf("应命中 SQL052（内容雷同），实际 %v", fs)
	}
	if summary["namedVersions"] != 3 {
		t.Fatalf("版本数应为 3，实际 %v", summary["namedVersions"])
	}
}

func findRuleIn(list []Finding, ruleID string) *Finding {
	for i := range list {
		if list[i].RuleID == ruleID {
			return &list[i]
		}
	}
	return nil
}

func TestVersionParsing(t *testing.T) {
	v, d, ok := parseVersionedName("migrations/V2.3.41__add_user_index.sql")
	if !ok || v != "2.3.41" || d != "add user index" {
		t.Fatalf("解析结果不对: v=%q d=%q ok=%v", v, d, ok)
	}
	if _, _, ok := parseVersionedName("random.sql"); ok {
		t.Fatal("非版本化文件名不应解析成功")
	}
}

func TestFlywayChecksum(t *testing.T) {
	// 相同内容 → 相同校验和；内容不同 → 不同校验和。
	a := flywayChecksum([]byte("SELECT 1;\n"))
	b := flywayChecksum([]byte("SELECT 1;\n"))
	c := flywayChecksum([]byte("SELECT 2;\n"))
	if a != b {
		t.Fatal("相同内容校验和应相同")
	}
	if a == c {
		t.Fatal("不同内容校验和应不同")
	}
}
