package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSchemaModelMySQLDDL(t *testing.T) {
	files := []batchInput{{Path: "V1__init.sql", Content: `
CREATE TABLE orders (
  id BIGINT NOT NULL PRIMARY KEY AUTO_INCREMENT,
  order_no VARCHAR(32) NOT NULL,
  amount DOUBLE,
  buyer_id BIGINT NOT NULL,
  created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
  remark TEXT,
  KEY idx_buyer (buyer_id),
  CONSTRAINT fk_buyer FOREIGN KEY (buyer_id) REFERENCES users(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE order_items (
  id BIGINT NOT NULL PRIMARY KEY,
  order_id BIGINT NOT NULL,
  price DECIMAL(12,2) NOT NULL
);
`}}
	model := BuildSchemaModel(files, DialectMySQL)
	if len(model.Tables) != 2 {
		t.Fatalf("应解析出 2 张表，实际 %d", len(model.Tables))
	}
	orders := model.Tables["orders"]
	if orders == nil {
		t.Fatal("orders 表缺失")
	}
	if len(orders.PrimaryKey) == 0 {
		t.Error("orders 应识别到主键")
	}
	if len(orders.Columns) != 6 {
		t.Errorf("orders 列数应为 6，实际 %d", len(orders.Columns))
	}
	if orders.Engine != "innodb" || orders.Charset != "utf8mb4" {
		t.Errorf("表级属性未解析: engine=%s charset=%s", orders.Engine, orders.Charset)
	}
	if len(model.Refs) != 1 || model.Refs[0].ToTable != "users" {
		t.Errorf("外键未解析: %+v", model.Refs)
	}
	if len(model.Indexes) != 1 {
		t.Errorf("索引数应为 1（orders.idx_buyer），实际 %d", len(model.Indexes))
	}
}

func TestSchemaReviewFindings(t *testing.T) {
	files := []batchInput{{Path: "V1__init.sql", Content: `
CREATE TABLE users (
  user_id BIGINT NOT NULL,
  amount DOUBLE,
  status VARCHAR(8),
  created_at TIMESTAMP
) ENGINE=MyISAM DEFAULT CHARSET=latin1;

CREATE TABLE users (
  id BIGINT NOT NULL PRIMARY KEY
);
`}}
	model := BuildSchemaModel(files, DialectMySQL)
	report := RunSchemaReview(model, defaultConfig().Rules, true)

	ids := map[string]bool{}
	for _, f := range report.Findings {
		ids[f.RuleID] = true
	}
	// ARCH005 字符集非 utf8mb4；ARCH006 引擎非 innodb；ARCH011 金额浮点；
	// ARCH032 重复定义；第二份定义补回主键，故第一张表不再报 ARCH001。
	for _, want := range []string{"ARCH005", "ARCH006", "ARCH011", "ARCH032"} {
		if !ids[want] {
			t.Errorf("应命中 %s，实际命中：%v", want, keys(ids))
		}
	}
	if report.Score < 0 || report.Score > 100 {
		t.Errorf("架构分越界: %d", report.Score)
	}
	if report.Stats.Tables != 1 {
		t.Errorf("统计表数应为 1（重复定义合并），实际 %d", report.Stats.Tables)
	}
	if report.Stats.Findings != len(report.Findings) {
		t.Error("Stats.Findings 与 Findings 数量不一致")
	}
	// 评分必须独立：架构报告不改动既有六维质量分（这里只校验结构可序列化）。
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("报告序列化失败: %v", err)
	}
	if !strings.Contains(string(raw), "score") || !strings.Contains(string(raw), "findings") {
		t.Error("JSON 缺少核心字段")
	}
}

func TestSchemaReviewDisabledByConfig(t *testing.T) {
	files := []batchInput{{Path: "V1.sql", Content: `CREATE TABLE t (id BIGINT);`}}
	cfg := defaultConfig().Rules
	cfg.Disabled = []string{"ARCH001"}
	model := BuildSchemaModel(files, DialectMySQL)
	report := RunSchemaReview(model, cfg, true)
	for _, f := range report.Findings {
		if f.RuleID == "ARCH001" {
			t.Error("被禁用的 ARCH001 不应命中")
		}
	}
}

