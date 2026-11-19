package main

import (
	"strings"
)

// DDL 提取器：把一批 SQL 脚本还原成虚拟数据字典（VDD）。
//
// 复用 lex.go 的 tokenizer 与语句切分，独立于 analyze.go 的规则判定，
// 避免把「为判定规则而临时 tokenize 的结构」与「需要持久化的模型」耦合。
// 全程静态词法分析，不连库、不执行 SQL。

// BuildSchemaModel 从多文件文本构建 VDD。files 为 (path, content) 对。
//
// 模型级方言取「显式指定；未指定时取首个文件的实际判定结果」并全程锁定：
// 不允许后续文件（如路径恰好带 sqlite 字样的脚本）把整批模型从 MySQL 翻成
// 其他方言——那会让 ARCH005/ARCH006 等 MySQL 规则整组静默失效。
// 单文件的逐文件方言仍按各自检测值传给 extractFile，不受影响。
func BuildSchemaModel(files []batchInput, explicit Dialect) *SchemaModel {
	model := newSchemaModel(explicit)
	for _, f := range files {
		d := resolveDialect(explicit, f.Content, f.Path)
		if model.Dialect == "" {
			model.Dialect = d
		}
		extractFile(model, f.Path, f.Content, d)
	}
	if model.Dialect == "" {
		model.Dialect = DialectMySQL
	}
	return model
}

func extractFile(model *SchemaModel, path, raw string, d Dialect) {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	stmts := splitStatementsDialect(raw, d)
	for _, st := range stmts {
		toks := tokenizeDialect(st.Text, d)
		if len(toks) == 0 {
			continue
		}
		head := toks[0].Upper
		switch head {
		case "CREATE":
			extractCreate(model, path, st.Line, toks, d)
		case "ALTER":
			extractAlter(model, path, st.Line, toks, d)
		case "COMMENT":
			extractComment(model, path, st.Line, toks, d)
		case "RENAME":
			extractRename(model, path, st.Line, toks)
		case "SELECT", "WITH", "UPDATE", "DELETE", "INSERT", "REPLACE":
			extractPredicates(model, path, st.Line, toks)
		}
	}
}

// scanQualified 从 i 起扫描一个可能带 schema/库前缀的标识符（db.t、[dbo].[t]、`t`），
// 返回拼接原文与下一个未消费下标。
func scanQualified(toks []Token, i int) (string, int) {
	var parts []string
	for i < len(toks) {
		t := toks[i]
		if (t.Kind == "word" || t.Kind == "ident") &&
			(len(parts) == 0 || (i > 0 && toks[i-1].Kind == "op" && toks[i-1].Text == ".")) {
			parts = append(parts, t.Text)
			i++
			continue
		}
		if t.Kind == "op" && t.Text == "." && len(parts) > 0 {
			i++
			continue
		}
		break
	}
	return strings.Join(parts, "."), i
}

// extractRename 处理 RENAME TABLE a TO b, c TO d（多对用逗号分隔）。
func extractRename(model *SchemaModel, path string, line int, toks []Token) {
	if len(toks) < 2 || toks[1].Upper != "TABLE" {
		return
	}
	i := 2
	for i < len(toks) {
		src, next := scanQualified(toks, i)
		i = next
		if src == "" {
			break
		}
		dst := ""
		if i < len(toks) && toks[i].Kind == "word" && (toks[i].Upper == "TO" || toks[i].Upper == "AS") {
			dst, i = scanQualified(toks, i+1)
		}
		if dst != "" {
			oldKey, _ := normalizeTableName(src)
			newKey, newQual := normalizeTableName(dst)
			if oldKey != "" && newKey != "" {
				model.renameTable(oldKey, newKey, newQual)
			}
		}
		if i < len(toks) && toks[i].Kind == "op" && toks[i].Text == "," {
			i++
			continue
		}
		break
	}
}

