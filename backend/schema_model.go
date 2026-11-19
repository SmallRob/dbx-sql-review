package main

import "strings"

// 虚拟数据字典（VDD, Virtual Data Dictionary）。
//
// 设计依据 docs/v0.2.0-design.md §2：把整个批次的 DDL 汇总成一份逻辑模型，
// 作为架构评审的唯一事实源。全程静态词法分析——不连库、不执行被评审 SQL、
// 不发起网络请求。

// TableDef 是一张表在 VDD 中的最终状态（已合并跨文件 ALTER）。
type TableDef struct {
	Name          string      `json:"name"`
	QualifiedName string      `json:"qualifiedName"`
	Columns       []ColumnDef `json:"columns"`
	PrimaryKey    []string    `json:"primaryKey"`
	Charset       string      `json:"charset,omitempty"`
	Collation     string      `json:"collation,omitempty"`
	Engine        string      `json:"engine,omitempty"`
	Comment       string      `json:"comment,omitempty"`
	Partitioned   bool        `json:"partitioned,omitempty"`
	SourceFile    string      `json:"sourceFile"`
	Line          int         `json:"line"`
}

// ColumnDef 是一列的定义。
type ColumnDef struct {
	Name          string `json:"name"`
	Type          string `json:"type"`
	RawType       string `json:"rawType"`
	Nullable      bool   `json:"nullable"`
	AutoIncrement bool   `json:"autoIncrement,omitempty"`
	Identity      bool   `json:"identity,omitempty"`
	Default       string `json:"default,omitempty"`
	Comment       string `json:"comment,omitempty"`
	Length        int    `json:"length,omitempty"`
	Precision     int    `json:"precision,omitempty"`
	Scale         int    `json:"scale,omitempty"`
	SourceFile    string `json:"sourceFile,omitempty"`
	Line          int    `json:"line,omitempty"`
	// AddedByAlter 标记该列由 ALTER ADD/MODIFY/CHANGE 引入（ARCH029 只对这类列生效，
	// CREATE TABLE 的 NOT NULL 列本就没有存量数据，不参与判定）。不序列化到报告。
	AddedByAlter bool `json:"-"`
}

// IndexDef 是一个索引/唯一约束。
type IndexDef struct {
	Name       string   `json:"name"`
	Table      string   `json:"table"`
	Columns    []string `json:"columns"`
	Unique     bool     `json:"unique,omitempty"`
	Kind       string   `json:"kind,omitempty"` // BTREE / HASH / GIN / FULLTEXT / SPATIAL…
	SourceFile string   `json:"sourceFile"`
	Line       int      `json:"line"`
}

// Reference 是表间关系：显式外键或命名推断出的隐式引用。
type Reference struct {
	FromTable   string `json:"fromTable"`
	FromColumns string `json:"fromColumns"`
	ToTable     string `json:"toTable"`
	ToColumns   string `json:"toColumns"`
	Kind        string `json:"kind"` // FK / IMPLICIT
	OnDelete    string `json:"onDelete,omitempty"`
	SourceFile  string `json:"sourceFile"`
	Line        int    `json:"line"`
}

// ObjectSource 记录对象首次出现的源文件与行号，供报告下钻。
type ObjectSource struct {
	Kind string `json:"kind"` // table / index
	Name string `json:"name"`
	File string `json:"file"`
	Line int    `json:"line"`
}

// ParseNote 记录一条未能完整解析的语句（不阻断评审）。
type ParseNote struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Note string `json:"note"`
}

// PredicateUsage 记录一条 DML/SELECT 语句里出现在 WHERE/JOIN/ORDER BY 的列，
// 供 ARCH023 谓词覆盖缺口判定。
type PredicateUsage struct {
	Table  string
	Column string
	Where  bool
	Join   bool
	Order  bool
}

// SchemaModel 是整批文件汇总出的逻辑模型。
type SchemaModel struct {
	Dialect     Dialect              `json:"dialect"`
	Tables      map[string]*TableDef `json:"tables"`
	Indexes     []IndexDef           `json:"indexes"`
	Refs        []Reference          `json:"refs"`
	ObjectSrc   []ObjectSource       `json:"objectSrc"`
	ParseErrors []ParseNote          `json:"parseErrors,omitempty"`
	Predicates  []PredicateUsage     `json:"-"`

	// seenCreate 记录表是否已被 CREATE 过，用于 ARCH032 重复定义检测。
	seenCreate map[string][]ObjectSource `json:"-"`
}

// SchemaFinding 是架构评审的一条命中。
type SchemaFinding struct {
	RuleID      string         `json:"ruleId"`
	Severity    string         `json:"severity"`
	Category    string         `json:"category"`
	Title       string         `json:"title"`
	Message     string         `json:"message"`
	Advice      string         `json:"advice"`
	Tables      []string       `json:"tables,omitempty"`
	Files       []string       `json:"files,omitempty"`
	Evidence    map[string]any `json:"evidence,omitempty"`
	PracticeIDs []string       `json:"practiceIds,omitempty"`
}

// newSchemaModel 初始化模型内部集合。
func newSchemaModel(d Dialect) *SchemaModel {
	return &SchemaModel{
		Dialect:    d,
		Tables:     map[string]*TableDef{},
		seenCreate: map[string][]ObjectSource{},
	}
}

