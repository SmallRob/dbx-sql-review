// dbx-sql-review — 公共通用 SQL 评审插件 sidecar。
//
// 职责：
//  1. 对单段 SQL 做确定性规则评审（词法级，不建语法树）；
//  2. 对整个迁移目录做批量评审与文件级一致性校验；
//  3. 把规则清单与评审能力通过 MCP 暴露给 AI 客户端。
//
// 安全边界（对市场评审很重要，明确写死）：
//   - 不连接任何数据库，不发起任何网络请求；
//   - 用户 SQL 永不执行，只做静态词法分析；
//   - 唯一的磁盘写入是插件数据目录下的配置与审计日志（哈希链，append-only）。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	dbxpluginsdk "github.com/t8y2/dbx/plugins/sdk/go/dbx-plugin-sdk"
)

var pluginVersion = "0.1.25"

const pluginID = "io.github.smallrob.sql-review"

type plugin struct {
	mu      sync.Mutex
	batches *batchStore
	reviews *reviewStore
}

func main() {
	p := &plugin{batches: newBatchStore(), reviews: newReviewStore()}
	server := dbxpluginsdk.NewServer(
		dbxpluginsdk.Metadata{ID: pluginID, Version: pluginVersion, Capabilities: []string{}},
		p,
	)
	if err := server.Serve(); err != nil {
		fmt.Fprintf(os.Stderr, "sidecar 退出: %v\n", err)
		os.Exit(1)
	}
}

func (p *plugin) cfg() *Config { return loadConfig() }