// stmtKind 取 CREATE/ALTER 之后的对象类型（TABLE/INDEX/UNIQUE INDEX…）。
func stmtKind(toks []Token) string {
	skip := map[string]bool{"OR": true, "REPLACE": true, "TEMPORARY": true, "TEMP": true,
		"UNIQUE": true, "FULLTEXT": true, "SPATIAL": true, "DEFINER": true,
		"IF": true, "NOT": true, "EXISTS": true, "CONCURRENTLY": true, "ONLINE": true}
	for i := 1; i < len(toks) && i < 9; i++ {
		t := toks[i]
		if t.Kind != "word" {
			continue
		}
		if skip[t.Upper] {
			continue
		}
		return t.Upper
	}
	return ""
}

// firstIdent 取从 from 起第一个标识符（支持 db.t / [dbo].[t] / `t`）；
// 跳过 IF/NOT/EXISTS/OR/REPLACE/TEMPORARY 等从句关键字。
func firstIdent(toks []Token, from int) string {
	skip := map[string]bool{"IF": true, "NOT": true, "EXISTS": true, "OR": true,
		"REPLACE": true, "TEMPORARY": true, "TEMP": true, "UNIQUE": true,
		"TABLE": true, "INDEX": true, "KEY": true, "VIEW": true}
	for i := from; i < len(toks) && i < from+6; i++ {
		t := toks[i]
		if t.Kind == "word" && skip[t.Upper] {
			continue
		}
		if t.Kind == "word" || t.Kind == "ident" {
			name := t.Text
			// 拼接 db.t
			if i+2 < len(toks) && toks[i+1].Kind == "op" && toks[i+1].Text == "." {
				name = name + "." + toks[i+2].Text
			}
			return name
		}
	}
	return ""
}

func extractCreate(model *SchemaModel, path string, line int, toks []Token, d Dialect) {
	kind := stmtKind(toks)
	switch kind {
	case "TABLE":
		extractCreateTable(model, path, line, toks, d)
	case "INDEX", "UNIQUE", "KEY":
		extractCreateIndex(model, path, line, toks, d)
	}
}

// splitParenSegs 按顶层逗号切分括号内的 token 段。
func splitParenSegs(toks []Token) [][]Token {
	start := -1
	for i, t := range toks {
		if t.Kind == "op" && t.Text == "(" {
			start = i
			break
		}
	}
	if start < 0 {
		return nil
	}
	var segs [][]Token
	var seg []Token
	depth := 0
	for i := start; i < len(toks); i++ {
		t := toks[i]
		if t.Kind == "op" {
			switch t.Text {
			case "(":
				depth++
				if depth == 1 {
					continue
				}
			case ")":
				depth--
				if depth == 0 {
					segs = append(segs, seg)
					return segs
				}
			case ",":
				if depth == 1 {
					segs = append(segs, seg)
					seg = nil
					continue
				}
			}
		}
		if depth >= 1 {
			seg = append(seg, t)
		}
	}
	return segs
}

