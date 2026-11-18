package main

import (
	"math"
	"strings"
	"testing"
)

// 复现用户场景：一份结构良好的迁移脚本，6 条 ALTER 未声明 ALGORITHM/LOCK，
// 2 条 ADD KEY 未声明 LOCK=NONE。修复前 6×20+2×8=136 → 风险分封顶 100 → 质量分 0，
// 且 gradeOf 把 0 分反向抬成 C「需改进」。
const advisoryMigrationSQL = `
USE asdm_admin;
CREATE TABLE IF NOT EXISTS aos_context_suite_binding (
  context_id VARCHAR(64) NOT NULL,
  binding_mode VARCHAR(16) NOT NULL,
  PRIMARY KEY (context_id),
  KEY idx_mode (binding_mode)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
ALTER TABLE aos_suite ADD COLUMN source VARCHAR(16) NOT NULL DEFAULT 'builtin';
ALTER TABLE aos_run ADD COLUMN binding_revision BIGINT NULL;
ALTER TABLE aos_attempt ADD COLUMN binding_revision BIGINT NULL;
ALTER TABLE aos_snapshot ADD COLUMN binding_revision BIGINT NULL;
ALTER TABLE aos_run ADD KEY idx_aos_run_binding (context_id, binding_revision);
ALTER TABLE aos_snapshot ADD KEY idx_aos_snapshot_binding (context_id, binding_revision);
`

func TestAdvisoryFindingsDoNotZeroQuality(t *testing.T) {
	res := analyze(t, advisoryMigrationSQL,
		AnalyzeOptions{Dialect: DialectMySQL, NoUseRequired: true})
	if len(res.Findings) == 0 {
		t.Fatal("样例应命中 SQL020/SQL021")
	}
	if res.Metrics.Quality <= 0 {
		t.Fatalf("纯提示类命中不应把质量分打到 0，实际 %d (score=%d)", res.Metrics.Quality, res.Score)
	}
	if res.Metrics.Quality < 50 {
		t.Fatalf("质量分过低: %d", res.Metrics.Quality)
	}
	// 0 分被反向抬成 C 是旧 bug；现在分数合理，等级应与分数一致。
	if res.Metrics.Grade == "C" && res.Metrics.Quality >= 72 {
		t.Fatalf("等级与分数不一致: grade=%s quality=%d", res.Metrics.Grade, res.Metrics.Quality)
	}
}

func TestScoreOfDiminishingReturns(t *testing.T) {
	high := func(id string) Finding { return Finding{RuleID: id, Severity: "high"} }
	// 同一规则重复 6 次：第 1 次 20，其后各 5，单规则封顶 2×20=40。
	same := []Finding{high("SQL020"), high("SQL020"), high("SQL020"),
		high("SQL020"), high("SQL020"), high("SQL020")}
	got, _ := scoreOf(same)
	if got != 40 {
		t.Fatalf("同规则递减计分应为 40，实际 %d", got)
	}
	// 6 条不同规则各一次：独立问题线性累加，6×20=120 → 封顶 100。
	var diff []Finding
	for i := 0; i < 6; i++ {
		diff = append(diff, high("R"+string(rune('A'+i))))
	}
	got, _ = scoreOf(diff)
	if got != 100 {
		t.Fatalf("不同规则应线性累加并封顶 100，实际 %d", got)
	}
}

func TestGradeOfCapDirection(t *testing.T) {
	cases := []struct {
		quality int
		level   string
		want    string
	}{
		{100, "high", "C"}, // 好分数 + high → 压到 C（封顶）
		{90, "high", "C"},
		{60, "high", "C"}, // 恰好 C 档，保持
		{40, "high", "D"}, // 旧 bug：分数 40 属于 D，封顶不应把它抬成 C
		{20, "high", "E"}, // 旧 bug：0~34 属于 E，绝不能抬成 C
		{0, "high", "E"},
		{10, "critical", "E"},
		{98, "medium", "A+"}, // 无 high 时不封顶
	}
	for _, c := range cases {
		grade, _, _ := gradeOf(c.quality, c.level)
		if grade != c.want {
			t.Fatalf("gradeOf(%d,%s)=%s，期望 %s", c.quality, c.level, grade, c.want)
		}
	}
}

func TestAdvisoryRuleSeverities(t *testing.T) {
	if r := ruleIndex["SQL020"]; r.DefaultSeverity != "medium" {
		t.Fatalf("SQL020 应为 medium（静态提示），实际 %s", r.DefaultSeverity)
	}
	if r := ruleIndex["SQL021"]; r.DefaultSeverity != "low" {
		t.Fatalf("SQL021 应为 low（静态提示），实际 %s", r.DefaultSeverity)
	}
}

// 确认递减计分仍然单调：同规则命中越多分越高，但增速递减且封顶。
func TestScoreMonotonicAndBounded(t *testing.T) {
	prev := -1
	for n := 1; n <= 20; n++ {
		fs := make([]Finding, n)
		for i := range fs {
			fs[i] = Finding{RuleID: "SQL020", Severity: "high"}
		}
		got, _ := scoreOf(fs)
		if got < prev || got > 100 {
			t.Fatalf("n=%d 分数非单调或越界: prev=%d got=%d", n, prev, got)
		}
		prev = got
	}
	if math.IsNaN(float64(prev)) {
		t.Fatal("NaN score")
	}
	_ = strings.TrimSpace
}
