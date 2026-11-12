package main

import (
	"hash/crc32"
	"path"
	"regexp"
	"strings"
)

// versionedNameRe 匹配版本化迁移文件名：V<版本>__<描述>.sql。
//
// 兼容 R__（可重复脚本）与 VB（基线）等变体：只要以 V 开头且含 __ 即视为版本化。
var versionedNameRe = regexp.MustCompile(`(?i)^V(\d[\d._]*)__(.+)\.sql$`)

// parseVersionedName 从文件名解析版本号与描述。
func parseVersionedName(p string) (string, string, bool) {
	base := path.Base(strings.ReplaceAll(p, "\\", "/"))
	m := versionedNameRe.FindStringSubmatch(base)
	if m == nil {
		return "", "", false
	}
	v := strings.ReplaceAll(m[1], "_", ".")
	desc := strings.ReplaceAll(m[2], "_", " ")
	return v, desc, true
}

// flywayChecksum 复刻 Flyway 的 CRC32：逐行 UTF-8 字节，行间不含换行，去 BOM。
//
// 这是纯本地计算，不连数据库。用于识别「不同版本的文件内容完全相同」。
func flywayChecksum(content []byte) int32 {
	s := string(content)
	s = strings.TrimPrefix(s, "\uFEFF")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	h := crc32.NewIEEE()
	for _, ln := range lines {
		h.Write([]byte(ln))
	}
	return int32(h.Sum32())
}