func extractCreateTable(model *SchemaModel, path string, line int, toks []Token, d Dialect) {
	name := firstIdent(toks, 1)
	key, qual := normalizeTableName(name)
	if key == "" {
		model.ParseErrors = append(model.ParseErrors, ParseNote{File: path, Line: line, Note: "CREATE TABLE 表名无法解析"})
		return
	}
	if _, exists := model.Tables[key]; exists {
		// 同名重复定义（ARCH032）；仍补记一次 CREATE，并把第二份定义合并进既有表，
		// 避免第二次 CREATE 只因为同名就被整体丢弃。
		model.markCreate(key, path, line)
		tbl := model.Tables[key]
		segs := splitParenSegs(toks)
		for _, seg := range segs {
			if len(seg) == 0 {
				continue
			}
			col := parseColumnSeg(seg, path, line, d)
			if col.Name != "" {
				if hasWord(seg, "PRIMARY") && hasWord(seg, "KEY") {
					tbl.PrimaryKey = append(tbl.PrimaryKey, col.Name)
				}
				tbl.AddColumn(col)
				extractColumnRef(model, key, col.Name, seg, path, line)
			}
		}
		return
	}
	tbl := model.AddTable(key, qual, path, line)
	model.markCreate(key, path, line)

	segs := splitParenSegs(toks)
	for _, seg := range segs {
		if len(seg) == 0 {
			continue
		}
		head := seg[0]
		hu := strings.ToUpper(head.Text)
		switch hu {
		case "PRIMARY":
			tbl.PrimaryKey = append(tbl.PrimaryKey, identList(seg)...)
		case "CONSTRAINT":
			if hasWord(seg, "PRIMARY") {
				tbl.PrimaryKey = append(tbl.PrimaryKey, identList(seg)...)
			}
			if hasWord(seg, "FOREIGN") {
				extractFKConstraint(model, tbl.Name, seg, path, line)
			}
		case "UNIQUE", "KEY", "INDEX", "FULLTEXT", "SPATIAL":
			cols := identList(seg)
			if len(cols) > 0 {
				idx := IndexDef{Name: "", Table: tbl.Name, Columns: cols,
					Unique: hu == "UNIQUE", Kind: kindToKind(hu), SourceFile: path, Line: line}
				// 索引名：KEY/INDEX 后紧跟的标识符
				for i, t := range seg {
					if t.Kind == "word" && (t.Upper == "KEY" || t.Upper == "INDEX") && i+1 < len(seg) {
						if seg[i+1].Kind == "word" || seg[i+1].Kind == "ident" {
							idx.Name = strings.ToLower(seg[i+1].Text)
						}
					}
				}
				model.AddIndex(idx)
			}
		case "FOREIGN":
			extractFKConstraint(model, tbl.Name, seg, path, line)
		default:
			col := parseColumnSeg(seg, path, line, d)
			if col.Name != "" {
				if hasWord(seg, "PRIMARY") && hasWord(seg, "KEY") {
					tbl.PrimaryKey = append(tbl.PrimaryKey, col.Name)
				}
				tbl.AddColumn(col)
				extractColumnRef(model, tbl.Name, col.Name, seg, path, line)
			}
		}
	}

	// 表级属性：ENGINE / CHARSET / COLLATE / COMMENT / PARTITION。
	// 只扫描主括号闭合之后的 token：列级 COMMENT '…'、CHARACTER SET 都出现在
	// 括号内，扫全文会把列注释/列字符集误挂到表上（如列注释覆盖表 COMMENT，
	// 造成 ARCH004 漏报）。
	attrFrom := 0
	{
		depth := 0
		opened := false
		for i, t := range toks {
			if t.Kind != "op" {
				continue
			}
			switch t.Text {
			case "(":
				depth++
				opened = true
			case ")":
				depth--
				if opened && depth == 0 {
					attrFrom = i + 1
				}
			}
		}
	}
	for i := attrFrom; i < len(toks); i++ {
		t := toks[i]
		if t.Kind != "word" {
			continue
		}
		switch t.Upper {
		case "ENGINE":
			if v := valueAfter(toks, i); v != "" {
				tbl.Engine = strings.ToLower(v)
			}
		case "CHARSET", "CHARACTER":
			if t.Upper == "CHARACTER" && i+1 < len(toks) && toks[i+1].Upper == "SET" {
				if v := valueAfter(toks, i+1); v != "" {
					tbl.Charset = strings.ToLower(v)
				}
			} else if v := valueAfter(toks, i); v != "" {
				tbl.Charset = strings.ToLower(v)
			}
		case "COLLATE", "COLLATION":
			if v := valueAfter(toks, i); v != "" {
				tbl.Collation = strings.ToLower(v)
			}
		case "COMMENT":
			j := i + 1
			// 兼容 COMMENT 'x' 与 COMMENT='x' 两种写法
			if j < len(toks) && toks[j].Kind == "op" && toks[j].Text == "=" {
				j++
			}
			if j < len(toks) && toks[j].Kind == "str" {
				tbl.Comment = toks[j].Text
			}
		case "PARTITION":
			tbl.Partitioned = true
		}
	}
}

