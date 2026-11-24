package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	dbxpluginsdk "github.com/t8y2/dbx/plugins/sdk/go/dbx-plugin-sdk"
)

// Thresholds 是可调阈值。插件不连数据库，行数类阈值只用于「按目录规模推断」。
type Thresholds struct {
	// LargeTableRows 是「大表」的行数门槛，仅用于静态推断，不来自真实库。
	LargeTableRows int64 `json:"largeTableRows"`
	// MaxStatements 是单文件语句数上限。
	MaxStatements int `json:"maxStatements"`
	// MaxFilesPerBatch 是单批次文件数上限，防止一次上传把内存打满。
	MaxFilesPerBatch int `json:"maxFilesPerBatch"`
	// DeepPageOffset 深分页阈值：OFFSET/LIMIT 偏移达到该行数即判慢查询。
	DeepPageOffset int `json:"deepPageOffset"`
	// InListSize IN 列表项数阈值；超过则提示拆分。
	InListSize int `json:"inListSize"`
	// JoinTables 单条语句连接表数阈值；超过则提示连接扇出。
	JoinTables int `json:"joinTables"`
}

// RuleConfig 是规则开关与级别覆盖。
type RuleConfig struct {
	// Disabled 是被关掉的规则 ID。
	Disabled []string `json:"disabled"`
	// Severity 覆盖某些规则的级别（critical/high/medium/low/info）。
	Severity map[string]string `json:"severity"`
	// Thresholds 是阈值配置。
	Thresholds Thresholds `json:"thresholds"`
}

// Config 是插件的持久化配置。
//
// 公共插件刻意不保存任何连接信息：没有 host、没有账号、没有口令。
// 配置里只有规则开关与阈值。
type Config struct {
	// Actor 是评审人标识，仅用于审计记录，可留空。
	Actor string `json:"actor"`
	// Rules 是规则配置。
	Rules RuleConfig `json:"rules"`
	// Schema 是架构评审（v0.2.0）的独立开关与阈值；与六维质量分隔离。
	Schema SchemaConfig `json:"schema"`
	// DefaultDialect 是默认方言（空=按内容自动判定）。
	DefaultDialect string `json:"defaultDialect,omitempty"`
	// DefaultMigration 表示默认按迁移脚本口径评审（开启幂等/命名类规则）。
	DefaultMigration bool `json:"defaultMigration"`
}

// SchemaConfig 是架构评审子配置。
type SchemaConfig struct {
	// Enabled 关闭后回退到 v0.1.33 行为：不构建 VDD、不产出架构报告。
	Enabled bool `json:"enabled"`
	// WideTableCols 是宽表列数阈值（ARCH007）。
	WideTableCols int `json:"wideTableColumns"`
}

func defaultConfig() *Config {
	return &Config{
		Actor: "",
		Rules: RuleConfig{
			// 房规类规则（Optional）默认关闭：通用项目不该被
			// 「必须写 USE」「列必须有 COMMENT」这类团队约定打扰。
			Disabled: defaultDisabled(),
			Severity: map[string]string{},
			Thresholds: Thresholds{
				LargeTableRows:   1000000,
				MaxStatements:    40,
				MaxFilesPerBatch: 800,
				DeepPageOffset:   1000,
				InListSize:       50,
				JoinTables:       5,
			},
		},
		Schema: SchemaConfig{Enabled: true, WideTableCols: 50},
	}
}

var cfgMu sync.Mutex

// configPath 返回配置文件路径。
//
// 没有 DBX_PLUGIN_DATA_DIR 时返回空串，表示「不落盘」——
// 配置是可选的，保存失败不该让整个插件不可用。
func configPath() string {
	dir := dbxpluginsdk.DataDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "config.json")
}

func loadConfig() *Config {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	return loadConfigLocked()
}

func loadConfigLocked() *Config {
	path := configPath()
	if path == "" {
		return defaultConfig() // 没有数据目录：用默认配置，不落盘
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return defaultConfig()
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return defaultConfig()
	}
	def := defaultConfig()
	if c.Rules.Severity == nil {
		c.Rules.Severity = map[string]string{}
	}
	if c.Rules.Disabled == nil {
		// 旧配置没有 disabled 字段时，用默认值补齐房规规则（默认关闭）。
		// 注意不能置空数组——那会把房规规则全部打开，等于施加了团队约定。
		c.Rules.Disabled = def.Rules.Disabled
	}
	if c.Rules.Thresholds.LargeTableRows <= 0 {
		c.Rules.Thresholds.LargeTableRows = def.Rules.Thresholds.LargeTableRows
	}
	if c.Rules.Thresholds.MaxStatements <= 0 {
		c.Rules.Thresholds.MaxStatements = def.Rules.Thresholds.MaxStatements
	}
	if c.Rules.Thresholds.MaxFilesPerBatch <= 0 {
		c.Rules.Thresholds.MaxFilesPerBatch = def.Rules.Thresholds.MaxFilesPerBatch
	}
	if c.Rules.Thresholds.DeepPageOffset <= 0 {
		c.Rules.Thresholds.DeepPageOffset = def.Rules.Thresholds.DeepPageOffset
	}
	if c.Rules.Thresholds.InListSize <= 0 {
		c.Rules.Thresholds.InListSize = def.Rules.Thresholds.InListSize
	}
	if c.Rules.Thresholds.JoinTables <= 0 {
		c.Rules.Thresholds.JoinTables = def.Rules.Thresholds.JoinTables
	}
	if c.Schema.WideTableCols <= 0 {
		c.Schema.WideTableCols = def.Schema.WideTableCols
	}
	// Enabled 零值（旧配置缺字段）按开启处理，保持 v0.2.0 默认行为。
	return &c
}

func saveConfig(c *Config) error {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	return writeConfigLocked(c)
}

func writeConfigLocked(c *Config) error {
	dir := dbxpluginsdk.DataDir()
	if dir == "" {
		// 没有数据目录就不落盘：配置是可选的，
		// 不能因此在用户当前工作目录里凭空造出 config.json。
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	path := configPath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// publicConfig 回给 UI 的配置。
func (c *Config) publicConfig() map[string]any {
	return map[string]any{
		"actor":            c.Actor,
		"rules":            c.Rules,
		"schema":           c.Schema,
		"defaultDialect":   c.DefaultDialect,
		"defaultMigration": c.DefaultMigration,
		"dataDir":          dbxpluginsdk.DataDir(),
	}
}
