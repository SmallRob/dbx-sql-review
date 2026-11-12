package main

// SQL 质量指标
//
// 三个层次，各有用途，别混着看：
//  1. 风险分 risk（Result.Score，0-100）：罚分累加，用于排优先级，值越大越糟；
//  2. 质量分 quality = 100 - risk：给人看的结论，值越大越好；
//  3. 维度健康度：把命中按类别摊到 6 个维度，用饱和曲线算 0-100，
//     避免「20 条缺 COMMENT」把结构维度直接压成 0 分、让报告失去区分度。

import (
	"fmt"
	"math"
	"regexp"
	"strings"
)

// dimSaturation 控制饱和曲线：罚分累加到 55 时维度健康度约 37 分。
// 比线性更抗噪：1 条 high(20) → 70 分，2 条 → 48 分，10 条 → 11 分。
const dimSaturation = 55.0

// DimHealth 是单个质量维度的得分卡。
type DimHealth struct {
	Key        string `json:"key"`
	Label      string `json:"label"`
	Hint       string `json:"hint"`
	Weight     int    `json:"weight"` // 归一化后的权重
	Score      int    `json:"score"`  // 0-100，100 = 该维度无问题
	Hits       int    `json:"hits"`   // 该维度的命中条数
	Penalty    int    `json:"penalty"`
	Worst      string `json:"worst,omitempty"` // 该维度最重的级别
	Applicable bool   `json:"applicable"`
}

// QualityStats 是「这段 SQL 到底有多大」的客观描述，本身不带褒贬。
type QualityStats struct {
	Lines          int `json:"lines"`
	Statements     int `json:"statements"`
	DDL            int `json:"ddl"`
	DML            int `json:"dml"`
	Query          int `json:"query"`
	Txn            int `json:"txn"`
	Tables         int `json:"tables"`
	Indexes        int `json:"indexes"`
	Joins          int `json:"joins"`
	Subqueries     int `json:"subqueries"`
	MaxNesting     int `json:"maxNesting"`
	Dynamic        int `json:"dynamic"`
	Alter          int `json:"alter"`
	CreateTable    int `json:"createTable"`
	UnfilteredDML  int `json:"unfilteredDml"`
}

// QualityMetrics 是回给 UI 的完整质量画像。
type QualityMetrics struct {
	Quality    int         `json:"quality"`
	Grade      string      `json:"grade"`
	GradeLabel string      `json:"gradeLabel"`
	Verdict    string      `json:"verdict"`
	Risk       int         `json:"risk"`
	RiskLevel  string      `json:"riskLevel"`
	Density    float64     `json:"density"` // 每 100 行命中数
	Dimensions []DimHealth `json:"dimensions"`
	Stats      QualityStats `json:"stats"`
}

type dimMeta struct {
	Key    string
	Label  string
	Hint   string
	Weight int
}

// 顺序即 UI 展示顺序；权重之和为 100。
var qualityDimMeta = []dimMeta{
	{"destructive", "安全性", "删表/删列/无 WHERE 等破坏性变更", 32},
	{"performance", "性能", "索引、扫描行数、锁范围", 20},
	{"schema", "结构质量", "主键、类型、命名、注释", 16},
	{"migration", "可迁移性", "命名约定、幂等守卫、事务与方言兼容", 14},
	{"sensitive", "数据合规", "敏感列与凭据是否脱敏", 10},
	{"consistency", "一致性", "目录内版本重复、缺口、内容雷同", 8},
}

var (
	reCreateIndex = regexp.MustCompile(`(?i)\b(CREATE\s+(?:UNIQUE\s+)?INDEX|ADD\s+(?:UNIQUE\s+)?(?:KEY|INDEX))\b`)
	reSubquery    = regexp.MustCompile(`(?i)\(\s*(?:SELECT|WITH)\b`)
)

