package store

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// isCJK 判断是否中日韩文字（按「字」切二元组的那一类）。
func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hangul, r)
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

// token 是一段连续文字：中文串或一个西文词。
type token struct {
	text string
	cjk  bool
}

// splitTokens 把文本切成中文串和西文词（小写），标点空白都是分隔符。
func splitTokens(s string) []token {
	var out []token
	var cur []rune
	curCJK := false
	flush := func() {
		if len(cur) > 0 {
			out = append(out, token{text: string(cur), cjk: curCJK})
			cur = cur[:0]
		}
	}
	for _, r := range s {
		switch {
		case isCJK(r):
			if !curCJK {
				flush()
			}
			curCJK = true
			cur = append(cur, r)
		case isWordRune(r):
			if curCJK {
				flush()
			}
			curCJK = false
			cur = append(cur, unicode.ToLower(r))
		default:
			flush()
		}
	}
	flush()
	return out
}

// bigramize：中文串按重叠二元组展开（单字保留），西文词原样小写。
// 结果用空格连接，交给 unicode61 分词器：每个二元组就是一个词条。
func bigramize(s string) string {
	var b strings.Builder
	for _, t := range splitTokens(s) {
		if !t.cjk {
			b.WriteString(t.text)
			b.WriteByte(' ')
			continue
		}
		rs := []rune(t.text)
		if len(rs) == 1 {
			b.WriteString(t.text)
			b.WriteByte(' ')
			continue
		}
		for i := 0; i+1 < len(rs); i++ {
			b.WriteString(string(rs[i : i+2]))
			b.WriteByte(' ')
		}
	}
	return b.String()
}

// Query 是解析后的检索词。
type Query struct {
	Raw      string
	Terms    []string // 原样的词（中文串 / 西文词），用于邻近度、标签、主题
	Bigrams  []string // 中文二元组（去重）
	Words    []string // 西文词
	Singles  []string // 单个汉字（没有二元组可用）
	Trigrams []string // ≥3 字符的词，走 trigram 精确子串
}

func parseQuery(q string) Query {
	pq := Query{Raw: strings.TrimSpace(q)}
	seen := map[string]bool{}
	add := func(kind string, list *[]string, v string) {
		if !seen[kind+"\x00"+v] {
			seen[kind+"\x00"+v] = true
			*list = append(*list, v)
		}
	}
	for _, t := range splitTokens(q) {
		add("term", &pq.Terms, t.text)
		if utf8.RuneCountInString(t.text) >= 3 {
			add("tri", &pq.Trigrams, t.text)
		}
		if !t.cjk {
			add("word", &pq.Words, t.text)
			continue
		}
		rs := []rune(t.text)
		if len(rs) == 1 {
			add("single", &pq.Singles, t.text)
			continue
		}
		for i := 0; i+1 < len(rs); i++ {
			add("bi", &pq.Bigrams, string(rs[i:i+2]))
		}
	}
	return pq
}

// ftsQuote 把一个词包成 FTS5 字符串字面量。
func ftsQuote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// normalize 用于 content_hash：去首尾空白、合并空白。
func normalize(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// splitTags 解析「a,b，c;d」。
func splitTags(s string) []string {
	// 顿号不是分隔符：「算力、芯片与成本」是一个标签
	f := func(r rune) bool { return r == ',' || r == '，' || r == ';' || r == '；' || r == '\n' }
	var out []string
	seen := map[string]bool{}
	for _, t := range strings.FieldsFunc(s, f) {
		t = strings.TrimSpace(t)
		t = strings.TrimPrefix(t, "#")
		if t == "" || seen[strings.ToLower(t)] {
			continue
		}
		seen[strings.ToLower(t)] = true
		out = append(out, t)
	}
	return out
}
