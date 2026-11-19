package store

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// SearchOpts 检索参数。
type SearchOpts struct {
	Q       string `json:"q"`
	Limit   int    `json:"limit"`
	Kind    string `json:"kind"`
	Tag     string `json:"tag"`
	Topic   string `json:"topic"`   // 主题前缀
	History bool   `json:"history"` // 包含已被取代/撤回的
	Explain bool   `json:"explain"`
	Caller  string `json:"caller"`
	NoCache bool   `json:"no_cache"`
	Since   int64  `json:"since"`   // 只要这个时间（unix 秒）之后写入的；0 = 不限
	Until   int64  `json:"until"`   // 只要这个时间之前写入的
	NoFold  bool   `json:"no_fold"` // 关掉近似重复折叠
}

type Explain struct {
	Lex      float64        `json:"lex"`
	Prox     float64        `json:"proximity"`
	Rec      float64        `json:"recency"`
	Imp      float64        `json:"importance"`
	TagBoost float64        `json:"tag_boost"`
	Routes   map[string]int `json:"routes"` // 路名 → 在该路的名次（1 起）
}

type Hit struct {
	*Entry
	Score   float64  `json:"score"`
	Explain *Explain `json:"explain,omitempty"`
}

type SearchResult struct {
	Hits       []Hit    `json:"hits"`
	Candidates int      `json:"candidates"`
	Cached     bool     `json:"cached"`
	Ms         float64  `json:"ms"`
	TagTop1    bool     `json:"tag_top1"`
	TagCands   int      `json:"tag_cands"`
	HubTags    []string `json:"hub_tags,omitempty"` // 因为太泛被放弃的标签
	Routes     []string `json:"routes"`
	Expanded   []string `json:"expanded,omitempty"` // 按同义词组扩展出来的写法
	Folded     int      `json:"folded,omitempty"`   // 近似重复、被挪到后面的条数
}

type candMeta struct {
	id            int64
	kind          string
	topic         string
	created       int64
	volatility    string
	useCount      int
	notSuperseded bool
	validTo       int64
}

func (m *candMeta) active(t int64) bool { return m.notSuperseded && (m.validTo == 0 || m.validTo > t) }

const routeLimit = 300

func (s *Store) ids(q string, args ...any) []int64 {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out
}

func appendUnique(dst []int64, seen map[int64]bool, src []int64) []int64 {
	for _, id := range src {
		if !seen[id] {
			seen[id] = true
			dst = append(dst, id)
		}
	}
	return dst
}

func (s *Store) tagCap(limit int) int {
	c := s.hubCapN * limit
	if c < 60 {
		c = 60
	}
	return c
}

// Search 四路并行（二元组 / 三元组 / 主题范围 / 标签点查）→ RRF 融合 → 相关×邻近×新鲜×重要。读路径不写库。
func (s *Store) Search(o SearchOpts) (*SearchResult, error) {
	t0 := time.Now()
	if o.Limit <= 0 {
		o.Limit = 8
	}
	if o.Limit > 200 {
		o.Limit = 200
	}
	if o.Kind != "" {
		k, err := NormKind(o.Kind)
		if err != nil {
			return nil, err
		}
		o.Kind = k
	}
	key := ""
	if !o.NoCache {
		kb, _ := json.Marshal(struct {
			SearchOpts
			G uint64
		}{SearchOpts{Q: o.Q, Limit: o.Limit, Kind: o.Kind, Tag: o.Tag, Topic: o.Topic, History: o.History, Explain: o.Explain,
			Since: o.Since, Until: o.Until, NoFold: o.NoFold}, s.Generation()})
		key = string(kb)
		if v, ok := s.cache.get(key); ok {
			r := *(v.(*SearchResult))
			r.Cached = true
			r.Ms = float64(time.Since(t0).Microseconds()) / 1000
			s.countHits(r.Hits)
			return &r, nil
		}
	}
	res, err := s.search(o)
	if err != nil {
		return nil, err
	}
	res.Ms = float64(time.Since(t0).Microseconds()) / 1000
	if key != "" {
		s.cache.put(key, res)
	}
	s.countHits(res.Hits)
	cp := *res
	return &cp, nil
}

var traceSearch bool

