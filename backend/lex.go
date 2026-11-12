package main

import (
	"strings"
)

// Token 是词法单元。Kind: word(裸标识符/关键字) ident(反引号) str num var op
type Token struct {
	Kind  string `json:"k"`
	Text  string `json:"t"`
	Upper string `json:"u,omitempty"`
	Line  int    `json:"l"`
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isIdentChar(c byte) bool {
	return c == '_' || c == '$' || isDigit(c) ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

// tokenize 跳过注释，保留字符串/反引号原样（去掉引号）。
func tokenize(src string) []Token {
	out := make([]Token, 0, len(src)/4)
	i, line := 0, 1
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r':
			i++
		case c == '\n':
			line++
			i++
		case c == '-' && i+1 < len(src) && src[i+1] == '-' &&
			(i+2 >= len(src) || src[i+2] == ' ' || src[i+2] == '\t' || src[i+2] == '\n'):
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			i += 2
			for i < len(src) && !(src[i] == '*' && i+1 < len(src) && src[i+1] == '/') {
				if src[i] == '\n' {
					line++
				}
				i++
			}
			if i < len(src) {
				i += 2
			}
		case c == '\'' || c == '"':
			q := c
			start := i
			startLine := line
			i++
			for i < len(src) {
				if src[i] == '\\' {
					i += 2
					continue
				}
				if src[i] == q {
					if i+1 < len(src) && src[i+1] == q {
						i += 2
						continue
					}
					i++
					break
				}
				if src[i] == '\n' {
					line++
				}
				i++
			}
			inner := src[minInt(len(src), start+1):maxInt(start+1, minInt(len(src), i-1))]
			out = append(out, Token{Kind: "str", Text: decodeLiteral(inner, q), Line: startLine})
		case c == '`':
			start := i
			i++
			for i < len(src) {
				if src[i] == '`' {
					if i+1 < len(src) && src[i+1] == '`' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			txt := strings.TrimSuffix(strings.TrimPrefix(src[start:i], "`"), "`")
			txt = strings.ReplaceAll(txt, "``", "`")
			out = append(out, Token{Kind: "ident", Text: txt, Upper: strings.ToUpper(txt), Line: line})
		case c == '@':
			start := i
			i++
			if i < len(src) && src[i] == '@' {
				i++
			}
			for i < len(src) && (isIdentChar(src[i]) || src[i] == '.') {
				i++
			}
			out = append(out, Token{Kind: "var", Text: src[start:i], Line: line})
		case isDigit(c):
			start := i
			for i < len(src) && (isDigit(src[i]) || src[i] == '.' || src[i] == 'e' || src[i] == 'E') {
				i++
			}
			out = append(out, Token{Kind: "num", Text: src[start:i], Line: line})
		case isIdentChar(c):
			start := i
			for i < len(src) && isIdentChar(src[i]) {
				i++
			}
			w := src[start:i]
			out = append(out, Token{Kind: "word", Text: w, Upper: strings.ToUpper(w), Line: line})
		default:
			if i+1 < len(src) {
				two := src[i : i+2]
				switch two {
				case ":=", "<=", ">=", "<>", "!=", "||", "&&":
					out = append(out, Token{Kind: "op", Text: two, Line: line})
					i += 2
					continue
				}
			}
			out = append(out, Token{Kind: "op", Text: string(c), Line: line})
			i++
		}
	}
	return out
}

func decodeLiteral(s string, q byte) string {
	s = strings.ReplaceAll(s, string(q)+string(q), string(q))
	s = strings.ReplaceAll(s, "\\'", "'")
	s = strings.ReplaceAll(s, "\\\"", "\"")
	s = strings.ReplaceAll(s, "\\n", "\n")
	s = strings.ReplaceAll(s, "\\r", "\r")
	s = strings.ReplaceAll(s, "\\t", "\t")
	s = strings.ReplaceAll(s, "\\0", "")
	s = strings.ReplaceAll(s, "\\\\", "\\")
	return s
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Statement 是一条被分隔符切开的语句。
type Statement struct {
	Text  string `json:"text"`
	Line  int    `json:"line"`
	Index int    `json:"index"`
}

// splitStatements 支持 DELIMITER 指令、注释与字符串内的分号。
func splitStatements(src string) []Statement {
	var out []Statement
	delim := ";"
	line := 1
	start := 0
	startLine := 1
	i := 0
	bol := true

	flush := func(end int) {
		if end <= start {
			start = end
			return
		}
		text := src[start:end]
		lead := len(text) - len(strings.TrimLeft(text, " \t\r\n"))
		stmtLine := startLine + strings.Count(text[:lead], "\n")
		trimmed := strings.TrimSpace(text)
		cleaned, dropped := trimLeadingComments(trimmed)
		if cleaned != "" && !commentOnly(cleaned) {
			out = append(out, Statement{Text: cleaned, Line: stmtLine + dropped, Index: len(out) + 1})
		}
	}

	for i < len(src) {
		c := src[i]

		if c == ' ' || c == '\t' || c == '\r' {
			i++
			continue
		}
		if c == '\n' {
			i++
			line++
			bol = true
			continue
		}
		if bol {
			bol = false
			j := i
			if len(src)-j >= 9 && strings.EqualFold(src[j:j+9], "DELIMITER") {
				flush(i)
				k := j + 9
				for k < len(src) && (src[k] == ' ' || src[k] == '\t') {
					k++
				}
				e := k
				for e < len(src) && src[e] != '\n' && src[e] != '\r' {
					e++
				}
				if d := strings.TrimSpace(src[k:e]); d != "" {
					delim = d
				}
				i = e
				if i < len(src) && src[i] == '\r' {
					i++
				}
				if i < len(src) && src[i] == '\n' {
					i++
					line++
				}
				start = i
				startLine = line
				bol = true
				continue
			}
		}

		switch {
		case c == '-' && i+1 < len(src) && src[i+1] == '-' &&
			(i+2 >= len(src) || src[i+2] == ' ' || src[i+2] == '\t' || src[i+2] == '\n' || src[i+2] == '\r'):
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			i += 2
			for i < len(src) && !(src[i] == '*' && i+1 < len(src) && src[i+1] == '/') {
				if src[i] == '\n' {
					line++
					bol = true
				}
				i++
			}
			if i < len(src) {
				i += 2
			}
		case c == '\'' || c == '"':
			q := c
			i++
			for i < len(src) {
				if src[i] == '\\' {
					i += 2
					continue
				}
				if src[i] == q {
					if i+1 < len(src) && src[i+1] == q {
						i += 2
						continue
					}
					i++
					break
				}
				if src[i] == '\n' {
					line++
					bol = true
				}
				i++
			}
		case c == '`':
			i++
			for i < len(src) {
				if src[i] == '`' {
					if i+1 < len(src) && src[i+1] == '`' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
		case strings.HasPrefix(src[i:], delim):
			flush(i)
			i += len(delim)
			start = i
			startLine = line
		default:
			i++
		}
	}
	flush(len(src))
	return out
}

// trimLeadingComments 去掉语句前的注释行，返回清理后的文本与被去掉的行数。
func trimLeadingComments(s string) (string, int) {
	dropped := 0
	for {
		s = strings.TrimLeft(s, " \t\r\n")
		if strings.HasPrefix(s, "--") || strings.HasPrefix(s, "#") {
			if idx := strings.IndexByte(s, '\n'); idx >= 0 {
				s = s[idx+1:]
				dropped++
				continue
			}
			return "", dropped
		}
		if strings.HasPrefix(s, "/*") && !strings.HasPrefix(s, "/*!") {
			if idx := strings.Index(s, "*/"); idx >= 0 {
				dropped += strings.Count(s[:idx], "\n")
				s = s[idx+2:]
				continue
			}
			return "", dropped
		}
		return s, dropped
	}
}

func commentOnly(s string) bool {
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			i++
		case c == '-' && i+1 < len(s) && s[i+1] == '-':
			for i < len(s) && s[i] != '\n' {
				i++
			}
		case c == '#':
			for i < len(s) && s[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			i += 2
			for i < len(s) && !(s[i] == '*' && i+1 < len(s) && s[i+1] == '/') {
				i++
			}
			if i < len(s) {
				i += 2
			}
		default:
			return false
		}
	}
	return true
}

// stripLiterals 把字符串字面量换成 ?，用于报告片段（避免泄露数据）。
func stripLiterals(sql string) string {
	var b strings.Builder
	i := 0
	for i < len(sql) {
		c := sql[i]
		if c == '\'' || c == '"' {
			q := c
			i++
			for i < len(sql) {
				if sql[i] == '\\' {
					i += 2
					continue
				}
				if sql[i] == q {
					if i+1 < len(sql) && sql[i+1] == q {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			b.WriteString("'?'")
			continue
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

func snippet(sql string, limit int) string {
	s := stripLiterals(sql)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > limit {
		s = s[:limit] + "…"
	}
	return s
}
