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
			Description: "对 SQL 做静态规则评审，返回逐条 finding、风险分、质量分与 reviewId。支持 MySQL、SQLite、SQL Server/T-SQL、PostgreSQL；不连接数据库，不执行 SQL。评审后可调用 sql_review_result 或 sql_optimize。",
			Schema: obj(map[string]any{
				"sql":       propStr("待评审 SQL（DDL/DML 均可）"),
				"file":      propStr("文件名，用于版本号解析与方言提示，可省略"),
				"migration": propBool("按迁移脚本规则评审（命名/幂等类规则）"),
				"dialect":   propStr("方言：auto / mysql / sqlite / sqlserver / postgres；省略则自动判定"),
			}, "sql"),
		},
		{
			Name: "sql_review_result", Access: "read", Title: "评审结果读取",
			Description: "读取最近一次或指定 reviewId 的评审结果，支持按严重级别、规则过滤和分页，供 AI Agent 解释问题或生成补丁；不返回原始 SQL。",
			Schema: obj(map[string]any{
				"reviewId": propStr("sql_review 返回的评审 ID；留空读取最近一次"),
				"severity": propStr("严重级别过滤：critical / high / medium / low / info"),
				"ruleId":   propStr("规则 ID 过滤，例如 SQL003、SQL070、SQL081"),
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
				"dialect": propStr("方言：auto / mysql / sqlite / sqlserver / postgres；省略则自动判定"),
			}, "sql"),
		},
		{
			Name: "sql_rules", Access: "read", Title: "规则清单",
			Description: "返回规则的 id、类别、级别、说明、修复建议与适用方言，以及当前阈值配置与启用状态；可按方言过滤。",
			Schema: obj(map[string]any{
				"dialect": propStr("可选：mysql / sqlite / sqlserver / postgres；留空返回全部"),
			}),
		},
		{
			Name: "sql_best_practices", Access: "read", Title: "最佳实践",
			Description: "返回按 SQL 方言组织的最佳实践条目（分类、原理、推荐做法、反例、正反 SQL 片段）；整理自 ASDM《MySQL 数据库性能优化最佳实践》并适配 Postgres / SQL Server / SQLite。可按方言与分类过滤，或传入 SQL 只返回与命中相关的建议。不连接数据库、不执行 SQL。",
			Schema: obj(map[string]any{
				"dialect":     propStr("可选：mysql / sqlite / sqlserver / postgres；留空返回全部方言"),
				"category":    propStr("可选：index / query / schema / transaction / memory / config / architecture / diagnosis"),
				"sql":         propStr("可选：传入 SQL 时，仅返回与该 SQL 命中规则相关的最佳实践"),
				"file":        propStr("可选：文件名，用于方言提示，可省略"),
				"dialectHint": propStr("可选：与 dialect 等价的别名，方便自然语言调用"),
			}),
		},
		{
			Name: "sql_schema_review", Access: "read", Title: "架构评审",
			Description: "对多段 DDL/DML 脚本构建跨文件虚拟数据字典（表/列/主键/索引/外键/字符集），输出表设计、类型一致性、索引设计、关系完整性与迁移演进类 ARCH 规则命中及按表聚合的优化建议。独立架构分，不连库、不执行 SQL。",
			Schema: obj(map[string]any{
				"files":   propStr("可选：[{path, content}] 文件数组（JSON）"),
				"sql":     propStr("可选：多段脚本合并文本（与 files 二选一或同时提供）"),
				"file":    propStr("可选：文件名，用于方言提示，可省略"),
				"dialect": propStr("可选：mysql / sqlite / sqlserver / postgres；省略则自动判定"),
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
	case "sql_best_practices":
		dialectRaw := getStr("dialect")
		if dialectRaw == "" {
			dialectRaw = getStr("dialectHint")
		}
		filter := NormalizeDialect(dialectRaw)
		if dialectRaw != "" && filter == "" {
			return nil, dbxpluginsdk.NewError(-32602, "dialect 仅支持 mysql / sqlite / sqlserver / postgres")
		}
		category := getStr("category")
		sqlText := getStr("sql")
		if strings.TrimSpace(sqlText) != "" {
			// 传入 SQL：只返回与命中规则相关的建议。
			file := getStr("file")
			if file == "" {
				file = "inline.sql"
			}
			d := resolveDialect(filter, sqlText, file)
			res := AnalyzeSQL(sqlText, file, p.cfg().Rules, AnalyzeOptions{Dialect: d, NoUseRequired: true})
			return map[string]any{
				"version": pluginVersion, "dialect": string(d), "mode": "suggest",
				"total":     len(SuggestPractices(res.Findings, d)),
				"practices": SuggestPractices(res.Findings, d),
				"ruleIds":   collectRuleIDs(res.Findings),
			}, nil
		}
		items := Practices(filter, category)
		return map[string]any{
			"version": pluginVersion, "mode": "catalog", "total": len(items),
			"practices": items, "categories": PracticeCategories(filter),
			"dialects": []string{"mysql", "postgres", "sqlserver", "sqlite"},
		}, nil

	case "sql_rules":
		c := p.cfg()
		filter := NormalizeDialect(getStr("dialect"))
		if raw := getStr("dialect"); raw != "" && filter == "" {
			return nil, dbxpluginsdk.NewError(-32602, "dialect 仅支持 mysql / sqlite / sqlserver / postgres")
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
			"dialects": []string{"mysql", "sqlite", "sqlserver", "postgres"}}, nil

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
		res.withPractices()
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

	case "sql_schema_review":
		var files []batchInput
		if raw, ok := args["files"].([]any); ok {
			for _, item := range raw {
				m, ok := item.(map[string]any)
				if !ok {
					continue
				}
				files = append(files, batchInput{
					Path:    strAny(m["path"]),
					Content: strAny(m["content"]),
				})
			}
		}
		if sqlText := getStr("sql"); strings.TrimSpace(sqlText) != "" {
			files = append(files, batchInput{Path: getStr("file"), Content: sqlText})
		}
		if len(files) == 0 {
			return nil, dbxpluginsdk.NewError(-32602, "files 或 sql 不能为空")
		}
		cfg := p.cfg()
		model := BuildSchemaModel(files, NormalizeDialect(getStr("dialect")))
		report := RunSchemaReview(model, cfg.Rules, true)
		appendAudit("schema-review", files[0].Path, report.Grade, report.Score, len(report.Findings), "ai_tool")
		return map[string]any{
			"version": pluginVersion, "dialect": string(model.Dialect),
			"architecture": report,
			"note":         "静态词法分析：未连接数据库，不执行被评审的 SQL。",
		}, nil
	}
	return nil, dbxpluginsdk.NewError(-32601, "unhandled tool: "+name)
}

func strAny(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