func hasWord(seg []Token, up string) bool {
	for _, t := range seg {
		if t.Kind == "word" && t.Upper == up {
			return true
		}
	}
	return false
}

func kindToKind(hu string) string {
	switch hu {
	case "FULLTEXT":
		return "FULLTEXT"
	case "SPATIAL":
		return "SPATIAL"
	case "UNIQUE":
		return "BTREE"
	}
	return "BTREE"
}

// parseColumnSeg 解析列定义段。
func parseColumnSeg(seg []Token, path string, line int, d Dialect) ColumnDef {
	col := ColumnDef{SourceFile: path, Line: line, Nullable: true}
	if len(seg) == 0 {
		return col
	}
	col.Name = seg[0].Text
	// 找类型：跳过 NULL/NOT NULL/PRIMARY/KEY/DEFAULT 等关键字。
	typeStart := 1
	stopTypes := map[string]bool{"NOT": true, "NULL": true, "PRIMARY": true, "KEY": true,
		"DEFAULT": true, "AUTO_INCREMENT": true, "IDENTITY": true, "COMMENT": true,
		"UNIQUE": true, "REFERENCES": true, "ON": true, "CHECK": true, "CONSTRAINT": true}
	for typeStart < len(seg) && seg[typeStart].Kind != "word" {
		typeStart++
	}
	for typeStart < len(seg) && seg[typeStart].Kind == "word" && stopTypes[seg[typeStart].Upper] {
		typeStart++
	}
	if typeStart < len(seg) {
		col.RawType = typeWithArgs(seg, typeStart)
		col.Type = col.RawType
	}
	for i, t := range seg {
		switch t.Upper {
		case "NOT":
			col.Nullable = false
		case "NULL":
			// 紧跟 NOT 已处理；单独 NULL 保持可空
		case "AUTO_INCREMENT":
			col.AutoIncrement = true
		case "IDENTITY":
			col.Identity = true
		case "DEFAULT":
			col.Nullable = false
			if i+1 < len(seg) {
				col.Default = seg[i+1].Text
			}
		case "COMMENT":
			if i+1 < len(seg) && seg[i+1].Kind == "str" {
				col.Comment = seg[i+1].Text
			}
		}
	}
	// 长度/精度粗略提取
	if lp := typeLenPrec(col.RawType); lp != "" {
		_ = lp
	}
	return col
}

// typeLenPrec 预留：从类型串里提取长度（当前规则不强制使用，保留接口）。
func typeLenPrec(raw string) string {
	return raw
}

// 被引用的方言分词函数，避免未使用告警。
var _ = strings.TrimSpace

func extractCreateIndex(model *SchemaModel, path string, line int, toks []Token, d Dialect) {
	// CREATE [UNIQUE] INDEX name ON table (cols)
	idx := IndexDef{SourceFile: path, Line: line}
	idx.Unique = hasWord(toks, "UNIQUE")
	name, tbl := "", ""
	onIdx := -1
	for i, t := range toks {
		if t.Kind == "word" && t.Upper == "ON" {
			onIdx = i
			break
		}
	}
	// 索引名：INDEX/KEY 后第一个标识符
	for i, t := range toks {
		if t.Kind == "word" && (t.Upper == "INDEX" || t.Upper == "KEY") && i+1 < len(toks) {
			if segName := nextIdent(toks, i+1); segName != "" {
				name = strings.ToLower(segName)
			}
		}
	}
	if onIdx >= 0 {
		tbl = firstIdent(toks, onIdx+1)
	}
	idx.Name = name
	if tbl != "" {
		key, _ := normalizeTableName(tbl)
		idx.Table = key
	}
	idx.Columns = identList(toks)
	if len(idx.Columns) > 0 {
		model.AddIndex(idx)
	}
}

func nextIdent(toks []Token, from int) string {
	for i := from; i < len(toks) && i < from+4; i++ {
		t := toks[i]
		if t.Kind == "word" || t.Kind == "ident" {
			return t.Text
		}
	}
	return ""
}