func TestSchemaReviewPostgresTypes(t *testing.T) {
	files := []batchInput{{Path: "V1.sql", Content: `
CREATE TABLE events (
  id BIGINT PRIMARY KEY,
  payload JSON,
  created_at TIMESTAMP WITHOUT TIME ZONE
);
`}}
	model := BuildSchemaModel(files, DialectPostgreSQL)
	report := RunSchemaReview(model, defaultConfig().Rules, true)
	ids := map[string]bool{}
	for _, f := range report.Findings {
		ids[f.RuleID] = true
	}
	if !ids["ARCH014"] {
		t.Error("PG 无时区时间列应命中 ARCH014")
		for _, f := range report.Findings {
			t.Logf("%s: %s", f.RuleID, f.Message)
		}
	}
	if !ids["ARCH015"] {
		t.Error("PG JSON 应命中 ARCH015（建议 JSONB）")
	}
}

func TestSchemaCycleFK(t *testing.T) {
	files := []batchInput{{Path: "V1.sql", Content: `
CREATE TABLE a (id BIGINT PRIMARY KEY, b_id BIGINT);
CREATE TABLE b (id BIGINT PRIMARY KEY, a_id BIGINT);
ALTER TABLE a ADD CONSTRAINT fk_ab FOREIGN KEY (b_id) REFERENCES b(id);
ALTER TABLE b ADD CONSTRAINT fk_ba FOREIGN KEY (a_id) REFERENCES a(id);
`}}
	model := BuildSchemaModel(files, DialectMySQL)
	report := RunSchemaReview(model, defaultConfig().Rules, true)
	hasCycle := false
	for _, f := range report.Findings {
		if f.RuleID == "ARCH027" {
			hasCycle = true
		}
	}
	if !hasCycle {
		t.Error("循环外键应命中 ARCH027")
	}
}

func keys(m map[string]bool) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}

// 回归：单条 CREATE TABLE 不得触发 ARCH032（AddTable/markCreate 曾双重计数）。
func TestSchemaNoFalseDuplicateCreate(t *testing.T) {
	files := []batchInput{{Path: "V1__init.sql", Content: `
CREATE TABLE t_order (
  id BIGINT NOT NULL PRIMARY KEY AUTO_INCREMENT,
  order_no VARCHAR(32) NOT NULL,
  amount DECIMAL(12,2) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
`}}
	model := BuildSchemaModel(files, DialectMySQL)
	report := RunSchemaReview(model, defaultConfig().Rules, true)
	for _, f := range report.Findings {
		if f.RuleID == "ARCH032" {
			t.Errorf("单次 CREATE 不应报 ARCH032，实际：%s", f.Message)
		}
	}
}

// 回归：CREATE TABLE 的 NOT NULL 列不报 ARCH029；ALTER ADD 的才报。
func TestSchemaARCH029OnlyForAlter(t *testing.T) {
	files := []batchInput{{Path: "V1__a.sql", Content: `
CREATE TABLE t_user (id BIGINT PRIMARY KEY, name VARCHAR(32) NOT NULL);
ALTER TABLE t_user ADD COLUMN nickname VARCHAR(32) NOT NULL;
`}}
	model := BuildSchemaModel(files, DialectMySQL)
	report := RunSchemaReview(model, defaultConfig().Rules, true)
	var arch029 []SchemaFinding
	for _, f := range report.Findings {
		if f.RuleID == "ARCH029" {
			arch029 = append(arch029, f)
		}
	}
	if len(arch029) != 1 || !strings.Contains(arch029[0].Message, "nickname") {
		t.Errorf("ARCH029 应只命中 ALTER ADD 的 nickname 列，实际：%v", arch029)
	}
}

// 回归：自动方言下，首个文件判定为 MySQL 后不允许被后续 sqlite 文件翻转。
func TestSchemaDialectLockedByFirstFile(t *testing.T) {
	files := []batchInput{
		{Path: "V1__a.sql", Content: "CREATE TABLE a (id BIGINT PRIMARY KEY) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;"},
		{Path: "deploy/sqlite/sql/V1__b.sql", Content: "CREATE TABLE b (id INTEGER PRIMARY KEY);"},
	}
	model := BuildSchemaModel(files, "")
	if model.Dialect != DialectMySQL {
		t.Errorf("模型方言应锁定为首个文件的 mysql，实际 %q", model.Dialect)
	}
}

// 回归：别名限定列引用（o.buyer_id）不得对别名表报 ARCH023。
func TestSchemaPredicateAliasNotReported(t *testing.T) {
	files := []batchInput{{Path: "V2__dml.sql", Content: `
CREATE TABLE orders (id BIGINT PRIMARY KEY, status VARCHAR(8), buyer_id BIGINT);
UPDATE orders o SET status = 'x' WHERE o.buyer_id = 1 AND o.status = 'y';
`}}
	model := BuildSchemaModel(files, DialectMySQL)
	report := RunSchemaReview(model, defaultConfig().Rules, true)
	for _, f := range report.Findings {
		if f.RuleID == "ARCH023" && strings.Contains(f.Message, "表 o ") {
			t.Errorf("别名 o 不应被当成表名报 ARCH023：%s", f.Message)
		}
	}
}

