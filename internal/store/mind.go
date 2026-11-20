package store

import (
	"math"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// 「心头在想什么」和「新冒出来的连接」。
//
// 心头（激活度）：仿 ACT-R 的基础激活——每个标签每被「用」一次记一笔，按距今天数衰减：
//   A = ln Σ uses_d × (age_d + 1)^-0.5
// 昨天用了 3 次比半年前用了 30 次更在心头。「用」只算真实发生的：写入（2 分）、被引用为推理依据（3 分）、
// 被核对（2 分）、出现在别人检索结果的前 3 条（1 分）。巡检、网页浏览这类调用方的检索不算，免得自我强化。
//
// 连接：标签在同一条记忆里一起出现就算连一次。「新连接」= 最近 N 天一起出现了至少 2 次、以前从没一起出现过的两个标签，
// 按「比偶然一起出现多了多少」（PMI）排序，所以和谁都连的大词排不上来。
//
// 太泛的「枢纽标签」（超过 8% 的记忆都挂着它）和系统标签（每日沉思、ic-event-123……）不参与；大词单独列在「背景」里。

const tagDailySchema = `CREATE TABLE IF NOT EXISTS tag_daily(tag TEXT NOT NULL COLLATE NOCASE, day INTEGER NOT NULL, uses REAL NOT NULL,
  PRIMARY KEY(tag, day)) WITHOUT ROWID;`

const (
	useWrite  = 2.0
	useCite   = 3.0
	useVerify = 2.0
	useHit    = 1.0
	hubShare  = 0.08
	actDecay  = 0.5
)

var defaultStopTags = []string{"InfoCollector", "每日沉思", "新旧联想", "假设", "规律", "记忆修正", "假设复盘", "关注方向", "watchdog"}

var sysTagRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*-\d+$`) // ic-event-123 这类机器编号

type mindState struct {
	mu      sync.Mutex
	pending map[[2]any]float64 // (标签, 天) → 分
	hitIDs  map[int64]float64  // 检索命中的条目 → 分（回写时再查它们的标签，读路径不碰库）
	stop    map[string]bool
	// 标签文档数缓存：大库上 GROUP BY 要几百毫秒，5 分钟内复用
	dfAt  time.Time
	dfGen uint64
	df    map[string]int
	dfN   int
}

func (s *Store) initMind() error {
	var has int
	s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='tag_daily'`).Scan(&has)
	if _, err := s.db.Exec(tagDailySchema); err != nil {
		return err
	}
	if has == 0 { // 第一次：拿已有记忆的写入时间补一份历史
		s.db.Exec(`INSERT INTO tag_daily(tag, day, uses) SELECT t.tag, e.created_epoch/86400, count(*)*? FROM entry_tags t
			JOIN entries e ON e.id=t.eid GROUP BY t.tag, e.created_epoch/86400`, useWrite)
	}
	s.mind = &mindState{pending: map[[2]any]float64{}, hitIDs: map[int64]float64{}, stop: map[string]bool{}}
	stops := defaultStopTags
	if v := os.Getenv("L5_MIND_STOP"); v != "" {
		stops = append(stops, strings.Split(v, ",")...)
	}
	for _, t := range stops {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
			s.mind.stop[t] = true
		}
	}
	return nil
}

func (s *Store) sysTag(t string) bool {
	lt := strings.ToLower(t)
	return s.mind.stop[lt] || sysTagRe.MatchString(lt)
}

func (s *Store) useTags(tags []string, w float64, at time.Time) {
	if s.mind == nil {
		return
	}
	day := at.Unix() / 86400
	s.mind.mu.Lock()
	for _, t := range tags {
		if t != "" && !s.sysTag(t) {
			s.mind.pending[[2]any{t, day}] += w
		}
	}
	s.mind.mu.Unlock()
}

func (s *Store) useEntries(ids []int64, w float64) {
	if s.mind == nil {
		return
	}
	s.mind.mu.Lock()
	for _, id := range ids {
		s.mind.hitIDs[id] += w
	}
	s.mind.mu.Unlock()
}

// noteSearch：检索结果的前 3 条算「被想起」。巡检、网页、基准这类调用方不算。
func (s *Store) noteSearch(caller string, hits []Hit) {
	c := strings.ToLower(caller)
	if c == "" || strings.HasPrefix(c, "watchdog") || strings.HasPrefix(c, "ui") || c == "web" || strings.HasPrefix(c, "bench") || strings.HasPrefix(c, "l5") {
		return
	}
	var ids []int64
	for i, h := range hits {
		if i >= 3 {
			break
		}
		ids = append(ids, h.ID)
	}
	s.useEntries(ids, useHit)
}

