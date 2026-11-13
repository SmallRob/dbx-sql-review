// MCP 暴露层：只读的静态评审能力给 AI 用，写操作一个都不开。
package main

import (
	"encoding/json"
	"strings"
	"time"

	dbxpluginsdk "github.com/t8y2/dbx/plugins/sdk/go/dbx-plugin-sdk"
)

type mcpToolSpec struct {
	Name        string
	Access      string
	Title       string
	Description string
	Schema      map[string]any
}

func obj(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func propStr(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func propBool(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

func propInt(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func mcpTools() []mcpToolSpec {
	return []mcpToolSpec{
		{
			Name: "sql_review", Access: "read", Title: "SQL 评审",
			Description: "对 SQL 做静态规则评审，返回逐条 finding、风险分、质量分与 reviewId。支持 MySQL、SQLite、SQL Server/T-SQL；不连接数据库，不执行 SQL。评审后可调用 sql_review_result 或 sql_optimize。",
			Schema: obj(map[string]any{
				"sql":       propStr("待评审 SQL（DDL/DML 均可）"),
				"file":      propStr("文件名，用于版本号解析与方言提示，可省略"),
				"migration": propBool("按迁移脚本规则评审（命名/幂等类规则）"),
				"dialect":   propStr("方言：auto / mysql / sqlite / sqlserver；省略则自动判定"),
			}, "sql"),
		},
		{
			Name: "sql_review_result", Access: "read", Title: "评审结果读取",
			Description: "读取最近一次或指定 reviewId 的评审结果，支持按严重级别、规则过滤和分页，供 AI Agent 解释问题或生成补丁；不返回原始 SQL。",
			Schema: obj(map[string]any{
				"reviewId": propStr("sql_review 返回的评审 ID；留空读取最近一次"),
				"severity": propStr("严重级别过滤：critical / high / medium / low / info"),
				"ruleId":   propStr("规则 ID 过滤，例如 SQL003、SQL070"),
				"limit":    propInt("最多返回条数，默认 100，范围 1–500"),
				"offset":   propInt("发现列表偏移量，默认 0"),
			}),
		},
		{
			Name: "sql_optimize", Access: "read", Title: "SQL 优化建议",
			Description: "为 SQL 生成结构化优化建议与验证步骤，供 AI Agent 生成可复核补丁；changed=false，不做未经证明的自动改写，不连接数据库、不执行 SQL。",
			Schema: obj(map[string]any{
				"sql":     propStr("待优化 SQL"),
				"file":    propStr("文件名，用于方言提示，可省略"),
				"dialect": propStr("方言：auto / mysql / sqlite / sqlserver；省略则自动判定"),
			}, "sql"),
		},
		{
			Name: "sql_rules", Access: "read", Title: "规则清单",
			Description: "返回规则的 id、类别、级别、说明、修复建议与适用方言，以及当前阈值配置与启用状态；可按方言过滤。",
			Schema: obj(map[string]any{
				"dialect": propStr("可选：mysql / sqlite / sqlserver；留空返回全部"),
			}),
		},
	}
}

func mcpAnnotations(access string) map[string]any {
	if access == "read" {
		return map[string]any{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false}
	}
	return map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": false, "openWorldHint": false}
}

func mcpToolDescriptor(s mcpToolSpec) map[string]any {
	return map[string]any{
		"name": s.Name, "title": s.Title, "description": s.Description,
		"inputSchema": s.Schema, "annotations": mcpAnnotations(s.Access),
	}
}

func findMcpTool(name string) (mcpToolSpec, bool) {
	for _, s := range mcpTools() {
		if s.Name == name {
			return s, true
		}
	}
	return mcpToolSpec{}, false
}

// normalizeToolName 兼容宿主给工具名加的前缀（如 dbx_sql_review__sql_review）。
func normalizeToolName(raw string) string {
	name := strings.TrimSpace(raw)
	for _, prefix := range []string{"dbx_sql_review__", "dbx_sqlreview__", "dbx_"} {
		if strings.HasPrefix(name, prefix) {
			name = strings.TrimPrefix(name, prefix)
			break
		}
	}
	if _, ok := findMcpTool(name); ok {
		return name
	}
	for _, s := range mcpTools() {
		if strings.HasSuffix(name, s.Name) {
			return s.Name
		}
	}
	return name
}

func handleMcpTool(params json.RawMessage) any {
	var req map[string]any
	_ = json.Unmarshal(params, &req)
	if name, _ := req["name"].(string); name != "" {
		if spec, ok := findMcpTool(normalizeToolName(name)); ok {
			return mcpToolDescriptor(spec)
		}
	}
	tools := make([]map[string]any, 0, len(mcpTools()))
	for _, s := range mcpTools() {
		tools = append(tools, mcpToolDescriptor(s))
	}
	return map[string]any{"tools": tools}
}

func toolError(msg string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": msg}},
		"isError": true,
	}
}

func (p *plugin) handleMcpCall(values map[string]json.RawMessage) (any, *dbxpluginsdk.PluginError) {
	var name string
	if raw, ok := values["name"]; ok {
		_ = json.Unmarshal(raw, &name)
	}
	if name == "" {
		if raw, ok := values["tool"]; ok {
			_ = json.Unmarshal(raw, &name)
		}
	}
	name = normalizeToolName(name)
	if _, ok := findMcpTool(name); !ok {
		return toolError("unknown tool: " + name), nil
	}
	args := map[string]any{}
	for _, key := range []string{"arguments", "args", "input", "params"} {
		if raw, ok := values[key]; ok && len(raw) > 0 && string(raw) != "null" {
			_ = json.Unmarshal(raw, &args)
			break
		}
	}
	res, err := p.dispatchTool(name, args)
	if err != nil {
		return toolError(err.Message), nil
	}
	blob, _ := json.MarshalIndent(res, "", "  ")
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(blob)}},
		"isError": false,
	}, nil
}