// 回归：ALTER COLUMN SET NOT NULL 不得把列类型污染成 "SET"。
func TestSchemaAlterColumnDoesNotCorruptType(t *testing.T) {
	files := []batchInput{{Path: "V3__alt.sql", Content: `
CREATE TABLE t_user (id BIGINT PRIMARY KEY, code VARCHAR(32) NULL);
ALTER TABLE t_user ALTER COLUMN code SET NOT NULL;
`}}
	model := BuildSchemaModel(files, DialectMySQL)
	tu := model.Tables["t_user"]
	if tu == nil {
		t.Fatal("t_user 缺失")
	}
	c := tu.Column("code")
	if c == nil {
		t.Fatal("code 列缺失")
	}
	if c.RawType != "VARCHAR(32)" {
		t.Errorf("ALTER COLUMN 不应破坏列类型，实际 RawType=%q", c.RawType)
	}
	if c.Nullable {
		t.Error("SET NOT NULL 后应不可空")
	}
}

// 回归：表级 COMMENT='x' 可解析；列级 COMMENT 不再覆盖表注释。
func TestSchemaTableCommentParsing(t *testing.T) {
	files := []batchInput{{Path: "V1.sql", Content: `
CREATE TABLE t_c (id BIGINT PRIMARY KEY COMMENT '主键'
) COMMENT='订单表' ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
`}}
	model := BuildSchemaModel(files, DialectMySQL)
	if got := model.Tables["t_c"].Comment; got != "订单表" {
		t.Errorf("表注释应为「订单表」，实际 %q", got)
	}
}

func TestSchemaToolDispatch(t *testing.T) {
	p := &plugin{reviews: newReviewStore()}
	out, err := p.dispatchTool("sql_schema_review", map[string]any{
		"sql": `CREATE TABLE t (id BIGINT NOT NULL, amount DOUBLE, status VARCHAR(8));`,
	})
	if err != nil {
		t.Fatalf("sql_schema_review: %v", err)
	}
	m := out.(map[string]any)
	arch, ok := m["architecture"].(*ArchitectureReport)
	if !ok {
		t.Fatalf("architecture 类型错误: %T", m["architecture"])
	}
	if arch.Stats.Tables != 1 {
		t.Errorf("应识别 1 张表，实际 %d", arch.Stats.Tables)
	}
}

// ---------------- 0.2.x/0.3.0 补强回归 ----------------

// CHANGE COLUMN：重命名 + 重定义，旧列名不得残留。
func TestSchemaChangeColumnRename(t *testing.T) {
	files := []batchInput{{Path: "V1.sql", Content: `
CREATE TABLE t_user (id BIGINT PRIMARY KEY, old_name VARCHAR(32) NOT NULL);
ALTER TABLE t_user CHANGE COLUMN old_name new_name VARCHAR(64) NOT NULL;
`}}
	model := BuildSchemaModel(files, DialectMySQL)
	tu := model.Tables["t_user"]
	if tu == nil {
		t.Fatal("t_user 缺失")
	}
	if tu.Column("old_name") != nil {
		t.Error("旧列名不应残留在模型中")
	}
	c := tu.Column("new_name")
	if c == nil {
		t.Fatal("新列名缺失")
	}
	if c.RawType != "VARCHAR(64)" {
		t.Errorf("新列类型应为 VARCHAR(64)，实际 %q", c.RawType)
	}
}

// 多列外键：FromColumns 保留完整列清单；引用侧建复合索引后 ARCH020 不误报。
func TestSchemaMultiColumnFK(t *testing.T) {
	files := []batchInput{{Path: "V1.sql", Content: `
CREATE TABLE t_order_item (
  order_id BIGINT NOT NULL,
  item_no INT NOT NULL,
  PRIMARY KEY (order_id, item_no)
);
CREATE TABLE t_shipment (
  order_id BIGINT NOT NULL,
  item_no INT NOT NULL,
  KEY idx_ship (order_id, item_no),
  CONSTRAINT fk_si FOREIGN KEY (order_id, item_no) REFERENCES t_order_item (order_id, item_no)
);
`}}
	model := BuildSchemaModel(files, DialectMySQL)
	report := RunSchemaReview(model, defaultConfig().Rules, true)
	for _, f := range report.Findings {
		if f.RuleID == "ARCH020" {
			t.Errorf("复合索引已覆盖外键列，不应报 ARCH020：%s", f.Message)
		}
	}
	var fk *Reference
	for i := range model.Refs {
		if model.Refs[i].FromTable == "t_shipment" {
			fk = &model.Refs[i]
		}
	}
	if fk == nil || fk.FromColumns != "order_id,item_no" {
		t.Errorf("多列外键 FromColumns 应为 order_id,item_no，实际 %+v", fk)
	}
}