// flushMind 把攒着的使用记录写进 tag_daily（随访问计数一起定时回写）。
func (s *Store) flushMind() {
	if s.mind == nil {
		return
	}
	s.mind.mu.Lock()
	pend, hits := s.mind.pending, s.mind.hitIDs
	s.mind.pending, s.mind.hitIDs = map[[2]any]float64{}, map[int64]float64{}
	s.mind.mu.Unlock()
	if len(hits) > 0 {
		day := time.Now().Unix() / 86400
		for id, w := range hits {
			rows, err := s.db.Query(`SELECT tag FROM entry_tags WHERE eid=?`, id)
			if err != nil {
				continue
			}
			for rows.Next() {
				var t string
				if rows.Scan(&t) == nil && !s.sysTag(t) {
					pend[[2]any{t, day}] += w
				}
			}
			rows.Close()
		}
	}
	if len(pend) == 0 {
		return
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return
	}
	for k, w := range pend {
		tx.Exec(`INSERT INTO tag_daily(tag, day, uses) VALUES(?,?,?) ON CONFLICT(tag, day) DO UPDATE SET uses=uses+excluded.uses`,
			k[0], k[1], w)
	}
	tx.Commit()
}

// ---------------- 心头 ----------------

type MindTag struct {
	Tag        string  `json:"tag"`
	Activation float64 `json:"activation"`
	Uses7      float64 `json:"uses_7d"`
	UsesPrev   float64 `json:"uses_prev_28d"`
	Trend      string  `json:"trend"` // new / up / flat / down
	Docs       int     `json:"docs"`
	Explore    bool    `json:"explore,omitempty"` // 探索名额：不在最前面，但最近升得最快
	Recent     []Brief `json:"recent,omitempty"`
}

type Brief struct {
	ID    int64  `json:"id"`
	Kind  string `json:"kind"`
	Topic string `json:"topic"`
}

type MindResult struct {
	Tags       []MindTag `json:"tags"`
	Background []MindTag `json:"background"` // 太泛的大词：一直在，但想到了也没新东西
	Entries    int       `json:"entries"`
}

func (s *Store) tagDF() (map[string]int, int) {
	s.mind.mu.Lock()
	if s.mind.df != nil && time.Since(s.mind.dfAt) < 5*time.Minute && s.mind.dfGen == s.Generation() {
		df, n := s.mind.df, s.mind.dfN
		s.mind.mu.Unlock()
		return df, n
	}
	s.mind.mu.Unlock()
	df := map[string]int{}
	rows, err := s.db.Query(`SELECT tag, count(*) FROM entry_tags GROUP BY tag`)
	if err == nil {
		for rows.Next() {
			var t string
			var n int
			rows.Scan(&t, &n)
			df[strings.ToLower(t)] = n
		}
		rows.Close()
	}
	var n int
	s.db.QueryRow(`SELECT count(*) FROM entries WHERE superseded_by IS NULL`).Scan(&n)
	s.mind.mu.Lock()
	s.mind.df, s.mind.dfN, s.mind.dfAt, s.mind.dfGen = df, n, time.Now(), s.Generation()
	s.mind.mu.Unlock()
	return df, n
}

func (s *Store) Mind(limit int) (*MindResult, error) {
	if limit <= 0 || limit > 50 {
		limit = 12
	}
	s.flushMind()
	today := time.Now().Unix() / 86400
	rows, err := s.db.Query(`SELECT tag, day, uses FROM tag_daily WHERE day >= ?`, today-365)
	if err != nil {
		return nil, err
	}
	type agg struct {
		name          string
		a, r7, prev28 float64
	}
	by := map[string]*agg{}
	for rows.Next() {
		var t string
		var day int64
		var u float64
		rows.Scan(&t, &day, &u)
		if s.sysTag(t) {
			continue
		}
		k := strings.ToLower(t)
		g := by[k]
		if g == nil {
			g = &agg{name: t}
			by[k] = g
		}
		age := float64(today - day)
		if age < 0 {
			age = 0
		}
		g.a += u * math.Pow(age+1, -actDecay)
		if age < 7 {
			g.r7 += u
		} else if age < 35 {
			g.prev28 += u
		}
	}
	rows.Close()
	df, n := s.tagDF()
	res := &MindResult{Tags: []MindTag{}, Background: []MindTag{}, Entries: n}
	var all []MindTag
	for k, g := range by {
		if g.a <= 0 {
			continue
		}
		mt := MindTag{Tag: g.name, Activation: round3(math.Log(g.a)), Uses7: g.r7, UsesPrev: g.prev28, Docs: df[k]}
		weekly := g.prev28 / 4
		switch {
		case g.prev28 == 0 && g.r7 > 0:
			mt.Trend = "new"
		case g.r7 > 1.5*weekly+0.5:
			mt.Trend = "up"
		case g.r7 < 0.67*weekly:
			mt.Trend = "down"
		default:
			mt.Trend = "flat"
		}
		all = append(all, mt)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Activation > all[j].Activation })
	isHub := func(t MindTag) bool { return n >= 50 && float64(t.Docs) > hubShare*float64(n) }
	main := limit - limit/5 // 八成名额给最在心头的，两成留给升得最快的（探索）
	picked := map[string]bool{}
	for _, t := range all {
		if isHub(t) {
			if len(res.Background) < 5 {
				res.Background = append(res.Background, t)
			}
			continue
		}
		if len(res.Tags) < main {
			res.Tags = append(res.Tags, t)
			picked[strings.ToLower(t.Tag)] = true
		}
	}
	var rising []MindTag
	for _, t := range all {
		if !picked[strings.ToLower(t.Tag)] && !isHub(t) && t.Uses7 > 0 && (t.Trend == "new" || t.Trend == "up") {
			rising = append(rising, t)
		}
	}
	sort.Slice(rising, func(i, j int) bool {
		return rising[i].Uses7/(rising[i].UsesPrev/4+1) > rising[j].Uses7/(rising[j].UsesPrev/4+1)
	})
	for _, t := range rising {
		if len(res.Tags) >= limit {
			break
		}
		t.Explore = true
		res.Tags = append(res.Tags, t)
	}
	for i := range res.Tags {
		res.Tags[i].Recent = s.recentByTag(res.Tags[i].Tag, 3)
	}
	return res, nil
}