func (s *Store) search(o SearchOpts) (*SearchResult, error) {
	tt := time.Now()
	mark := func(label string) {
		if traceSearch {
			fmt.Printf("  %-12s %v\n", label, time.Since(tt))
			tt = time.Now()
		}
	}
	defer mark("end")
	pq := parseQuery(o.Q)
	nowT := time.Now()
	res := &SearchResult{Hits: []Hit{}}
	routes := map[string][]int64{}

	if pq.Raw == "" {
		// 没有检索词：按筛选条件列最新的
		until := o.Until
		if until <= 0 {
			until = 1 << 62
		}
		routes["recent"] = s.ids(`SELECT id FROM entries WHERE created_epoch >= ? AND created_epoch < ? ORDER BY created_epoch DESC LIMIT ?`,
			o.Since, until, routeLimit*3)
	} else {
		// 每个词的所有写法（同义词组）：词内 OR，词与词之间 AND
		variants := make([][]string, len(pq.Terms))
		for i, t := range pq.Terms {
			variants[i] = s.termVariants(t)
			for _, v := range variants[i][1:] {
				res.Expanded = append(res.Expanded, v)
			}
		}
		// 1) 二元组：中文按重叠二元组 AND，西文词前缀
		{
			var groups []string
			for _, vs := range variants {
				var ex []string
				seen := map[string]bool{}
				for k, v := range vs {
					// 用户自己敲的词：西文按前缀匹配；同义扩展出来的写法：整词匹配（免得 Arm 带出 army）
					if e := termExpr(v, k == 0); e != "" && !seen[e] {
						seen[e] = true
						ex = append(ex, e)
					}
				}
				if len(ex) == 1 {
					groups = append(groups, ex[0])
				} else if len(ex) > 1 {
					groups = append(groups, "("+strings.Join(ex, " OR ")+")")
				}
			}
			if len(groups) > 0 {
				routes["bigram"] = s.ftsRoute("fts_bi", strings.Join(groups, " AND "))
			}
		}
		// 2) 三元组：≥3 字的词做子串精确匹配（同义写法都 ≥3 字才扩展，否则这个词交给二元组）
		{
			var tp []string
			for i, t := range pq.Terms {
				if utf8.RuneCountInString(t) < 3 {
					continue
				}
				ok, ex := true, []string{}
				for _, v := range variants[i] {
					lv := strings.ToLower(strings.TrimSpace(v))
					if utf8.RuneCountInString(lv) < 3 {
						ok = false
						break
					}
					ex = append(ex, ftsQuote(lv))
				}
				if !ok {
					ex = []string{ftsQuote(t)}
				}
				if len(ex) == 1 {
					tp = append(tp, ex[0])
				} else {
					tp = append(tp, "("+strings.Join(ex, " OR ")+")")
				}
			}
			if len(tp) > 0 {
				routes["trigram"] = s.ftsRoute("fts_tri", strings.Join(tp, " AND "))
			}
		}
		allKeys := []string{pq.Raw}
		for _, vs := range variants {
			allKeys = append(allKeys, vs...)
		}
		// 3) 主题范围查（BINARY 范围，中文前缀也走索引）
		{
			seen := map[int64]bool{}
			var list []int64
			done := map[string]bool{}
			for _, k := range allKeys {
				if k == "" || done[k] {
					continue
				}
				done[k] = true
				list = appendUnique(list, seen, s.ids(`SELECT id FROM entries WHERE topic >= ? AND topic < ? ORDER BY created_epoch DESC LIMIT 100`,
					k, k+"\U0010ffff"))
			}
			if len(list) > 0 {
				routes["topic"] = list
			}
		}
		// 4) 标签点查：查询词 = 标签名 → 直接顶到第一梯队；太泛的枢纽标签放弃（交给全文）
		{
			cap := s.tagCap(o.Limit)
			seen := map[int64]bool{}
			tried := map[string]bool{}
			var list []int64
			for _, k := range allKeys {
				k = strings.TrimPrefix(strings.TrimSpace(k), "#")
				lk := strings.ToLower(k)
				if k == "" || tried[lk] {
					continue
				}
				tried[lk] = true
				var df int
				s.db.QueryRow(`SELECT count(*) FROM (SELECT 1 FROM entry_tags WHERE tag=? LIMIT ?)`, k, cap+1).Scan(&df)
				if df == 0 {
					continue
				}
				if df > cap {
					res.HubTags = append(res.HubTags, k)
					continue
				}
				list = appendUnique(list, seen, s.ids(`SELECT t.eid FROM entry_tags t JOIN entries e ON e.id=t.eid WHERE t.tag=?
					ORDER BY e.created_epoch DESC`, k))
			}
			if len(list) > 0 {
				routes["tag"] = list
				res.TagCands = len(list)
			}
		}
		// 5) 单个汉字：没有二元组可用，走有界 LIKE（只扫最近 2 万条）
		if len(pq.Singles) > 0 && len(pq.Bigrams) == 0 && len(pq.Words) == 0 {
			var maxID int64
			s.db.QueryRow(`SELECT COALESCE(max(id),0) FROM entries`).Scan(&maxID)
			cond := []string{}
			args := []any{maxID - 20000}
			for _, c := range pq.Singles {
				cond = append(cond, `(body LIKE ? OR topic LIKE ? OR tags LIKE ?)`)
				p := "%" + c + "%"
				args = append(args, p, p, p)
			}
			args = append(args, routeLimit)
			routes["like"] = s.ids(`SELECT id FROM entries WHERE id > ? AND `+strings.Join(cond, " AND ")+
				` ORDER BY created_epoch DESC LIMIT ?`, args...)
		}
	}

	mark("routes")
	for name := range routes {
		res.Routes = append(res.Routes, name)
	}
	sort.Strings(res.Routes)

	// RRF 融合
	k := s.W.RRFK
	rrf := map[int64]float64{}
	rank := map[int64]map[string]int{}
	for name, list := range routes {
		for i, id := range list {
			rrf[id] += 1 / (k + float64(i) + 1)
			if rank[id] == nil {
				rank[id] = map[string]int{}
			}
			rank[id][name] = i + 1
		}
	}
	if len(rrf) == 0 {
		return res, nil
	}

	// 候选的元数据 + 筛选
	ids := make([]int64, 0, len(rrf))
	for id := range rrf {
		ids = append(ids, id)
	}
	metas, err := s.candMetas(ids, o, nowT)
	if err != nil {
		return nil, err
	}
	mark("metas")
	res.Candidates = len(metas)
	if len(metas) == 0 {
		return res, nil
	}
	maxRRF := 0.0
	for _, m := range metas {
		if rrf[m.id] > maxRRF {
			maxRRF = rrf[m.id]
		}
	}
	pending := s.pendingHits()

	type scored struct {
		m  candMeta
		ex Explain
		sc float64
	}
	all := make([]scored, 0, len(metas))
	for _, m := range metas {
		ex := Explain{Routes: rank[m.id]}
		ex.Lex = rrf[m.id] / maxRRF
		ex.Rec = s.recency(m.created, m.volatility, nowT)
		ex.Imp = importance(m.kind, m.useCount+pending[m.id])
		if _, ok := rank[m.id]["tag"]; ok {
			ex.TagBoost = s.W.TagBoost
		}
		sc := s.W.Lex*ex.Lex + s.W.Rec*ex.Rec + s.W.Imp*ex.Imp + ex.TagBoost
		all = append(all, scored{m, ex, sc})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].sc > all[j].sc })

	mark("score1")
	// 第二阶段：只给前 3n 算邻近度（原文连续出现才加分）
	n2 := o.Limit * 3
	if n2 > len(all) {
		n2 = len(all)
	}
	top := all[:n2]
	full := map[int64]*Entry{}
	{
		args := make([]any, len(top))
		ph := make([]string, len(top))
		for i, x := range top {
			args[i] = x.m.id
			ph[i] = "?"
		}
		rows, err := s.db.Query(`SELECT `+entryCols+` FROM entries WHERE id IN (`+strings.Join(ph, ",")+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			if e, err := scanEntry(rows); err == nil {
				full[e.ID] = e
			}
		}
		rows.Close()
	}
	mark("load")
	for i := range top {
		e := full[top[i].m.id]
		if e == nil {
			continue
		}
		if pq.Raw != "" {
			top[i].ex.Prox = proximity(pq, e, s.termVariants)
		}
		top[i].sc += s.W.Prox * top[i].ex.Prox
	}
	sort.Slice(top, func(i, j int) bool { return top[i].sc > top[j].sc })
	// 近似重复折叠：和排在前面的某条几乎一样（同一件事的不同版本、补录的同类事实）就挪到后面，
	// 让有限的名额留给不同的信息。只挪不删：名额够就还在。
	if !o.NoFold && len(top) > 1 {
		var picked, later []int
		sigs := make([][]uint64, len(top))
		i := 0
		for ; i < len(top) && len(picked) < o.Limit; i++ { // 选够 limit 条就停，后面的不用算
			if e := full[top[i].m.id]; e != nil {
				sigs[i] = shingles(e.Topic + " " + e.Body)
			}
			dup := false
			for _, j := range picked {
				if jaccard(sigs[i], sigs[j]) >= foldJaccard {
					dup = true
					break
				}
			}
			if dup {
				later = append(later, i)
			} else {
				picked = append(picked, i)
			}
		}
		for ; i < len(top); i++ {
			picked = append(picked, i)
		}
		if len(later) > 0 {
			order := append(picked, later...)
			nt := make([]scored, len(top))
			for k, i := range order {
				nt[k] = top[i]
			}
			top = nt
			res.Folded = len(later)
		}
	}
	if len(top) > o.Limit {
		top = top[:o.Limit]
	}
	for _, x := range top {
		e := full[x.m.id]
		if e == nil {
			continue
		}
		e.UseCount += pending[e.ID]
		h := Hit{Entry: e, Score: round3(x.sc)}
		if o.Explain {
			ex := x.ex
			ex.Lex, ex.Prox, ex.Rec, ex.Imp = round3(ex.Lex), round3(ex.Prox), round3(ex.Rec), round3(ex.Imp)
			h.Explain = &ex
		}
		res.Hits = append(res.Hits, h)
	}
	if len(res.Hits) > 0 {
		_, res.TagTop1 = rank[res.Hits[0].ID]["tag"]
	}
	return res, nil
}