func extractFKConstraint(model *SchemaModel, fromTable string, seg []Token, path string, line int) {
	// CONSTRAINT fk FOREIGN KEY (cols) REFERENCES other_table (cols) [ON DELETE ...]
	ref := Reference{FromTable: fromTable, Kind: "FK", SourceFile: path, Line: line}
	inCols := false
	inRefs := false
	collectRefs := false // REFERENCES 之后进入引用列括号才收集，跳过表名本身
	colBuf := []string{}
	refColBuf := []string{}
	onDelete := ""
	for i, t := range seg {
		switch t.Upper {
		case "FOREIGN":
			inCols = true
		case "REFERENCES":
			inCols = false
			inRefs = true
			if i+1 < len(seg) {
				ref.ToTable, _ = normalizeTableName(seg[i+1].Text)
			}
		case "ON":
			if i+2 < len(seg) && seg[i+1].Upper == "DELETE" {
				onDelete = strings.ToLower(seg[i+2].Text)
			}
		case "(":
			if inRefs {
				collectRefs = true
			}
			continue
		case ")":
			collectRefs = false
			continue
		default:
			if t.Kind == "word" || t.Kind == "ident" {
				switch t.Upper {
				case "KEY", "CONSTRAINT", "FOREIGN", "REFERENCES", "ON", "DELETE", "CASCADE", "RESTRICT", "SET", "NULL", "UPDATE":
					continue
				}
				if inCols {
					colBuf = append(colBuf, strings.ToLower(t.Text))
				} else if inRefs && collectRefs {
					refColBuf = append(refColBuf, strings.ToLower(t.Text))
				}
			}
		}
	}
	// 多列外键保留完整列清单（逗号分隔）；单列时与旧行为一致。
	if len(colBuf) > 0 {
		ref.FromColumns = strings.Join(colBuf, ",")
	}
	if len(refColBuf) > 0 {
		ref.ToColumns = strings.Join(refColBuf, ",")
	}
	ref.OnDelete = onDelete
	if ref.ToTable != "" {
		model.AddReference(ref)
	}
}

// extractColumnRef 解析列级 REFERENCES target [(col)]（SQLite / 标准 SQL 外键写法），
// 让 ARCH020/025/027/028 不再对这类脚本全盲。
func extractColumnRef(model *SchemaModel, fromTable, colName string, seg []Token, path string, line int) {
	for i, t := range seg {
		if t.Kind == "word" && t.Upper == "REFERENCES" && i+1 < len(seg) {
			ref := Reference{FromTable: fromTable, Kind: "FK", SourceFile: path, Line: line}
			ref.FromColumns = strings.ToLower(colName)
			ref.ToTable, _ = normalizeTableName(seg[i+1].Text)
			if i+2 < len(seg) && seg[i+2].Kind == "op" && seg[i+2].Text == "(" && i+3 < len(seg) {
				ref.ToColumns = strings.ToLower(seg[i+3].Text)
			}
			if ref.ToTable != "" {
				model.AddReference(ref)
			}
			return
		}
	}
}

