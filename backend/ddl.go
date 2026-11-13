package main

import "strings"

type colDef struct {
	Name       string
	Type       string
	IsPK       bool
	Indexed    bool
	HasComment bool
	NotNull    bool
	HasDefault bool
}

type createTableInfo struct {
	Columns    []colDef
	HasPK      bool
	HasComment bool
	Engine     string
	Charset    string
}

func parseCreateTable(toks []Token) createTableInfo {
	info := createTableInfo{}
	start := -1
	for i, t := range toks {
		if t.Kind == "op" && t.Text == "(" {
			start = i
			break
		}
	}
	if start < 0 {
		return info
	}

	var segs [][]Token
	var seg []Token
	depth := 0
	end := len(toks)
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
					end = i
					i = len(toks)
					continue
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

	indexed := map[string]bool{}
	pk := map[string]bool{}
	for _, s := range segs {
		if len(s) == 0 {
			continue
		}
		head := s[0]
		hu := strings.ToUpper(head.Text)
		switch hu {
		case "PRIMARY":
			info.HasPK = true
			for _, n := range identList(s) {
				pk[n] = true
			}
			continue
		case "CONSTRAINT":
			// SQL Server 常写 CONSTRAINT pk_name PRIMARY KEY (id)。
			for _, t := range s {
				if t.Kind == "word" && t.Upper == "PRIMARY" {
					info.HasPK = true
					for _, n := range identList(s) {
						pk[n] = true
					}
					break
				}
			}
			continue
		case "UNIQUE", "KEY", "INDEX", "FULLTEXT", "SPATIAL", "FOREIGN", "CHECK":
			if hu == "FOREIGN" || hu == "CHECK" {
				continue
			}
			for _, n := range identList(s) {
				indexed[n] = true
			}
			continue
		}
		c := colDef{Name: head.Text}
		if len(s) > 1 {
			c.Type = typeWithArgs(s, 1)
		}
		for _, t := range s[2:] {
			switch strings.ToUpper(t.Text) {
			case "NOT":
				c.NotNull = true
			case "DEFAULT":
				c.HasDefault = true
			case "COMMENT":
				c.HasComment = true
			case "PRIMARY":
				c.IsPK = true
				info.HasPK = true
			case "UNIQUE":
				c.Indexed = true
			case "KEY":
				if t.Kind == "word" {
					c.Indexed = true
				}
			}
		}
		info.Columns = append(info.Columns, c)
	}
	for i := range info.Columns {
		n := strings.ToLower(info.Columns[i].Name)
		// 注意用 || 而不是直接赋值：pk 表只收录表级 `PRIMARY KEY (col)`，
		// 而列级 `id BIGINT PRIMARY KEY` 是在上面的循环里识别的。
		// 直接覆盖会把列级主键误判成非主键（曾导致 SQL036 永不触发）。
		info.Columns[i].IsPK = pk[n] || info.Columns[i].IsPK
		info.Columns[i].Indexed = indexed[n] || info.Columns[i].Indexed
	}

	for i := end; i < len(toks); i++ {
		t := toks[i]
		if t.Kind != "word" && t.Kind != "ident" {
			continue
		}
		up := strings.ToUpper(t.Text)
		switch up {
		case "ENGINE":
			if v := valueAfter(toks, i); v != "" {
				info.Engine = v
			}
		case "CHARSET":
			if v := valueAfter(toks, i); v != "" {
				info.Charset = v
			}
		case "CHARACTER":
			// CHARACTER SET [=] utf8mb4
			if i+1 < len(toks) && toks[i+1].Upper == "SET" {
				if v := valueAfter(toks, i+1); v != "" {
					info.Charset = v
				}
			}
		case "COMMENT":
			info.HasComment = true
		}
	}
	return info
}

