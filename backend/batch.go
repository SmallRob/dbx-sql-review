package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	dbxpluginsdk "github.com/t8y2/dbx/plugins/sdk/go/dbx-plugin-sdk"
)

type batchInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type fileSummary struct {
	File        string         `json:"file"`
	Version     string         `json:"version,omitempty"`
	Description string         `json:"description,omitempty"`
	Statements  int            `json:"statements"`
	Score       int            `json:"score"`
	Level       string         `json:"level"`
	Quality     int            `json:"quality"`
	Grade       string         `json:"grade"`
	Dialect     string         `json:"dialect"`
	Counts      map[string]int `json:"counts"`
	Top         []Finding      `json:"top"`
	Tables      int            `json:"tables"`
	Note        string         `json:"note,omitempty"`
}

type batchSummary struct {
	Files      int            `json:"files"`
	Statements int            `json:"statements"`
	Counts     map[string]int `json:"counts"`
	Score      int            `json:"score"`
	Level      string         `json:"level"`
	Passing    int            `json:"passing"`
	Failing    int            `json:"failing"`
	Worst      []fileSummary  `json:"worst"`
	// Quality/QualityGrade 是整批的聚合质量画像；Grades 是文件等级分布。
	Quality      QualityMetrics `json:"quality"`
	QualityGrade string         `json:"qualityGrade"`
	Grades       map[string]int `json:"grades"`
}

type BatchReport struct {
	ID           string         `json:"id"`
	Dialect      string         `json:"dialect"`
	RulesProfile string         `json:"rulesProfile"`
	CreatedAt    string         `json:"createdAt"`
	Summary      batchSummary   `json:"summary"`
	Files        []fileSummary  `json:"files"`
	Consistency  []Finding      `json:"consistency"`
	ConsistencySummary map[string]any `json:"consistencySummary"`
	Note         string         `json:"note,omitempty"`
}

type batchSession struct {
	ID        string
	Dialect   string
	CreatedAt time.Time
	Files     []batchInput
	Total     int
	Running   bool
	Progress  int
	Report    *BatchReport
	Results   []*Result
}

type batchStore struct {
	mu       sync.Mutex
	sessions map[string]*batchSession
	order    []string
}

func newBatchStore() *batchStore { return &batchStore{sessions: map[string]*batchSession{}} }

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return fmt.Sprintf("b%s%s", time.Now().Format("0102-1504"), hex.EncodeToString(b))
}

func (s *batchStore) create(total int, dialect string) *batchSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := &batchSession{ID: newID(), Dialect: dialect, CreatedAt: time.Now(), Total: total}
	s.sessions[sess.ID] = sess
	s.order = append(s.order, sess.ID)
	if len(s.order) > 8 {
		old := s.order[0]
		s.order = s.order[1:]
		delete(s.sessions, old)
	}
	return sess
}

func (s *batchStore) get(id string) *batchSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

func (s *batchStore) addFiles(id string, files []batchInput, limit int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return 0, fmt.Errorf("批次不存在或已过期：%s", id)
	}
	if sess.Running {
		return 0, fmt.Errorf("批次正在运行，不能追加文件")
	}
	if limit <= 0 {
		limit = 800
	}
	if len(sess.Files)+len(files) > limit {
		return 0, fmt.Errorf("单批次最多 %d 个文件", limit)
	}
	sess.Files = append(sess.Files, files...)
	return len(sess.Files), nil
}

func (s *batchStore) setRunning(id string, running bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[id]; ok {
		sess.Running = running
	}
}

func (s *batchStore) setProgress(id string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[id]; ok {
		sess.Progress = n
	}
}

func (s *batchStore) finish(id string, report *BatchReport, results []*Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[id]; ok {
		sess.Running = false
		sess.Report = report
		sess.Results = results
	}
}

// ---------------------------------------------------------------- 执行