func extractAlter(model *SchemaModel, path string, line int, toks []Token, d Dialect) {
	if len(toks) < 3 || toks[1].Upper != "TABLE" {
		return
	}
	name := firstIdent(toks, 2)
	key, qual := normalizeTableName(name)
	if key == "" {
		return
	}
	tbl, ok := model.Tables[key]
	if !ok {
		// ALTER 一张脚本内尚未 CREATE 的表：先占位，后续 CREATE 再补列。
		tbl = model.AddTable(key, qual, path, line)
	}

	u := " " + strings.ToUpper(joinText(toks)) + " "
	segs := splitAlterActions(toks)
	for _, seg := range segs {
		if len(seg) == 0 {
			continue
		}
		head := seg[0].Upper
		switch head {
		case "ADD":
			sub := nextWord(seg, 1)
			switch sub {
			case "COLUMN":
				col := parseColumnSeg(restAfter(seg, 2), path, line, d)
				if col.Name != "" {
					col.AddedByAlter = true
					tbl.AddColumn(col)
					extractColumnRef(model, tbl.Name, col.Name, seg, path, line)
				}
			case "PRIMARY", "CONSTRAINT", "FOREIGN", "UNIQUE", "INDEX", "KEY":
				if sub == "FOREIGN" || (sub == "CONSTRAINT" && hasWord(seg, "FOREIGN")) {
					extractFKConstraint(model, tbl.Name, seg, path, line)
				}
				if sub == "PRIMARY" {
					tbl.PrimaryKey = append(tbl.PrimaryKey, identList(seg)...)
				}
				if sub == "INDEX" || sub == "KEY" || sub == "UNIQUE" {
					idx := IndexDef{Table: tbl.Name, Columns: identList(seg),
						Unique: sub == "UNIQUE", SourceFile: path, Line: line}
					model.AddIndex(idx)
				}
			default:
				// ADD <col_def>（省略 COLUMN 关键字）；排除 ADD CHECK 等非列动作
				if len(seg) > 1 && seg[1].Kind == "word" && nonColumnAddTargets[seg[1].Upper] {
					break
				}
				col := parseColumnSeg(restAfter(seg, 1), path, line, d)
				if col.Name != "" {
					col.AddedByAlter = true
					tbl.AddColumn(col)
				}
			}
		case "MODIFY":
			if sub := nextWord(seg, 1); sub == "COLUMN" {
				col := parseColumnSeg(restAfter(seg, 2), path, line, d)
				if col.Name != "" {
					col.AddedByAlter = true
					tbl.AddColumn(col)
					extractColumnRef(model, tbl.Name, col.Name, seg, path, line)
				}
			} else {
				col := parseColumnSeg(restAfter(seg, 1), path, line, d)
				if col.Name != "" {
					col.AddedByAlter = true
					tbl.AddColumn(col)
					extractColumnRef(model, tbl.Name, col.Name, seg, path, line)
				}
			}
		case "CHANGE":
			// CHANGE [COLUMN] old new <type>…：重命名 + 重定义。
			// 旧实现把「新列名」当类型解析（name=旧名、type=新名片段），
			// 现按 old/new 显式拆分。
			idx := 1
			if nextWord(seg, 1) == "COLUMN" {
				idx = 2
			}
			if len(seg) > idx+1 && (seg[idx].Kind == "word" || seg[idx].Kind == "ident") {
				oldName := seg[idx].Text
				col := parseColumnSeg(restAfter(seg, idx+1), path, line, d)
				if col.Name != "" {
					col.AddedByAlter = true
					tbl.RenameColumn(oldName, col.Name)
					tbl.AddColumn(col)
				}
			}
		case "RENAME":
			// ALTER TABLE a RENAME TO b / RENAME AS b / RENAME a TO b
			newKey, newQual := "", ""
			for i, t := range seg {
				if t.Kind == "word" && (t.Upper == "TO" || t.Upper == "AS") && i+1 < len(seg) {
					if seg[i+1].Kind == "word" || seg[i+1].Kind == "ident" {
						newKey, newQual = normalizeTableName(seg[i+1].Text)
					}
				}
			}
			if newKey != "" && newKey != key {
				model.renameTable(key, newKey, newQual)
			}
		case "ALTER":
			// ALTER COLUMN x {SET DEFAULT v | DROP DEFAULT | SET NOT NULL | DROP NOT NULL}：
			// 只做有限字段合并。整段重解析会把 SET/DROP 当成列类型污染 VDD
			//（如 code 的 RawType 变成 "SET"，进而触发假的类型不一致）。
			if nextWord(seg, 1) == "COLUMN" {
				colName := ""
				if len(seg) > 2 && (seg[2].Kind == "word" || seg[2].Kind == "ident") {
					colName = seg[2].Text
				}
				if c := tbl.Column(colName); c != nil {
					c.AddedByAlter = true
					if hasWord(seg, "DEFAULT") {
						if hasWord(seg, "DROP") {
							c.Default = ""
						} else {
							for k, tk := range seg {
								if tk.Upper == "DEFAULT" && k+1 < len(seg) {
									c.Default = seg[k+1].Text
									break
								}
							}
						}
					}
					if hasWord(seg, "SET") && hasWord(seg, "NOT") && hasWord(seg, "NULL") {
						c.Nullable = false
					}
					if hasWord(seg, "DROP") && hasWord(seg, "NOT") && hasWord(seg, "NULL") {
						c.Nullable = true
					}
				}
			}
		case "DROP":
			// DROP COLUMN / DROP INDEX / DROP PK：从模型移除（静态近似）
			sub := nextWord(seg, 1)
			if sub == "COLUMN" && len(seg) > 2 {
				dropCol(tbl, seg[2].Text)
			}
		}
	}
	_ = u
}

