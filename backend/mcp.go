// MCP 暴露层：只读的静态评审能力给 AI 用，写操作一个都不开。
package main

import (
	"encoding/json"
	"strings"

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

func mcpTools() []mcpToolSpec {
	return []mcpToolSpec{
		{
			Name: "sql_review", Access: "read", Title: "SQL 评审",
			Description: "对一段 SQL 做静态规则评审：破坏性、迁移规范、性能、结构质量、敏感数据、一致性六类，返回逐条 finding（行号/片段/建议）、风险分与质量分。纯词法分析，不连接数据库，不执行被评审的 SQL。",
			Schema: obj(map[string]any{
				"sql":       propStr("待评审 SQL（DDL/DML 均可）"),
				"file":      propStr("文件名，用于版本号解析，可省略"),
				"migration": propBool("按迁移脚本规则评审（命名/幂等类规则）"),
				"dialect":   propStr("方言：mysql / sqlite；省略则按内容自动判定"),
			}, "sql"),
		},
		{
			Name: "sql_rules", Access: "read", Title: "规则清单",
			Description: "返回全部规则的 id、类别、级别、说明与修复建议，以及当前阈值配置与启用状态。标记为「项目约定」的规则默认关闭，可由调用方显式开启。",
			Schema:      obj(map[string]any{}),
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
			return v
		}
		return ""
	}
	switch name {
	case "sql_rules":
		c := p.cfg()
		return map[string]any{"rules": Rules(c.Rules), "thresholds": c.Rules.Thresholds,
			"config": c.Rules}, nil

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
		res := AnalyzeSQL(sqlText, file, cfg.Rules, AnalyzeOptions{
			Migration:     migration,
			Dialect:       dialect,
			NoUseRequired: true,
		})
		return map[string]any{"result": res}, nil
	}
	return nil, dbxpluginsdk.NewError(-32601, "unhandled tool: "+name)
}