// normalizeIdent 把各种方言的表/列标识符收敛成小写 key，同时保留原文。
// 兼容：`t`、[dbo].[t]、"schema"."t"、db.t、"t"。
// 返回 (规范化 key, 原文)。
func normalizeIdent(name string, dialect Dialect) (string, string) {
	trimmed := strings.TrimSpace(name)
	trimmed = strings.Trim(trimmed, "`\"'[] ")
	// 去掉 schema/库前缀，只保留对象名作为 key；QualifiedName 保留原文。
	if i := strings.LastIndex(trimmed, "."); i >= 0 {
		schema := strings.Trim(trimmed[:i], "`\"'[] ")
		obj := strings.Trim(trimmed[i+1:], "`\"'[] ")
		return strings.ToLower(obj), schema + "." + obj
	}
	return strings.ToLower(trimmed), trimmed
}

// normalizeTableName 规范化表名；返回 key 与展示用原文。
func normalizeTableName(raw string) (string, string) {
	return normalizeIdent(raw, "")
}

// AddTable 登记一张表；已存在时返回既有定义（ALTER 合并走 MergeAlter）。
// returns whether the table was newly created.
func (m *SchemaModel) AddTable(key, qualified, file string, line int) *TableDef {
	if t, ok := m.Tables[key]; ok {
		return t
	}
	t := &TableDef{Name: key, QualifiedName: qualified, SourceFile: file, Line: line}
	m.Tables[key] = t
	m.ObjectSrc = append(m.ObjectSrc, ObjectSource{Kind: "table", Name: key, File: file, Line: line})
	// seenCreate 只由 extractCreateTable 的 markCreate 登记；ALTER 占位建表不算 CREATE，
	// 否则每张正常建表都会被双重计数成 ARCH032「重复定义」。
	return t
}

// markCreate 记录一次 CREATE TABLE（用于 ARCH032 重复定义检测）。
func (m *SchemaModel) markCreate(key, file string, line int) {
	m.seenCreate[key] = append(m.seenCreate[key], ObjectSource{Kind: "table", Name: key, File: file, Line: line})
}

// AddColumn 向表追加一列；同名列已存在时更新其定义（按文件顺序合并）。
func (t *TableDef) AddColumn(c ColumnDef) {
	key := strings.ToLower(c.Name)
	for i := range t.Columns {
		if strings.ToLower(t.Columns[i].Name) == key {
			t.Columns[i] = c
			return
		}
	}
	t.Columns = append(t.Columns, c)
}

// Column 按规范化名取列定义。
func (t *TableDef) Column(name string) *ColumnDef {
	for i := range t.Columns {
		if strings.EqualFold(t.Columns[i].Name, name) {
			return &t.Columns[i]
		}
	}
	return nil
}

// RenameColumn 处理 CHANGE COLUMN 的重命名语义：把旧列（含主键清单里的引用）
// 改成新名，随后 AddColumn 按新名合并新定义。
func (t *TableDef) RenameColumn(old, new string) {
	for i := range t.Columns {
		if strings.EqualFold(t.Columns[i].Name, old) {
			t.Columns[i].Name = new
			break
		}
	}
	for i, pk := range t.PrimaryKey {
		if strings.EqualFold(pk, old) {
			t.PrimaryKey[i] = new
		}
	}
}

// AddIndex 登记索引。
func (m *SchemaModel) AddIndex(idx IndexDef) {
	m.Indexes = append(m.Indexes, idx)
	m.ObjectSrc = append(m.ObjectSrc, ObjectSource{Kind: "index", Name: idx.Name, File: idx.SourceFile, Line: idx.Line})
}

// AddReference 登记外键/隐式引用。
func (m *SchemaModel) AddReference(r Reference) {
	m.Refs = append(m.Refs, r)
}

// renameTable 把表迁移到新键名（RENAME TO / RENAME TABLE a TO b），
// 并同步索引与外键引用里的旧表名；目标键已被占用时跳过，避免覆盖既有定义。
func (m *SchemaModel) renameTable(oldKey, newKey, qualified string) {
	t, ok := m.Tables[oldKey]
	if !ok || oldKey == newKey {
		return
	}
	if _, clash := m.Tables[newKey]; clash {
		return
	}
	delete(m.Tables, oldKey)
	t.Name = newKey
	if qualified != "" {
		t.QualifiedName = qualified
	}
	m.Tables[newKey] = t
	for i := range m.Indexes {
		if strings.EqualFold(m.Indexes[i].Table, oldKey) {
			m.Indexes[i].Table = newKey
		}
	}
	for i := range m.Refs {
		if strings.EqualFold(m.Refs[i].FromTable, oldKey) {
			m.Refs[i].FromTable = newKey
		}
		if strings.EqualFold(m.Refs[i].ToTable, oldKey) {
			m.Refs[i].ToTable = newKey
		}
	}
}

// TablesOf 收集某表在模型里的索引。
func (m *SchemaModel) indexesOf(table string) []IndexDef {
	out := []IndexDef{}
	for _, idx := range m.Indexes {
		if strings.EqualFold(idx.Table, table) {
			out = append(out, idx)
		}
	}
	return out
}

// columnTypeNormalize 把类型名归一化以便跨表比较：去括号参数、小写、去空格。
func columnTypeNormalize(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if i := strings.Index(s, "("); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, " ", "")
	// 同义类型归一，避免 int/integer、bigint 之类被误判为不一致。
	switch s {
	case "integer", "int4":
		return "int"
	case "bigint", "int8":
		return "bigint"
	case "smallint", "int2":
		return "smallint"
	case "varchar", "charactervarying":
		return "varchar"
	case "char":
		return "char"
	case "numeric", "decimal":
		return "decimal"
	case "float4", "real":
		return "float"
	case "float8", "doubleprecision", "double":
		return "double"
	case "bool", "boolean":
		return "bool"
	case "timestamp", "timestampwithouttimezone":
		return "timestamp"
	case "timestamptz", "timestampwithtimezone":
		return "timestamptz"
	}
	return s
}
