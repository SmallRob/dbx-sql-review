package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// reviewRecord 供 AI Agent 继续读取和优化最近一次单条评审。
// 原始 SQL 只保存在 sidecar 进程内存中，不落盘、不写入审计日志。
type reviewRecord struct {
	ID         string
	SQL        string
	File       string
	Note       string
	ReviewedAt time.Time
	Result     *Result
	Options    AnalyzeOptions
}

type reviewStore struct {
	mu      sync.RWMutex
	records map[string]*reviewRecord
	order   []string
}

const maxReviewRecords = 32

func newReviewStore() *reviewStore {
	return &reviewStore{records: map[string]*reviewRecord{}}
}

func newReviewID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return fmt.Sprintf("r%s%s", time.Now().Format("0102-1504"), hex.EncodeToString(b))
}

func (s *reviewStore) put(sqlText, file, note string, result *Result, opts AnalyzeOptions) *reviewRecord {
	r := &reviewRecord{ID: newReviewID(), SQL: sqlText, File: file, Note: note,
		ReviewedAt: time.Now(), Result: result, Options: opts}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.records == nil {
		s.records = map[string]*reviewRecord{}
	}
	s.records[r.ID] = r
	s.order = append(s.order, r.ID)
	if len(s.order) > maxReviewRecords {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.records, oldest)
	}
	return r
}

// get 在 reviewId 为空时返回最新评审，便于 Agent 接着 UI 的结果工作。
func (s *reviewStore) get(id string) (*reviewRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if id == "" && len(s.order) > 0 {
		id = s.order[len(s.order)-1]
	}
	r, ok := s.records[id]
	return r, ok
}