func (p *plugin) runBatch(id string, emitter *dbxpluginsdk.Emitter) (*BatchReport, error) {
	sess := p.batches.get(id)
	if sess == nil {
		return nil, fmt.Errorf("批次不存在：%s", id)
	}
	if len(sess.Files) == 0 {
		return nil, fmt.Errorf("批次内没有文件")
	}
	p.batches.setRunning(id, true)
	defer p.batches.setRunning(id, false)

	cfg := p.cfg()
	// 房规类规则是否被用户开启：看默认关闭的 Optional 规则有没有被移出 Disabled。
	allowOptional := optionalRulesEnabled(cfg.Rules)

	files := sess.Files
	results := make([]*Result, 0, len(files))
	locals := make([]localFile, 0, len(files))

	dialectSet := map[Dialect]bool{}
	for i, f := range files {
		fd := resolveDialect(NormalizeDialect(sess.Dialect), f.Content, f.Path)
		dialectSet[fd] = true
		opts := AnalyzeOptions{
			Migration:     true,
			Dialect:       fd,
			NoUseRequired: true,
		}
		res := AnalyzeSQL(f.Content, f.Path, cfg.Rules, opts)
		if _, _, ok := parseVersionedName(f.Path); !ok {
			meta := ruleIndex["SQL010"]
			res.Findings = append(res.Findings, Finding{
				RuleID: "SQL010", Title: meta.Title, Category: meta.Category,
				Severity: "high", Message: "文件名不符合 V<版本>__<描述>.sql：" + f.Path,
				Advice: meta.Advice, File: f.Path, Line: 1,
			})
			res.refresh()
		}
		results = append(results, res)

		v, d, ok := parseVersionedName(f.Path)
		locals = append(locals, localFile{Path: f.Path, Version: v, Desc: d, OK: ok,
			Sum: flywayChecksum([]byte(f.Content))})

		if emitter != nil && (i%10 == 0 || i == len(files)-1) {
			p.batches.setProgress(id, i+1)
			_ = emitter.Event("batch/progress", map[string]any{
				"batchId": id, "done": i + 1, "total": len(files), "path": f.Path,
			})
		}
	}

	// 目录内一致性校验：完全静态，不连库。
	consistency, consistencySummary := consistencyFindings(locals, cfg.Rules)

	rulesProfile := "通用 SQL 评审"
	if allowOptional {
		rulesProfile = "通用 SQL 评审 + 项目约定类规则"
	}
	report := &BatchReport{
		ID: sess.ID, Dialect: joinDialects(dialectSet),
		RulesProfile: rulesProfile, CreatedAt: nowRFC3339(),
		Consistency: consistency, ConsistencySummary: consistencySummary,
		Note: "静态词法分析：未连接数据库，不执行被评审的 SQL。",
	}
	report.Summary, report.Files = summarizeBatch(results, consistency)
	p.batches.finish(id, report, results)
	p.persistReport(report, results)

	appendAudit("batch", fmt.Sprintf("%d files", len(results)),
		report.Summary.Level, report.Summary.Score,
		report.Summary.Counts["critical"]+report.Summary.Counts["high"], cfg.Actor)
	return report, nil
}

// optionalRulesEnabled 判断用户是否显式开启了任一 Optional（房规类）规则。
func optionalRulesEnabled(cfg RuleConfig) bool {
	disabled := map[string]bool{}
	for _, d := range cfg.Disabled {
		disabled[strings.ToUpper(strings.TrimSpace(d))] = true
	}
	for _, r := range ruleCatalog {
		if !r.Optional {
			continue
		}
		if !disabled[strings.ToUpper(r.ID)] {
			return true
		}
	}
	return false
}