func (s *Store) recentByTag(tag string, n int) []Brief {
	var out []Brief
	rows, err := s.db.Query(`SELECT e.id, e.kind, e.topic FROM entry_tags t JOIN entries e ON e.id=t.eid
		WHERE t.tag=? AND e.superseded_by IS NULL AND e.valid_to IS NULL ORDER BY e.created_epoch DESC LIMIT ?`, tag, n)
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var b Brief
		rows.Scan(&b.ID, &b.Kind, &b.Topic)
		out = append(out, b)
	}
	return out
}

// ---------------- 新连接 ----------------

type Link struct {
	A       string  `json:"a"`
	B       string  `json:"b"`
	Count   int     `json:"count"` // 最近一起出现的记忆条数
	Prior   int     `json:"prior"` // 窗口之前一起出现的次数
	PMI     float64 `json:"pmi"`
	Score   float64 `json:"score"`
	Entries []Brief `json:"entries"`
}

type LinksResult struct {
	Links []Link `json:"links"`
	Days  int    `json:"days"`
}

// Links 最近 days 天里新冒出来的标签连接。
func (s *Store) Links(days, minCount, maxPrior, limit int) (*LinksResult, error) {
	if days <= 0 {
		days = 7
	}
	if minCount <= 0 {
		minCount = 2
	}
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	since := time.Now().Add(-time.Duration(days) * 24 * time.Hour).Unix()
	rows, err := s.db.Query(`SELECT e.id, e.kind, e.topic, t.tag FROM entries e JOIN entry_tags t ON t.eid=e.id
		WHERE e.created_epoch >= ? AND e.superseded_by IS NULL AND e.valid_to IS NULL ORDER BY e.id`, since)
	if err != nil {
		return nil, err
	}
	df, n := s.tagDF()
	tagsOf := map[int64][]string{}
	brief := map[int64]Brief{}
	for rows.Next() {
		var b Brief
		var t string
		rows.Scan(&b.ID, &b.Kind, &b.Topic, &t)
		brief[b.ID] = b
		if s.sysTag(t) || (n >= 50 && float64(df[strings.ToLower(t)]) > hubShare*float64(n)) {
			continue
		}
		if len(tagsOf[b.ID]) < 8 {
			tagsOf[b.ID] = append(tagsOf[b.ID], t)
		}
	}
	rows.Close()
	type key struct{ a, b string }
	co := map[key][]int64{}
	names := map[string]string{}
	for id, ts := range tagsOf {
		for i := 0; i < len(ts); i++ {
			for j := i + 1; j < len(ts); j++ {
				a, b := strings.ToLower(ts[i]), strings.ToLower(ts[j])
				if a == b {
					continue
				}
				names[a], names[b] = ts[i], ts[j]
				if a > b {
					a, b = b, a
				}
				co[key{a, b}] = append(co[key{a, b}], id)
			}
		}
	}
	res := &LinksResult{Links: []Link{}, Days: days}
	for k, ids := range co {
		if len(ids) < minCount {
			continue
		}
		var prior int
		s.db.QueryRow(`SELECT count(*) FROM entry_tags x JOIN entry_tags y ON y.eid=x.eid AND y.tag=? JOIN entries e ON e.id=x.eid
			WHERE x.tag=? AND e.created_epoch < ?`, k.b, k.a, since).Scan(&prior)
		if prior > maxPrior {
			continue
		}
		da, db := float64(df[k.a]), float64(df[k.b])
		if da == 0 || db == 0 || n == 0 {
			continue
		}
		pmi := math.Log(float64(len(ids)+prior) * float64(n) / (da * db))
		if pmi <= 0 {
			continue
		}
		l := Link{A: names[k.a], B: names[k.b], Count: len(ids), Prior: prior, PMI: round3(pmi), Score: round3(float64(len(ids)) * pmi)}
		sort.Slice(ids, func(i, j int) bool { return ids[i] > ids[j] })
		for i, id := range ids {
			if i >= 5 {
				break
			}
			l.Entries = append(l.Entries, brief[id])
		}
		res.Links = append(res.Links, l)
	}
	sort.Slice(res.Links, func(i, j int) bool { return res.Links[i].Score > res.Links[j].Score })
	if len(res.Links) > limit {
		res.Links = res.Links[:limit]
	}
	return res, nil
}