func dropCol(tbl *TableDef, name string) {
	key := strings.ToLower(name)
	out := tbl.Columns[:0]
	for _, c := range tbl.Columns {
		if strings.ToLower(c.Name) != key {
			out = append(out, c)
		}
	}
	tbl.Columns = out
}

// splitAlterActions 按顶层逗号切分 ALTER 的多个动作。
func splitAlterActions(toks []Token) [][]Token {
	// 跳过 ALTER TABLE <name>，从第一个动作开始
	start := 2
	for start < len(toks) && toks[start].Kind != "word" {
		start++
	}
	// 表名之后可能跟 AS/ON/COMMENT 等，动作以 ADD/MODIFY/CHANGE/ALTER/DROP/RENAME 开头。
	actionStart := -1
	for i := start; i < len(toks); i++ {
		if toks[i].Kind == "word" && actionHeads[toks[i].Upper] {
			actionStart = i
			break
		}
	}
	if actionStart >= 0 {
		start = actionStart
	}
	var segs [][]Token
	var seg []Token
	depth := 0
	for i := start; i < len(toks); i++ {
		t := toks[i]
		if t.Kind == "op" {
			switch t.Text {
			case "(":
				depth++
			case ")":
				if depth > 0 {
					depth--
				}
			case ",":
				if depth == 0 {
					segs = append(segs, seg)
					seg = nil
					continue
				}
			}
		}
		seg = append(seg, t)
	}
	if len(seg) > 0 {
		segs = append(segs, seg)
	}
	return segs
}

var actionHeads = map[string]bool{
	"ADD": true, "MODIFY": true, "CHANGE": true, "ALTER": true,
	"DROP": true, "RENAME": true, "RENAME COLUMN": true, "SET": true, "RENAME INDEX": true,
}

// nonColumnAddTargets：ALTER … ADD 后面跟的不是列定义的关键字。
var nonColumnAddTargets = map[string]bool{
	"CHECK": true, "PARTITION": true, "PERIOD": true,
}

func nextWord(seg []Token, from int) string {
	for i := from; i < len(seg) && i < from+3; i++ {
		if seg[i].Kind == "word" {
			return seg[i].Upper
		}
	}
	return ""
}

func restAfter(seg []Token, n int) []Token {
	if n >= len(seg) {
		return nil
	}
	return seg[n:]
}

func joinText(toks []Token) string {
	parts := make([]string, 0, len(toks))
	for _, t := range toks {
		parts = append(parts, t.Text)
	}
	return strings.Join(parts, " ")
}

func extractComment(model *SchemaModel, path string, line int, toks []Token, d Dialect) {
	// COMMENT ON TABLE t IS '...' / COMMENT ON COLUMN t.c IS '...'
	// 只把注释挂到表/列上，注释内容不影响规则判定。
	target := ""
	for i, t := range toks {
		if t.Kind == "word" && (t.Upper == "TABLE" || t.Upper == "COLUMN") {
			if i+1 < len(toks) {
				target = toks[i+1].Text
			}
		}
	}
	if target == "" {
		return
	}
	comment := ""
	for _, t := range toks {
		if t.Kind == "str" {
			comment = t.Text
		}
	}
	key, _ := normalizeTableName(target)
	if tbl, ok := model.Tables[key]; ok {
		if strings.Contains(target, ".") {
			colName := strings.ToLower(strings.TrimSpace(strings.Split(target, ".")[1]))
			if c := tbl.Column(colName); c != nil {
				c.Comment = comment
			}
		} else {
			tbl.Comment = comment
		}
	}
}