func summarizeBatch(results []*Result, consistency []Finding) (batchSummary, []fileSummary) {
	sum := batchSummary{Counts: map[string]int{}, Level: "pass"}
	files := make([]fileSummary, 0, len(results))
	for _, r := range results {
		sum.Files++
		sum.Statements += r.Statements
		for k, v := range r.Counts {
			sum.Counts[k] += v
		}
		if r.Level == "pass" || r.Level == "info" {
			sum.Passing++
		} else {
			sum.Failing++
		}
		fs := fileSummary{
			File: r.File, Version: r.Version, Description: r.Description,
			Statements: r.Statements, Score: r.Score, Level: r.Level,
			Quality: r.Metrics.Quality, Grade: r.Metrics.Grade, Dialect: r.Dialect,
			Counts: r.Counts, Tables: len(r.Tables), Note: r.Note,
		}
		if n := len(r.Findings); n > 0 {
			top := r.Findings
			if n > 3 {
				top = top[:3]
			}
			fs.Top = top
		}
		files = append(files, fs)
	}
	for _, f := range consistency {
		sum.Counts[f.Severity]++
	}
	sort.SliceStable(files, func(i, j int) bool {
		if severityRank[files[i].Level] != severityRank[files[j].Level] {
			return severityRank[files[i].Level] > severityRank[files[j].Level]
		}
		return files[i].Score > files[j].Score
	})
	sum.Level = "pass"
	if len(files) > 0 {
		sum.Level = files[0].Level
	}
	if severityRank[consistencyLevel(consistency)] > severityRank[sum.Level] {
		sum.Level = consistencyLevel(consistency)
	}
	if sum.Level == "pass" && len(consistency) > 0 {
		sum.Level = "info"
	}
	worst := files
	if len(worst) > 8 {
		worst = worst[:8]
	}
	sum.Worst = worst
	// 整批风险分取最差文件，而不是累加——累加会让 100 个文件必然满分。
	sum.Score = 0
	for _, f := range files {
		if f.Score > sum.Score {
			sum.Score = f.Score
		}
	}
	// 整批质量画像：把所有文件与一致性结论合成一份。
	sum.Quality = AggregateQuality(results, consistency)
	sum.QualityGrade = sum.Quality.Grade
	sum.Grades = GradeDistribution(results)
	return sum, files
}

func consistencyLevel(list []Finding) string {
	lvl := "info"
	for _, f := range list {
		if severityRank[f.Severity] > severityRank[lvl] {
			lvl = f.Severity
		}
	}
	return lvl
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (p *plugin) persistReport(r *BatchReport, results []*Result) {
	dir := dbxpluginsdk.DataDir()
	if dir == "" {
		return
	}
	bdir := filepath.Join(dir, "batches")
	if err := os.MkdirAll(bdir, 0o700); err != nil {
		return
	}
	payload := map[string]any{"report": r, "results": results}
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(bdir, r.ID+".json"), data, 0o600)
}

