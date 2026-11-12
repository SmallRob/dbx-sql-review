package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	dbxpluginsdk "github.com/t8y2/dbx/plugins/sdk/go/dbx-plugin-sdk"
)

// AuditEntry 是一条评审记录。
//
// 链式哈希：hash = SHA256(prevHash + 规范化(entry))，append-only。
// 用途是「评审记录有没有被事后改过」，不是身份认证——
// 评审人标识是自填的，链只能证明记录未被篡改，不能证明是谁写的。
type AuditEntry struct {
	Seq      int    `json:"seq"`
	TS       string `json:"ts"`
	Kind     string `json:"kind"`
	Subject  string `json:"subject"`
	Level    string `json:"level"`
	Score    int    `json:"score"`
	Findings int    `json:"findings"`
	Actor    string `json:"actor"`
	PrevHash string `json:"prevHash"`
	Hash     string `json:"hash"`
}

var auditMu sync.Mutex

// auditPath 返回审计日志路径。
//
// 注意：没有 DBX_PLUGIN_DATA_DIR 时（独立运行、单元测试）返回空串，
// 表示「不落盘」。绝不退回到当前工作目录——那会在用户的项目目录里
// 留下 audit.jsonl 垃圾文件。
func auditPath() string {
	dir := dbxpluginsdk.DataDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "audit.jsonl")
}

func hashEntry(e AuditEntry) string {
	raw := fmt.Sprintf("%d|%s|%s|%s|%s|%d|%d|%s|%s",
		e.Seq, e.TS, e.Kind, e.Subject, e.Level, e.Score, e.Findings, e.Actor, e.PrevHash)
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func readAudit() []AuditEntry {
	auditMu.Lock()
	defer auditMu.Unlock()
	return readAuditLocked()
}

func readAuditLocked() []AuditEntry {
	path := auditPath()
	if path == "" {
		return nil // 没有数据目录：没有历史记录，也没有落盘
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []AuditEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var e AuditEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err == nil {
			out = append(out, e)
		}
	}
	return out
}

func appendAudit(kind, subject, level string, score, findings int, actor string) AuditEntry {
	auditMu.Lock()
	defer auditMu.Unlock()
	entries := readAuditLocked()
	prev := ""
	if len(entries) > 0 {
		prev = entries[len(entries)-1].Hash
	}
	e := AuditEntry{
		Seq: len(entries) + 1, TS: nowRFC3339(), Kind: kind,
		Subject: subject, Level: level, Score: score,
		Findings: findings, Actor: actor, PrevHash: prev,
	}
	e.Hash = hashEntry(e)
	path := auditPath()
	if path == "" {
		return e // 没有数据目录：只返回记录，不落盘
	}
	if dir := dbxpluginsdk.DataDir(); dir != "" {
		_ = os.MkdirAll(dir, 0o700)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err == nil {
		line, _ := json.Marshal(e)
		_, _ = f.Write(append(line, '\n'))
		f.Close()
	}
	return e
}

func verifyAudit() (bool, int, int) {
	entries := readAudit()
	prev := ""
	for i, e := range entries {
		if e.PrevHash != prev {
			return false, i + 1, len(entries)
		}
		if hashEntry(AuditEntry{e.Seq, e.TS, e.Kind, e.Subject, e.Level, e.Score, e.Findings, e.Actor, e.PrevHash, ""}) != e.Hash {
			return false, i + 1, len(entries)
		}
		prev = e.Hash
	}
	return true, 0, len(entries)
}

func nowRFC3339() string {
	return time.Now().Format(time.RFC3339)
}
