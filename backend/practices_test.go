package main

import (
	"testing"
)

func TestPracticeCatalogIntegrity(t *testing.T) {
	if len(practiceCatalog) < 40 {
		t.Fatalf("最佳实践条目过少: %d", len(practiceCatalog))
	}
	seen := map[string]bool{}
	for _, p := range practiceCatalog {
		if p.ID == "" || p.Title == "" || p.Category == "" {
			t.Fatalf("条目字段不完整: %+v", p)
		}
		if seen[p.ID] {
			t.Fatalf("重复 ID: %s", p.ID)
		}
		seen[p.ID] = true
		if practiceCategoryLabels[p.Category] == "" {
			t.Fatalf("未知分类 %s (条目 %s)", p.Category, p.ID)
		}
		for _, d := range p.Dialects {
			if NormalizeDialect(d) == "" {
				t.Fatalf("条目 %s 含未知方言 %s", p.ID, d)
			}
		}
		for _, rid := range p.RuleIDs {
			if _, ok := ruleIndex[rid]; !ok {
				t.Fatalf("条目 %s 关联了不存在的规则 %s", p.ID, rid)
			}
		}
	}
}

func TestPracticesDialectFilter(t *testing.T) {
	all := Practices("", "")
	if len(all) != len(practiceCatalog) {
		t.Fatalf("全方言过滤应返回全部: got %d want %d", len(all), len(practiceCatalog))
	}
	for _, d := range []Dialect{DialectMySQL, DialectPostgreSQL, DialectSQLServer, DialectSQLite} {
		list := Practices(d, "")
		if len(list) == 0 {
			t.Fatalf("方言 %s 无最佳实践", d)
		}
		// 每个方言都必须有专属条目（Dialects 精确命中），而非只有通用条目。
		specific := 0
		for _, p := range list {
			if len(p.Dialects) > 0 {
				specific++
			}
		}
		if specific < 5 {
			t.Fatalf("方言 %s 专属条目过少: %d", d, specific)
		}
		for _, p := range list {
			if !p.appliesTo(d) {
				t.Fatalf("过滤结果含不适用条目 %s (方言 %s)", p.ID, d)
			}
		}
	}
	// 分类过滤
	idx := Practices(DialectMySQL, bpIndex)
	if len(idx) == 0 {
		t.Fatal("MySQL 索引分类为空")
	}
	for _, p := range idx {
		if p.Category != bpIndex {
			t.Fatalf("分类过滤混入 %s", p.Category)
		}
	}
}

func TestSuggestPracticesByFindings(t *testing.T) {
	// SELECT * 应推荐 BP-GEN-006（关联 SQL022）
	res := analyze(t, "SELECT * FROM users WHERE status=1;", AnalyzeOptions{Dialect: DialectMySQL, NoUseRequired: true})
	res.withPractices()
	if len(res.SuggestedPractices) == 0 {
		t.Fatal("命中规则后未给出最佳实践建议")
	}
	found := false
	for _, p := range res.SuggestedPractices {
		if p.ID == "BP-GEN-006" {
			found = true
		}
	}
	if !found {
		t.Fatalf("SELECT * 未推荐覆盖索引条目，实际: %+v", res.SuggestedPractices)
	}

	// 无命中时不应有建议
	clean := analyze(t, "SELECT id FROM users WHERE id=1;", AnalyzeOptions{Dialect: DialectMySQL, NoUseRequired: true})
	clean.withPractices()
	if len(clean.SuggestedPractices) != 0 {
		t.Fatalf("无命中时不应有建议: %d", len(clean.SuggestedPractices))
	}

	// 建议条目的方言必须适配
	pg := analyze(t, "SELECT * FROM users;", AnalyzeOptions{Dialect: DialectPostgreSQL, NoUseRequired: true})
	for _, p := range SuggestPractices(pg.Findings, DialectPostgreSQL) {
		if !p.appliesTo(DialectPostgreSQL) {
			t.Fatalf("PG 建议含不适用条目 %s", p.ID)
		}
	}
}

func TestMcpBestPracticesTool(t *testing.T) {
	p := &plugin{reviews: newReviewStore()}
	// 目录模式
	catalog, err := p.dispatchTool("sql_best_practices", map[string]any{"dialect": "postgres"})
	if err != nil {
		t.Fatalf("目录查询失败: %v", err)
	}
	m := catalog.(map[string]any)
	if m["mode"] != "catalog" || m["total"].(int) == 0 {
		t.Fatalf("目录模式返回异常: %+v", m)
	}
	// 建议模式
	suggest, err := p.dispatchTool("sql_best_practices", map[string]any{
		"sql": "SELECT * FROM users;", "dialect": "mysql",
	})
	if err != nil {
		t.Fatalf("建议查询失败: %v", err)
	}
	s := suggest.(map[string]any)
	if s["mode"] != "suggest" {
		t.Fatalf("建议模式返回异常: %+v", s)
	}
}

func TestCollectRuleIDs(t *testing.T) {
	fs := []Finding{{RuleID: "SQL022"}, {RuleID: "SQL003"}, {RuleID: "SQL022"}}
	got := collectRuleIDs(fs)
	if len(got) != 2 || got[0] != "SQL003" || got[1] != "SQL022" {
		t.Fatalf("collectRuleIDs 结果错误: %v", got)
	}
}