// bm25 要给每个命中打分（每条都要查一次文档长度），高频词命中几千条时这是检索的大头。
// 命中少：全部按 bm25 排；命中多：高频词本身区分度就低，只在最新的 bm25Window 条里按 bm25 排，
// 再补上「原则/决策/判断/教训」这些不该因为旧就被漏掉的条目。
const bm25Full = 1500
const bm25Window = 600

func (s *Store) ftsRoute(table, match string) []int64 {
	var n int
	s.db.QueryRow(`SELECT count(*) FROM (SELECT 1 FROM `+table+` WHERE `+table+` MATCH ? LIMIT ?)`, match, bm25Full+1).Scan(&n)
	if n == 0 {
		return nil
	}
	if n <= bm25Full {
		return s.ids(`SELECT rowid FROM `+table+` WHERE `+table+` MATCH ? ORDER BY bm25(`+table+`) LIMIT ?`, match, routeLimit)
	}
	var floor int64
	s.db.QueryRow(`SELECT rowid FROM `+table+` WHERE `+table+` MATCH ? ORDER BY rowid DESC LIMIT 1 OFFSET ?`, match, bm25Window-1).Scan(&floor)
	list := s.ids(`SELECT rowid FROM `+table+` WHERE `+table+` MATCH ? AND rowid >= ? ORDER BY bm25(`+table+`) LIMIT ?`,
		match, floor, routeLimit)
	seen := make(map[int64]bool, len(list))
	for _, id := range list {
		seen[id] = true
	}
	return appendUnique(list, seen, s.ids(`SELECT id FROM entries WHERE kind IN ('principle','decision','judgment','lesson')
		AND id < ? AND id IN (SELECT rowid FROM `+table+` WHERE `+table+` MATCH ?) ORDER BY created_epoch DESC LIMIT 100`, floor, match))
}