func (p *plugin) dispatchTool(name string, args map[string]any) (any, *dbxpluginsdk.PluginError) {
	getStr := func(k string) string {
		if v, ok := args[k].(string); ok {
			return strings.TrimSpace(v)
		}
		return ""
	}
	getInt := func(k string, fallback int) int {
		switch v := args[k].(type) {
		case float64:
			return int(v)
		case int:
			return v
		default:
			return fallback
		}
	}
	switch name {
	case "sql_rules":
		c := p.cfg()
		filter := NormalizeDialect(getStr("dialect"))
		if raw := getStr("dialect"); raw != "" && filter == "" {
			return nil, dbxpluginsdk.NewError(-32602, "dialect 仅支持 mysql / sqlite / sqlserver")
		}
		all := Rules(c.Rules)
		rules := make([]RuleMeta, 0, len(all))
		for _, r := range all {
			if filter != "" && len(r.Dialects) > 0 {
				matched := false
				for _, d := range r.Dialects {
					if Dialect(d) == filter {
						matched = true
						break
					}
				}
				if !matched {
					continue
				}
			}
			rules = append(rules, r)
		}
		return map[string]any{"version": pluginVersion, "total": len(rules), "rules": rules,
			"thresholds": c.Rules.Thresholds, "config": c.Rules,
			"dialects": []string{"mysql", "sqlite", "sqlserver"}}, nil

	case "sql_review":
		sqlText := getStr("sql")
		if strings.TrimSpace(sqlText) == "" {
			return nil, dbxpluginsdk.NewError(-32602, "sql 为空")
		}
		cfg := p.cfg()
		file := getStr("file")
		if file == "" {
			file = "inline.sql"
		}
		migration, _ := args["migration"].(bool)
		dialect := resolveDialect(NormalizeDialect(getStr("dialect")), sqlText, file)
		opts := AnalyzeOptions{Migration: migration, Dialect: dialect, NoUseRequired: true}
		res := AnalyzeSQL(sqlText, file, cfg.Rules, opts)
		appendAudit("review", file, res.Level, res.Score, len(res.Findings), "ai_tool")
		note := "静态词法分析：未连接数据库，不执行被评审的 SQL。"
		review := p.reviews.put(sqlText, file, note, res, opts)
		return map[string]any{
			"reviewId": review.ID, "result": res,
			"reviewedAt": review.ReviewedAt.Format(time.RFC3339),
			"metrics":    res.Metrics, "note": note,
			"nextTools": []string{"sql_review_result", "sql_optimize"},
		}, nil

	case "sql_review_result":
		r, ok := p.reviews.get(getStr("reviewId"))
		if !ok {
			return nil, dbxpluginsdk.NewError(-32602, "未找到评审结果；请先调用 sql_review，或传入有效 reviewId")
		}
		severity := strings.ToLower(getStr("severity"))
		ruleID := strings.ToUpper(getStr("ruleId"))
		all := make([]Finding, 0, len(r.Result.Findings))
		for _, f := range r.Result.Findings {
			if severity != "" && f.Severity != severity {
				continue
			}
			if ruleID != "" && f.RuleID != ruleID {
				continue
			}
			all = append(all, f)
		}
		limit := getInt("limit", 100)
		if limit < 1 || limit > 500 {
			limit = 100
		}
		offset := getInt("offset", 0)
		if offset < 0 {
			offset = 0
		}
		if offset > len(all) {
			offset = len(all)
		}
		end := offset + limit
		if end > len(all) {
			end = len(all)
		}
		page := all[offset:end]
		if page == nil {
			page = []Finding{}
		}
		return map[string]any{
			"reviewId": r.ID, "file": r.File, "reviewedAt": r.ReviewedAt.Format(time.RFC3339),
			"dialect": r.Result.Dialect, "level": r.Result.Level, "score": r.Result.Score,
			"counts": r.Result.Counts, "metrics": r.Result.Metrics,
			"findings": page, "total": len(all), "offset": offset, "limit": limit,
			"hasMore": end < len(all), "note": r.Note,
		}, nil

	case "sql_optimize":
		sqlText := getStr("sql")
		if strings.TrimSpace(sqlText) == "" {
			return nil, dbxpluginsdk.NewError(-32602, "sql 为空")
		}
		file := getStr("file")
		if file == "" {
			file = "inline.sql"
		}
		cfg := p.cfg()
		dialect := resolveDialect(NormalizeDialect(getStr("dialect")), sqlText, file)
		opts := AnalyzeOptions{Dialect: dialect, NoUseRequired: true}
		return optimizeSQL(sqlText, file, cfg.Rules, opts), nil
	}
	return nil, dbxpluginsdk.NewError(-32601, "unhandled tool: "+name)
}