// 列级 REFERENCES（SQLite/标准语法）应进入关系规则；引用列有索引时 ARCH020 不报。
func TestSchemaColumnLevelReferences(t *testing.T) {
	files := []batchInput{{Path: "V1.sql", Content: `
CREATE TABLE t_order (id BIGINT PRIMARY KEY);
CREATE TABLE t_line (
  id BIGINT PRIMARY KEY,
  order_id BIGINT REFERENCES t_order (id),
  KEY idx_order (order_id)
);
`}}
	model := BuildSchemaModel(files, DialectMySQL)
	if len(model.Refs) != 1 || model.Refs[0].ToTable != "t_order" {
		t.Fatalf("列级 REFERENCES 未解析: %+v", model.Refs)
	}
	report := RunSchemaReview(model, defaultConfig().Rules, true)
	found := map[string]bool{}
	for _, f := range report.Findings {
		found[f.RuleID] = true
	}
	if found["ARCH020"] {
		t.Error("引用列已有索引，不应报 ARCH020")
	}
	if found["ARCH025"] {
		t.Error("目标表 t_order 在批次内，不应报 ARCH025")
	}
}

// RENAME：ALTER TABLE a RENAME TO b 与 RENAME TABLE a TO b 都要同步 VDD 键。
func TestSchemaRenameTable(t *testing.T) {
	files := []batchInput{{Path: "V1.sql", Content: `
CREATE TABLE a (id BIGINT PRIMARY KEY);
CREATE TABLE c (a_id BIGINT, CONSTRAINT fk_c FOREIGN KEY (a_id) REFERENCES a (id));
ALTER TABLE a RENAME TO b;
RENAME TABLE c TO d;
`}}
	model := BuildSchemaModel(files, DialectMySQL)
	if _, ok := model.Tables["a"]; ok {
		t.Error("旧键 a 不应残留")
	}
	if _, ok := model.Tables["b"]; !ok {
		t.Error("ALTER RENAME 后应有表 b")
	}
	if _, ok := model.Tables["d"]; !ok {
		t.Error("RENAME TABLE 后应有表 d")
	}
	for i := range model.Refs {
		if model.Refs[i].ToTable == "a" {
			t.Errorf("外键引用未同步改名：%+v", model.Refs[i])
		}
	}
}

// ARCH016：跨表同名索引不再误报；同表同名仍报。
func TestSchemaARCH016PerTable(t *testing.T) {
	files := []batchInput{{Path: "V1.sql", Content: `
CREATE TABLE a (id BIGINT PRIMARY KEY, x INT, KEY idx_x (x));
CREATE TABLE b (id BIGINT PRIMARY KEY, x INT, KEY idx_x (x));
CREATE TABLE c (id BIGINT PRIMARY KEY, y INT, KEY idx_y (y), KEY idx_y (y));
`}}
	model := BuildSchemaModel(files, DialectMySQL)
	report := RunSchemaReview(model, defaultConfig().Rules, true)
	for _, f := range report.Findings {
		if f.RuleID == "ARCH016" {
			if !strings.Contains(f.Message, "表 c") {
				t.Errorf("跨表同名索引不应报 ARCH016：%s", f.Message)
			}
		}
	}
}

// ARCH023：别名限定列要归还到真实表；真实表缺索引仍要报。
func TestSchemaPredicateAliasMapping(t *testing.T) {
	files := []batchInput{{Path: "V2__dml.sql", Content: `
CREATE TABLE orders (id BIGINT PRIMARY KEY, status VARCHAR(8), buyer_id BIGINT);
UPDATE orders o SET status = 'x' WHERE o.buyer_id = 1 AND o.status = 'y';
UPDATE orders SET status = 'z' WHERE orders.buyer_id = 2 AND orders.buyer_id = 3;
`}}
	model := BuildSchemaModel(files, DialectMySQL)
	report := RunSchemaReview(model, defaultConfig().Rules, true)
	var arch023 []SchemaFinding
	for _, f := range report.Findings {
		if f.RuleID == "ARCH023" {
			arch023 = append(arch023, f)
		}
	}
	if len(arch023) != 1 || !strings.Contains(arch023[0].Message, "表 orders") {
		t.Errorf("别名应映射到 orders 且只报一条 ARCH023，实际：%v", arch023)
	}
}