// CollectStats 只做词法级统计，不连数据库、不改动 SQL。
func CollectStats(raw string, stmts []stmtInfo, tables []string) QualityStats {
	st := QualityStats{Lines: 1 + strings.Count(raw, "\n"), Statements: len(stmts), Tables: len(tables)}
	if strings.TrimSpace(raw) == "" {
		st.Lines = 0
	}
	for _, s := range stmts {
		switch s.Kind {
		case "CREATE":
			st.DDL++
			if strings.Contains(strings.ToUpper(s.Raw), "CREATE TABLE") ||
				strings.Contains(strings.ToUpper(s.Raw), "CREATE TEMPORARY TABLE") {
				st.CreateTable++
			}
		case "ALTER":
			st.DDL++
			st.Alter++
		case "DROP", "TRUNCATE", "RENAME":
			st.DDL++
		case "INSERT", "REPLACE", "UPDATE", "DELETE", "LOAD":
			st.DML++
			if (s.Kind == "UPDATE" || s.Kind == "DELETE") && !s.HasWhere {
				st.UnfilteredDML++
			}
		case "SELECT":
			st.Query++
		case "START", "BEGIN", "COMMIT", "ROLLBACK":
			st.Txn++
		}
	}
	st.Indexes = len(reCreateIndex.FindAllString(raw, -1))
	st.Joins = countWord(raw, "JOIN")
	st.Subqueries = len(reSubquery.FindAllString(raw, -1))
	st.MaxNesting = maxParenDepth(raw)
	return st
}

// maxParenDepth 用括号深度近似嵌套复杂度（字符串字面量里的括号不计）。
func maxParenDepth(raw string) int {
	depth, max := 0, 0
	inStr := ""
	esc := false
	for _, r := range raw {
		if inStr != "" {
			if esc {
				esc = false
				continue
			}
			switch r {
			case '\\':
				esc = true
			case rune(inStr[0]):
				inStr = ""
			}
			continue
		}
		switch r {
		case '\'', '"', '`':
			inStr = string(r)
		case '(':
			depth++
			if depth > max {
				max = depth
			}
		case ')':
			if depth > 0 {
				depth--
			}
		}
	}
	return max
}

// ComputeQuality 由命中集合算出质量画像。
func ComputeQuality(findings []Finding, stats QualityStats) QualityMetrics {
	risk, level := scoreOf(findings)
	m := QualityMetrics{
		Risk: risk, RiskLevel: level,
		Quality: 100 - risk,
		Stats:   stats,
	}
	if m.Quality < 0 {
		m.Quality = 0
	}
	if stats.Lines > 0 {
		m.Density = math.Round(float64(len(findings))*1000/float64(stats.Lines)) / 10
	}
	m.Grade, m.GradeLabel, m.Verdict = gradeOf(m.Quality, level)

	penalty := map[string]int{}
	hits := map[string]int{}
	worst := map[string]string{}
	for _, f := range findings {
		penalty[f.Category] += severityWeight[f.Severity]
		hits[f.Category]++
		if severityRank[f.Severity] > severityRank[worst[f.Category]] {
			worst[f.Category] = f.Severity
		}
	}
	m.Dimensions = buildDims(func(key string) (int, int, string) {
		return dimScore(penalty[key]), hits[key], worst[key]
	})
	return m
}

// buildDims 组装六个维度。
func buildDims(at func(key string) (score, hits int, worst string)) []DimHealth {
	out := make([]DimHealth, 0, len(qualityDimMeta))
	for _, d := range qualityDimMeta {
		dh := DimHealth{Key: d.Key, Label: d.Label, Hint: d.Hint, Applicable: true, Weight: d.Weight}
		dh.Score, dh.Hits, dh.Worst = at(d.Key)
		out = append(out, dh)
	}
	return out
}

func dimScore(penalty int) int {
	if penalty <= 0 {
		return 100
	}
	s := int(math.Round(100 * math.Exp(-float64(penalty)/dimSaturation)))
	if s < 0 {
		s = 0
	}
	return s
}

// gradeOf 把质量分翻译成等级与一句话结论。
// 注意：等级还受「最重命中」封顶——分数再高，只要出现严重级不允许发布，出现高级最多 C。
// 这样避免「一堆提示把分数拉高，却掩盖了一条 DROP TABLE」。
func gradeOf(quality int, worstLevel string) (string, string, string) {
	grade, label, verdict := "A+", "优秀", "可以直接发布"
	switch {
	case quality >= 96:
		grade, label, verdict = "A+", "优秀", "可以直接发布"
	case quality >= 86:
		grade, label, verdict = "A", "良好", "可以发布，建议顺手处理提示项"
	case quality >= 72:
		grade, label, verdict = "B", "尚可", "修复中风险项后发布"
	case quality >= 55:
		grade, label, verdict = "C", "需改进", "修复高风险项后发布"
	case quality >= 35:
		grade, label, verdict = "D", "较差", "不建议直接发布，先改造再评审"
	default:
		grade, label, verdict = "E", "不合格", "禁止发布：风险分已满分"
	}
	// 严重级封顶：不可逆破坏一律不合格
	if worstLevel == "critical" {
		return "E", "不合格", "禁止发布：含严重级（不可逆）变更"
	}
	if worstLevel == "high" && severityRank[gradeToRank(grade)] > severityRank["medium"] {
		return "C", "需改进", "修复高风险项后发布"
	}
	return grade, label, verdict
}