func round3(f float64) float64 { return float64(int64(f*1000+0.5)) / 1000 }

func (s *Store) candMetas(ids []int64, o SearchOpts, t time.Time) ([]candMeta, error) {
	var tagSet map[int64]bool
	if o.Tag != "" {
		tagSet = map[int64]bool{}
		for _, id := range s.ids(`SELECT eid FROM entry_tags WHERE tag=?`, strings.TrimPrefix(o.Tag, "#")) {
			tagSet[id] = true
		}
	}
	ts := t.Unix()
	out := make([]candMeta, 0, len(ids))
	s.metaMu.RLock()
	defer s.metaMu.RUnlock()
	for _, id := range ids {
		m := s.metas[id]
		if m == nil {
			continue
		}
		if !o.History && !m.active(ts) {
			continue
		}
		if o.Kind != "" && m.kind != o.Kind {
			continue
		}
		if (o.Since > 0 && m.created < o.Since) || (o.Until > 0 && m.created >= o.Until) {
			continue
		}
		if o.Topic != "" && !strings.HasPrefix(m.topic, o.Topic) {
			continue
		}
		if tagSet != nil && !tagSet[id] {
			continue
		}
		out = append(out, *m)
	}
	return out, nil
}

// proximity：检索词在原文里连续出现的程度（bigram AND 只说明字都在，不说明词真的在）。
func proximity(pq Query, e *Entry, variants func(string) []string) float64 {
	text := strings.ToLower(e.Topic + "\n" + strings.Join(e.Tags, " ") + "\n" + e.Body)
	if len(pq.Terms) == 0 {
		return 0
	}
	hit := 0
	for _, t := range pq.Terms {
		for _, v := range variants(t) {
			if strings.Contains(text, strings.ToLower(v)) {
				hit++
				break
			}
		}
	}
	frac := float64(hit) / float64(len(pq.Terms))
	whole := 0.0
	if strings.Contains(text, strings.ToLower(strings.Join(strings.Fields(pq.Raw), " "))) {
		whole = 1
	}
	if len(pq.Terms) == 1 {
		return frac
	}
	return 0.6*frac + 0.4*whole
}