// Handle 路由表。
//
// 方法清单（全部为本地静态分析，无 DB、无网络）：
//
//	ping                 健康检查
//	config/get|save      评审配置（规则开关/级别/阈值）
//	rules/get|save       规则清单与开关
//	review/sql           单段 SQL 评审
//	batch/begin|add|run|get|file|export   目录批量评审
//	audit/list|verify    评审记录（哈希链）
//	mcp/tools|call       暴露给 AI 的只读工具
func (p *plugin) Handle(
	_ dbxpluginsdk.RequestContext,
	method string,
	params json.RawMessage,
	emitter *dbxpluginsdk.Emitter,
) (any, *dbxpluginsdk.PluginError) {
	values := map[string]json.RawMessage{}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &values)
	}
	str := func(k string) string {
		var s string
		if raw, ok := values[k]; ok {
			_ = json.Unmarshal(raw, &s)
		}
		return s
	}
	boolean := func(k string) bool {
		var b bool
		if raw, ok := values[k]; ok {
			_ = json.Unmarshal(raw, &b)
		}
		return b
	}

	switch method {
	case "ping", "dbx-sql-review/ping":
		return map[string]any{"ok": true, "version": pluginVersion, "rules": len(ruleCatalog)}, nil

	case "config/get":
		return p.cfg().publicConfig(), nil

	case "config/save":
		raw, ok := values["rules"]
		if !ok {
			return nil, dbxpluginsdk.NewError(-32602, "缺少 rules")
		}
		var rc RuleConfig
		if err := json.Unmarshal(raw, &rc); err != nil {
			return nil, dbxpluginsdk.NewError(-32602, "rules 格式错误")
		}
		cfg := p.cfg()
		cfg.Rules = normalizeRules(rc, cfg.Rules)
		if err := saveConfig(cfg); err != nil {
			return nil, dbxpluginsdk.NewError(-32000, "保存失败: "+err.Error())
		}
		return cfg.publicConfig(), nil

	case "rules/get":
		c := p.cfg()
		return map[string]any{"rules": Rules(c.Rules), "config": c.Rules,
			"thresholds": c.Rules.Thresholds}, nil

	case "rules/save":
		c := p.cfg()
		raw, ok := values["config"]
		if !ok {
			return nil, dbxpluginsdk.NewError(-32602, "缺少 config")
		}
		var rc RuleConfig
		if err := json.Unmarshal(raw, &rc); err != nil {
			return nil, dbxpluginsdk.NewError(-32602, "config 格式错误")
		}
		c.Rules = normalizeRules(rc, c.Rules)
		if err := saveConfig(c); err != nil {
			return nil, dbxpluginsdk.NewError(-32000, "保存失败: "+err.Error())
		}
		return map[string]any{"rules": Rules(c.Rules), "config": c.Rules}, nil

	case "review/sql":
		sqlText := str("sql")
		if strings.TrimSpace(sqlText) == "" {
			return nil, dbxpluginsdk.NewError(-32602, "SQL 为空")
		}
		cfg := p.cfg()
		file := str("file")
		if file == "" {
			file = "inline.sql"
		}
		dialect := resolveDialect(NormalizeDialect(str("dialect")), sqlText, file)
		opts := AnalyzeOptions{
			Migration: boolean("migration"),
			Dialect:   dialect,
			// 通用插件不假设「必须写 USE」——那是项目约定，不是通用质量问题。
			NoUseRequired: true,
		}
		res := AnalyzeSQL(sqlText, file, cfg.Rules, opts)
		actor := str("actor")
		if actor == "" {
			actor = cfg.Actor
		}
		appendAudit("review", file, res.Level, res.Score, len(res.Findings), actor)
		note := "静态词法分析：未连接数据库，不执行被评审的 SQL。"
		review := p.reviews.put(sqlText, file, note, res, opts)
		return map[string]any{
			"reviewId":   review.ID,
			"result":     res,
			"reviewedAt": review.ReviewedAt.Format(time.RFC3339),
			"metrics":    res.Metrics,
			"note":       note,
		}, nil

	case "batch/begin":
		total := 0
		if raw, ok := values["total"]; ok {
			_ = json.Unmarshal(raw, &total)
		}
		sess := p.batches.create(total, str("dialect"))
		return map[string]any{"batchId": sess.ID}, nil

	case "batch/add":
		var files []batchInput
		if raw, ok := values["files"]; ok {
			if err := json.Unmarshal(raw, &files); err != nil {
				return nil, dbxpluginsdk.NewError(-32602, "files 格式错误")
			}
		}
		n, err := p.batches.addFiles(str("batchId"), files, p.cfg().Rules.Thresholds.MaxFilesPerBatch)
		if err != nil {
			return nil, dbxpluginsdk.NewError(-32602, err.Error())
		}
		return map[string]any{"received": n}, nil

	case "batch/run":
		report, err := p.runBatch(str("batchId"), emitter)
		if err != nil {
			return nil, dbxpluginsdk.NewError(-32000, err.Error())
		}
		return map[string]any{"report": report}, nil

	case "batch/get":
		sess := p.batches.get(str("batchId"))
		if sess == nil || sess.Report == nil {
			return nil, dbxpluginsdk.NewError(-32602, "批次不存在或尚未运行")
		}
		return map[string]any{"report": sess.Report, "running": sess.Running, "progress": sess.Progress}, nil

	case "batch/file":
		sess := p.batches.get(str("batchId"))
		if sess == nil || sess.Results == nil {
			return nil, dbxpluginsdk.NewError(-32602, "批次不存在或尚未运行")
		}
		want := str("path")
		for _, r := range sess.Results {
			if r.File == want {
				return map[string]any{"result": r}, nil
			}
		}
		return nil, dbxpluginsdk.NewError(-32602, "文件不在本批次中："+want)

	case "batch/export":
		sess := p.batches.get(str("batchId"))
		if sess == nil || sess.Report == nil {
			return nil, dbxpluginsdk.NewError(-32602, "批次不存在或尚未运行")
		}
		return map[string]any{"markdown": exportMarkdown(sess.Report, sess.Results)}, nil

	case "audit/list":
		limit := 200
		if raw, ok := values["limit"]; ok {
			_ = json.Unmarshal(raw, &limit)
		}
		entries := readAudit()
		if limit > 0 && len(entries) > limit {
			entries = entries[len(entries)-limit:]
		}
		// 倒序，最新在前
		for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
			entries[i], entries[j] = entries[j], entries[i]
		}
		okChain, brokenAt, total := verifyAudit()
		return map[string]any{"entries": entries, "chainOk": okChain, "brokenAt": brokenAt, "total": total}, nil

	case "audit/verify":
		okChain, brokenAt, total := verifyAudit()
		return map[string]any{"ok": okChain, "brokenAt": brokenAt, "total": total}, nil

	case "mcp/tool", "mcp/tools":
		return handleMcpTool(params), nil

	case "mcp/call":
		return p.handleMcpCall(values)
	}
	return nil, dbxpluginsdk.MethodNotFound(method)
}

// optionalBool 读取「可能没传」的布尔参数。
func optionalBool(values map[string]json.RawMessage, key string) (bool, bool) {
	raw, ok := values[key]
	if !ok {
		return false, false
	}
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		return false, false
	}
	return v, true
}

func normalizeRules(rc RuleConfig, fallback RuleConfig) RuleConfig {
	if rc.Severity == nil {
		rc.Severity = map[string]string{}
	}
	if rc.Thresholds.LargeTableRows <= 0 {
		rc.Thresholds.LargeTableRows = fallback.Thresholds.LargeTableRows
	}
	if rc.Thresholds.MaxStatements <= 0 {
		rc.Thresholds.MaxStatements = fallback.Thresholds.MaxStatements
	}
	if rc.Disabled == nil {
		// nil 表示「没传这个字段」，保留既有配置；不能置空数组，
		// 否则会把房规类规则（默认关闭）全部打开。
		rc.Disabled = fallback.Disabled
	}
	if rc.Disabled == nil {
		rc.Disabled = defaultDisabled()
	}
	return rc
}
