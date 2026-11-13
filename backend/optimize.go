package main

import "fmt"

type optimizationItem struct {
	RuleID    string `json:"ruleId"`
	Severity  string `json:"severity"`
	Category  string `json:"category"`
	Line      int    `json:"line"`
	StmtIndex int    `json:"stmtIndex"`
	Problem   string `json:"problem"`
	Action    string `json:"action"`
	Snippet   string `json:"snippet,omitempty"`
}

// optimizeSQL 为 AI Agent 生成结构化、可复核的优化方案。
// 没有表结构和语义约束时不做自动改写；全过程保持通用版的零数据库、零网络边界。
func optimizeSQL(sqlText, file string, cfg RuleConfig, opts AnalyzeOptions) map[string]any {
	res := AnalyzeSQL(sqlText, file, cfg, opts)
	items := make([]optimizationItem, 0, len(res.Findings))
	for _, f := range res.Findings {
		if f.Category == catConsistency || f.Category == catSensitive {
			continue
		}
		items = append(items, optimizationItem{
			RuleID: f.RuleID, Severity: f.Severity, Category: f.Category,
			Line: f.Line, StmtIndex: f.StmtIndex, Problem: f.Message,
			Action: f.Advice, Snippet: f.Snippet,
		})
		if len(items) >= 100 {
			break
		}
	}
	summary := "未发现可静态识别的优化项；仍需在隔离环境验证执行计划与结果集。"
	if len(items) > 0 {
		summary = fmt.Sprintf("发现 %d 项可优化点；未做未经证明语义等价的自动改写。", len(items))
	}
	return map[string]any{
		"dialect":       res.Dialect,
		"changed":       false,
		"rewriteStatus": "agent-review-required",
		"sql":           sqlText,
		"optimizedSql":  sqlText,
		"summary":       summary,
		"optimizations": items,
		"review":        res,
		"validation": map[string]any{
			"databaseConnected": false,
			"executed":          false,
			"required":          true,
			"steps": []string{
				"由 AI Agent 或开发者根据 optimizations 生成候选补丁",
				"在隔离环境核对结果集、受影响行数与执行计划",
				"确认迁移可重放及回滚路径后再进入发布流程",
			},
		},
	}
}