// typeWithArgs 取列类型及其括号参数，例如 VARCHAR(255)、DECIMAL(10,2)。
//
// 为什么要保留真实参数：SQL036「VARCHAR(255) 作主键」这类规则必须能看到长度，
// 早期实现把参数统一截断成 "(…)"，导致该规则永远不触发。
func typeWithArgs(toks []Token, typeIdx int) string {
	if typeIdx < 0 || typeIdx >= len(toks) {
		return ""
	}
	base := toks[typeIdx].Text
	if toks[typeIdx].Kind != "word" {
		return base
	}
	j := typeIdx + 1
	result := base
	if j < len(toks) && toks[j].Kind == "op" && toks[j].Text == "(" {
		// 收集括号内的原始 token，直到配平的右括号。
		var args []string
		depth := 0
	collect:
		for ; j < len(toks); j++ {
			t := toks[j]
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
						result = base + "(" + strings.Join(args, "") + ")"
						j++
						break collect
					}
				case ",":
					if depth == 1 {
						args = append(args, ",")
						continue
					}
				}
			}
			if depth >= 1 {
				args = append(args, t.Text)
			}
		}
	}
	// PostgreSQL 的多词类型修饰，例如 TIMESTAMP(3) WITHOUT TIME ZONE、
	// DOUBLE PRECISION、CHARACTER VARYING。只吸收固定的修饰词，避免吞掉约束。
	allowed := map[string]bool{"WITHOUT": true, "WITH": true, "TIME": true, "ZONE": true,
		"PRECISION": true, "VARYING": true}
	for j < len(toks) {
		t := toks[j]
		if t.Kind != "word" || !allowed[t.Upper] {
			break
		}
		result += " " + strings.ToLower(t.Text)
		j++
	}
	return result
}

func valueAfter(toks []Token, i int) string {
	for j := i + 1; j < len(toks) && j < i+4; j++ {
		t := toks[j]
		if t.Kind == "op" && t.Text == "=" {
			continue
		}
		if t.Kind == "word" || t.Kind == "ident" || t.Kind == "num" {
			return t.Text
		}
	}
	return ""
}

// identList 取括号内的标识符（索引列）。
func identList(seg []Token) []string {
	var out []string
	depth := 0
	for i, t := range seg {
		if t.Kind == "op" {
			switch t.Text {
			case "(":
				depth++
			case ")":
				depth--
			}
			continue
		}
		if depth == 1 && (t.Kind == "word" || t.Kind == "ident") && i > 0 {
			out = append(out, strings.ToLower(t.Text))
		}
	}
	return out
}

func parseAlterColumns(toks []Token) []colDef {
	var out []colDef
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t.Kind != "word" {
			continue
		}
		up := t.Upper
		if up != "ADD" && up != "MODIFY" && up != "CHANGE" {
			continue
		}
		j := i + 1
		if j < len(toks) && toks[j].Kind == "word" && toks[j].Upper == "COLUMN" {
			j++
		}
		if j < len(toks) && toks[j].Kind == "word" && toks[j].Upper == "IF" {
			j += 2 // IF NOT EXISTS
		}
		if j < len(toks) && toks[j].Kind == "word" {
			// ADD CONSTRAINT / ADD INDEX / ADD PRIMARY KEY 等不是列定义
			switch toks[j].Upper {
			case "CONSTRAINT", "PRIMARY", "UNIQUE", "INDEX", "KEY", "FULLTEXT", "SPATIAL", "FOREIGN", "CHECK":
				continue
			}
		}
		if up == "CHANGE" {
			j++ // 旧列名
		}
		if j+1 >= len(toks) {
			continue
		}
		nameTok, typeTok := toks[j], toks[j+1]
		if nameTok.Kind != "word" && nameTok.Kind != "ident" {
			continue
		}
		c := colDef{Name: nameTok.Text, Type: typeTok.Text}
		if typeTok.Kind == "word" && j+2 < len(toks) && toks[j+2].Kind == "op" && toks[j+2].Text == "(" {
			c.Type = typeWithArgs(toks, j+1)
		}
		for k := j + 2; k < len(toks) && k < j+30; k++ {
			if toks[k].Kind == "word" && toks[k].Upper == "COMMENT" {
				c.HasComment = true
			}
			if toks[k].Kind == "op" && toks[k].Text == "," {
				break
			}
		}
		out = append(out, c)
	}
	return out
}

func indexTargetTable(toks []Token) string {
	for i, t := range toks {
		if t.Kind == "word" && t.Upper == "ON" && i+1 < len(toks) {
			return strings.ToLower(toks[i+1].Text)
		}
	}
	return ""
}