// gradeToRank 把字母等级映射成可比较的等级序列（A+ > A > B > C > D > E）。
func gradeToRank(grade string) string {
	switch grade {
	case "A+", "A":
		return "info"
	case "B":
		return "low"
	case "C":
		return "medium"
	case "D":
		return "high"
	default:
		return "critical"
	}
}

// AggregateQuality 供批量使用：质量分按语句数加权，而不是把罚分全加起来——
// 否则 11 个文件里一个重灾区会让整批归零，掩盖其余 10 个都通过的事实。
func AggregateQuality(results []*Result, consistency []Finding) QualityMetrics {
	var stats QualityStats
	totalW, qSum := 0, 0.0
	dimAcc := map[string]float64{}
	dimHits := map[string]int{}
	dimWorst := map[string]string{}
	worst := "pass"
	lines := 0
	for _, r := range results {
		w := r.Statements
		if w < 1 {
			w = 1
		}
		totalW += w
		qSum += float64(r.Metrics.Quality) * float64(w)
		lines += r.Stats.Lines
		if severityRank[r.Level] > severityRank[worst] {
			worst = r.Level
		}
		for _, d := range r.Metrics.Dimensions {
			if !d.Applicable {
				continue
			}
			dimAcc[d.Key] += float64(d.Score) * float64(w)
			dimHits[d.Key] += d.Hits
			if severityRank[d.Worst] > severityRank[dimWorst[d.Key]] {
				dimWorst[d.Key] = d.Worst
			}
		}
		stats.Lines += r.Stats.Lines
		stats.Statements += r.Stats.Statements
		stats.DDL += r.Stats.DDL
		stats.DML += r.Stats.DML
		stats.Query += r.Stats.Query
		stats.Txn += r.Stats.Txn
		stats.Tables += r.Stats.Tables
		stats.Indexes += r.Stats.Indexes
		stats.Joins += r.Stats.Joins
		stats.Subqueries += r.Stats.Subqueries
		stats.Dynamic += r.Stats.Dynamic
		stats.Alter += r.Stats.Alter
		stats.CreateTable += r.Stats.CreateTable
		stats.UnfilteredDML += r.Stats.UnfilteredDML
		if r.Stats.MaxNesting > stats.MaxNesting {
			stats.MaxNesting = r.Stats.MaxNesting
		}
	}
	if totalW == 0 {
		totalW = 1
	}
	consistencyPenalty := 0
	for _, f := range consistency {
		consistencyPenalty += severityWeight[f.Severity]
		if severityRank[f.Severity] > severityRank[worst] {
			worst = f.Severity
		}
	}
	m := QualityMetrics{
		Quality: int(math.Round(qSum / float64(totalW))),
		Stats:   stats,
		RiskLevel: worst,
	}
	m.Risk = 100 - m.Quality
	if m.Risk < 0 {
		m.Risk = 0
	}
	if stats.Lines > 0 {
		total := 0
		for _, r := range results {
			total += len(r.Findings)
		}
		m.Density = math.Round(float64(total+len(consistency))*1000/float64(stats.Lines)) / 10
	}
	m.Grade, m.GradeLabel, m.Verdict = gradeOf(m.Quality, worst)
	m.Dimensions = buildDims(func(key string) (int, int, string) {
		if key == "consistency" {
			return dimScore(consistencyPenalty), len(consistency), worst
		}
		return int(math.Round(dimAcc[key] / float64(totalW))), dimHits[key], dimWorst[key]
	})
	return m
}

// GradeDistribution 给批量报告一个等级分布，方便看整体水位。
func GradeDistribution(results []*Result) map[string]int {
	out := map[string]int{}
	for _, r := range results {
		out[r.Metrics.Grade]++
	}
	return out
}

// qualityLine 是 Markdown 导出里的一行摘要。
func qualityLine(m QualityMetrics) string {
	dims := make([]string, 0, len(m.Dimensions))
	for _, d := range m.Dimensions {
		if !d.Applicable {
			continue
		}
		dims = append(dims, fmt.Sprintf("%s %d", d.Label, d.Score))
	}
	return fmt.Sprintf("质量分 %d（%s %s）· 风险分 %d · %s", m.Quality, m.Grade, m.GradeLabel, m.Risk, strings.Join(dims, " / "))
}