// exportMarkdown 产出可直接贴进 PR / 周报的评审报告。
func exportMarkdown(r *BatchReport, results []*Result) string {
	var b strings.Builder
	b.WriteString("# SQL 评审报告\n\n")
	fmt.Fprintf(&b, "- 批次：`%s`\n- 方言：%s\n- 时间：%s\n- 规则集：`%s`\n\n",
		r.ID, r.Dialect, r.CreatedAt, r.RulesProfile)
	fmt.Fprintf(&b, "## 汇总\n\n| 文件 | 语句 | 严重 | 高 | 中 | 低 | 结果 |\n|---|--:|--:|--:|--:|--:|---|\n")
	fmt.Fprintf(&b, "| %d | %d | %d | %d | %d | %d | %s |\n\n",
		r.Summary.Files, r.Summary.Statements, r.Summary.Counts["critical"], r.Summary.Counts["high"],
		r.Summary.Counts["medium"], r.Summary.Counts["low"], r.Summary.Level)
	fmt.Fprintf(&b, "**整批质量：%s**\n\n", qualityLine(r.Summary.Quality))
	fmt.Fprintf(&b, "| 维度 | 健康度 | 命中 |\n|---|--:|--:|\n")
	for _, d := range r.Summary.Quality.Dimensions {
		if !d.Applicable {
			fmt.Fprintf(&b, "| %s | — | 未校验 |\n", d.Label)
			continue
		}
		fmt.Fprintf(&b, "| %s | %d | %d |\n", d.Label, d.Score, d.Hits)
	}
	b.WriteString("\n")
	if len(r.Summary.Grades) > 0 {
		var gs []string
		for _, g := range []string{"A+", "A", "B", "C", "D", "E"} {
			if n := r.Summary.Grades[g]; n > 0 {
				gs = append(gs, fmt.Sprintf("%s×%d", g, n))
			}
		}
		if len(gs) > 0 {
			fmt.Fprintf(&b, "等级分布：%s\n\n", strings.Join(gs, " · "))
		}
	}

	if len(r.Consistency) > 0 {
		b.WriteString("## 目录一致性\n\n")
		for _, f := range r.Consistency {
			fmt.Fprintf(&b, "- **[%s] %s** — %s\n", f.Severity, f.RuleID, f.Message)
		}
		b.WriteString("\n")
	}

	b.WriteString("## 问题明细\n\n")
	for _, res := range results {
		if len(res.Findings) == 0 {
			continue
		}
		fmt.Fprintf(&b, "### %s（质量 %d / %s %s，风险分 %d）\n\n",
			res.File, res.Metrics.Quality, res.Metrics.Grade, res.Metrics.GradeLabel, res.Score)
		for _, f := range res.Findings {
			fmt.Fprintf(&b, "- **[%s] %s %s** L%d — %s\n  - 建议：%s\n",
				f.Severity, f.RuleID, f.Title, f.Line, f.Message, f.Advice)
			if f.Snippet != "" {
				fmt.Fprintf(&b, "  - 片段：`%s`\n", strings.ReplaceAll(truncStr(f.Snippet, 120), "`", "'"))
			}
		}
		b.WriteString("\n")
	}
	b.WriteString("---\n\n> 由 dbx 插件 SQL Review 生成；静态分析为词法级，动态拼接的标识符无法还原。\n")
	return b.String()
}

// ---------------------------------------------------------------- 目录一致性

// localFile 是批内一个脚本的本地元信息。
type localFile struct {
	Path    string
	Version string
	Desc    string
	OK      bool
	Sum     int32
}