// ---------------- 访问计数：读路径只记内存，consolidate / 定时回写 ----------------

func (s *Store) countHits(hits []Hit) {
	if len(hits) == 0 {
		return
	}
	s.hitsMu.Lock()
	for _, h := range hits {
		s.hits[h.ID]++
	}
	s.hitsMu.Unlock()
}

func (s *Store) pendingHits() map[int64]int {
	s.hitsMu.Lock()
	defer s.hitsMu.Unlock()
	cp := make(map[int64]int, len(s.hits))
	for k, v := range s.hits {
		cp[k] = v
	}
	return cp
}

// FlushHits 把内存里的访问次数批量写回库；返回写回的条数。
func (s *Store) FlushHits() int {
	s.hitsMu.Lock()
	h := s.hits
	s.hits = map[int64]int{}
	s.hitsMu.Unlock()
	if len(h) == 0 {
		return 0
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		s.mergeBack(h)
		return 0
	}
	for id, n := range h {
		tx.Exec(`UPDATE entries SET use_count=use_count+? WHERE id=?`, n, id)
	}
	if err := tx.Commit(); err != nil {
		s.mergeBack(h)
		return 0
	}
	s.metaMu.Lock()
	for id, n := range h {
		if m := s.metas[id]; m != nil {
			m.useCount += n
		}
	}
	s.metaMu.Unlock()
	// 访问次数只影响重要性的一小部分，不清缓存
	return len(h)
}

func (s *Store) mergeBack(h map[int64]int) {
	s.hitsMu.Lock()
	for k, v := range h {
		s.hits[k] += v
	}
	s.hitsMu.Unlock()
}

const foldJaccard = 0.6

// shingles：文本前 300 字的相邻字对（编码成整数、排好序去重），近似重复判断用。
func shingles(text string) []uint64 {
	out := make([]uint64, 0, 300)
	var prev rune = -1
	n := 0
	for _, r := range text {
		if n >= 300 {
			break
		}
		n++
		r = unicode.ToLower(r)
		if unicode.IsSpace(r) {
			prev = -1
			continue
		}
		if prev >= 0 {
			out = append(out, uint64(prev)<<32|uint64(r))
		}
		prev = r
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	k := 0
	for i, v := range out {
		if i == 0 || v != out[k-1] {
			out[k] = v
			k++
		}
	}
	return out[:k]
}

// jaccard：两个排好序的集合，归并求交。
func jaccard(a, b []uint64) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	i, j, inter := 0, 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			inter++
			i++
			j++
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	return float64(inter) / float64(len(a)+len(b)-inter)
}

// termVariants：一个检索词的所有写法。词本身是某组的成员 → 整组；
// 中文串里包含某个别名（「英伟达财报」里的「英伟达」）→ 把那一段换成组里的每个写法。
func (s *Store) termVariants(t string) []string {
	if vs := s.Expand(t); len(vs) > 1 {
		return vs
	}
	rs := []rune(t)
	if len(rs) < 3 || !isCJK(rs[0]) {
		return []string{t}
	}
	s.aliasMu.RLock()
	best := ""
	for name := range s.aliasOf {
		if utf8.RuneCountInString(name) >= 2 && utf8.RuneCountInString(name) > utf8.RuneCountInString(best) && strings.Contains(t, name) {
			best = name
		}
	}
	s.aliasMu.RUnlock()
	if best == "" {
		return []string{t}
	}
	out := []string{t}
	for _, v := range s.Expand(best)[1:] {
		out = append(out, strings.Replace(t, best, " "+v+" ", 1))
		if len(out) >= 8 {
			break
		}
	}
	return out
}

// termExpr：一个写法 → FTS 表达式（中文二元组 AND，西文词前缀或整词）。
func termExpr(v string, prefix bool) string {
	var parts []string
	for _, tk := range splitTokens(v) {
		if !tk.cjk {
			if prefix {
				parts = append(parts, ftsQuote(tk.text)+"*")
			} else {
				parts = append(parts, ftsQuote(tk.text))
			}
			continue
		}
		rs := []rune(tk.text)
		for i := 0; i+1 < len(rs); i++ {
			parts = append(parts, ftsQuote(string(rs[i:i+2])))
		}
	}
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	return "(" + strings.Join(parts, " AND ") + ")"
}