// aliasStopWords：FROM/JOIN 后面跟的不是别名的关键字。
var aliasStopWords = map[string]bool{
	"WHERE": true, "GROUP": true, "ORDER": true, "ON": true, "JOIN": true,
	"LEFT": true, "RIGHT": true, "INNER": true, "OUTER": true, "CROSS": true,
	"FULL": true, "SET": true, "LIMIT": true, "HAVING": true, "UNION": true,
	"USING": true, "SELECT": true, "WHEN": true, "AS": true,
}

// extractPredicates 从 SELECT/DML 收集 WHERE/JOIN/ORDER BY 引用的列。
// 表归属解析 FROM/JOIN 的别名映射：`FROM orders o … o.status` 归还到 orders，
// 避免把别名当表名产生假 ARCH023。
func extractPredicates(model *SchemaModel, path string, line int, toks []Token) {
	alias := map[string]string{} // alias(lower) -> table key(lower)
	resolveTable := func(q string) string {
		if tk, ok := alias[strings.ToLower(q)]; ok {
			return tk
		}
		return strings.ToLower(q)
	}
	region := "" // where / join / order
	depth := 0
	for i, t := range toks {
		if t.Kind == "op" {
			switch t.Text {
			case "(":
				depth++
			case ")":
				if depth > 0 {
					depth--
				}
			}
			continue
		}
		if depth != 0 {
			continue
		}
		if t.Kind != "word" {
			continue
		}
		// 别名登记：FROM/JOIN <qualified> [AS] <alias>
		if t.Upper == "FROM" || t.Upper == "JOIN" {
			if name, j := scanQualified(toks, i+1); name != "" {
				if j < len(toks) && toks[j].Kind == "word" && toks[j].Upper == "AS" {
					j++
				}
				if j < len(toks) && (toks[j].Kind == "word" || toks[j].Kind == "ident") && !aliasStopWords[toks[j].Upper] {
					tk, _ := normalizeTableName(name)
					if tk != "" {
						alias[strings.ToLower(toks[j].Text)] = tk
					}
				}
			}
		}
		switch t.Upper {
		case "WHERE":
			region = "where"
		case "JOIN", "INNER", "LEFT", "RIGHT", "FULL":
			if t.Upper == "JOIN" || t.Upper == "INNER" {
				region = "join"
			}
		case "ORDER":
			region = "order"
		case "GROUP", "HAVING", "LIMIT", "UNION":
			region = ""
		}
		if region == "" {
			continue
		}
		// 列引用：col 或 alias.col（跳过关键字/字面量/参数）
		if t.Upper == "WHERE" || t.Upper == "JOIN" || t.Upper == "ORDER" || t.Upper == "ON" ||
			t.Upper == "AND" || t.Upper == "OR" || t.Upper == "BY" || t.Upper == "IN" ||
			t.Upper == "LIKE" || t.Upper == "IS" || t.Upper == "NOT" || t.Upper == "NULL" ||
			t.Upper == "BETWEEN" || t.Upper == "AS" || t.Upper == "SELECT" {
			continue
		}
		// 限定符本身（alias./table. 的前半段）不是列，跳过
		if i+1 < len(toks) && toks[i+1].Kind == "op" && toks[i+1].Text == "." {
			continue
		}
		if t.Kind == "var" || t.Kind == "num" || t.Kind == "str" {
			continue
		}
		col := strings.ToLower(t.Text)
		table := ""
		if i > 0 && toks[i-1].Kind == "op" && toks[i-1].Text == "." {
			if i-2 >= 0 {
				table = resolveTable(toks[i-2].Text)
			}
		}
		if col == "" {
			continue
		}
		model.Predicates = append(model.Predicates, PredicateUsage{
			Table: table, Column: col,
			Where: region == "where", Join: region == "join", Order: region == "order",
		})
	}
}