// consistencyFindings 在不连库的前提下做目录内一致性校验。
//
// 这是「不连库也能发现迁移漂移」的替代方案：只看目录自身就能发现
// 版本重复、版本缺口、以及两个不同版本文件内容完全雷同（复制粘贴忘改）。
func consistencyFindings(locals []localFile, cfg RuleConfig) ([]Finding, map[string]any) {
	fs := []Finding{}
	add := func(ruleID, msg, advice string, ev map[string]any) {
		meta, ok := ruleIndex[ruleID]
		if !ok {
			return
		}
		for _, d := range cfg.Disabled {
			if strings.EqualFold(d, ruleID) {
				return
			}
		}
		sev := meta.DefaultSeverity
		if s, ok := cfg.Severity[ruleID]; ok && s != "" {
			sev = s
		}
		fs = append(fs, Finding{
			RuleID: ruleID, Title: meta.Title, Category: meta.Category, Severity: sev,
			Message: msg, Advice: advice, Line: 1, Evidence: ev,
		})
	}

	localByVer := map[string][]localFile{}
	for _, l := range locals {
		if !l.OK {
			continue
		}
		localByVer[l.Version] = append(localByVer[l.Version], l)
	}

	// ① 同一版本出现多次
	var dupVersions []string
	for v, group := range localByVer {
		if len(group) > 1 {
			dupVersions = append(dupVersions, v)
			paths := make([]string, 0, len(group))
			for _, g := range group {
				paths = append(paths, g.Path)
			}
			add("SQL050", fmt.Sprintf("版本 %s 在本地重复：%s", v, strings.Join(paths, ", ")),
				"同一版本只能有一个脚本，否则版本管理工具会直接失败。",
				map[string]any{"version": v, "files": paths})
		}
	}
	sort.Strings(dupVersions)

	// ② 版本号缺口（按数字段比较，2.3 与 2.3.0 视为相同）
	var gaps []string
	versions := make([]string, 0, len(localByVer))
	for v := range localByVer {
		versions = append(versions, v)
	}
	sort.Slice(versions, func(i, j int) bool { return compareVersion(versions[i], versions[j]) < 0 })
	for i := 1; i < len(versions); i++ {
		prev, cur := versions[i-1], versions[i]
		if compareVersion(prev, cur) == 0 {
			continue
		}
		if isAdjacentVersion(prev, cur) {
			continue
		}
		gaps = append(gaps, fmt.Sprintf("%s → %s", prev, cur))
	}
	if len(gaps) > 0 {
		add("SQL051", fmt.Sprintf("版本号存在缺口：%s", strings.Join(gaps, "、")),
			"确认缺口是刻意跳过还是漏提交；漏提交会补一条补丁脚本。",
			map[string]any{"gaps": gaps})
	}

	// ③ 不同版本内容完全相同（复制粘贴忘改）
	bySum := map[int32][]localFile{}
	for _, l := range locals {
		if !l.OK || l.Sum == 0 {
			continue
		}
		bySum[l.Sum] = append(bySum[l.Sum], l)
	}
	var dupContents []string
	for sum, group := range bySum {
		if len(group) < 2 {
			continue
		}
		// 同一版本内的重复文件已由 ① 报告，这里只报「版本不同但内容相同」
		vers := map[string]bool{}
		for _, g := range group {
			vers[g.Version] = true
		}
		if len(vers) < 2 {
			continue
		}
		paths := make([]string, 0, len(group))
		for _, g := range group {
			paths = append(paths, g.Path)
		}
		sort.Strings(paths)
		dupContents = append(dupContents, fmt.Sprintf("校验和 %d：%s", sum, strings.Join(paths, ", ")))
	}
	sort.Strings(dupContents)
	if len(dupContents) > 0 {
		add("SQL052", fmt.Sprintf("%d 组不同版本的文件内容完全相同：%s", len(dupContents), strings.Join(dupContents, "；")),
			"确认第二个脚本确实是独立变更；否则合并或删除。",
			map[string]any{"groups": dupContents})
	}

	summary := map[string]any{
		"files":         len(locals),
		"namedVersions": len(localByVer),
		"duplicateVersions": dupVersions,
		"gaps":          gaps,
		"duplicateContents": dupContents,
	}
	return fs, summary
}

// isAdjacentVersion 判断两个版本号是否相邻（同一段内的下一个递增）。
//
// 相邻定义：把版本拆成数字段，只允许最后一段相差 1。
// 例如 2.3.40 → 2.3.41 算相邻；2.3.40 → 2.4.0 算相邻（同长度末段 +1）；
// 2.3.40 → 2.3.42 有缺口。
func isAdjacentVersion(a, b string) bool {
	pa := splitVersion(a)
	pb := splitVersion(b)
	if len(pa) != len(pb) || len(pa) == 0 {
		return false
	}
	for i := 0; i < len(pa)-1; i++ {
		if pa[i] != pb[i] {
			return false
		}
	}
	return pb[len(pb)-1] == pa[len(pa)-1]+1
}

func splitVersion(v string) []int {
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, _ := strconv.Atoi(p)
		out = append(out, n)
	}
	return out
}

// compareVersion 按数值段比较，段数不同时短的视为更小（2.3 与 2.3.0 视为相等）。
func compareVersion(a, b string) int {
	pa := splitVersion(a)
	pb := splitVersion(b)
	n := len(pa)
	if len(pb) > n {
		n = len(pb)
	}
	for i := 0; i < n; i++ {
		x, y := 0, 0
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}
