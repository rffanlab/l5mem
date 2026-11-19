package store

import (
	"math"
	"sort"
	"strings"
)

type Related struct {
	*Entry
	Score  float64  `json:"score"`
	Shared []string `json:"shared"` // 和锚点共享的标签（第二跳是和中间那条共享的）
	Hop    int      `json:"hop"`
	Via    int64    `json:"via,omitempty"` // 第二跳经由哪一条
}

// Relate 联想：标签图上 1~2 跳扩散，按 IDF 加权——稀有的共享标签才叫关联，人人都打的标签几乎不计分。
func (s *Store) Relate(id int64, limit int) ([]Related, []string, error) {
	if limit <= 0 {
		limit = 8
	}
	if _, err := s.Get(id); err != nil {
		return nil, nil, err
	}
	var total float64
	s.db.QueryRow(`SELECT count(*) FROM entries WHERE superseded_by IS NULL`).Scan(&total)
	if total < 2 {
		return nil, nil, nil
	}
	hubCap := s.tagCap(limit) * 4
	df := map[string]int{}
	tagsOf := func(eid int64) []string {
		rows, err := s.db.Query(`SELECT tag FROM entry_tags WHERE eid=?`, eid)
		if err != nil {
			return nil
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var t string
			rows.Scan(&t)
			out = append(out, t)
		}
		return out
	}
	idf := func(tag string) float64 {
		k := strings.ToLower(tag)
		n, ok := df[k]
		if !ok {
			s.db.QueryRow(`SELECT count(*) FROM entry_tags WHERE tag=?`, tag).Scan(&n)
			df[k] = n
		}
		if n <= 1 {
			return 0
		}
		return math.Log(total / float64(n))
	}
	anchorTags := tagsOf(id)
	var hubs []string
	score := map[int64]float64{}
	shared := map[int64]map[string]bool{}
	hop := map[int64]int{}
	addShared := func(eid int64, tag string) {
		if shared[eid] == nil {
			shared[eid] = map[string]bool{}
		}
		shared[eid][tag] = true
	}
	via := map[int64]int64{}
	expand := func(tags []string, weight float64, h int, exclude map[string]bool, from int64) {
		for _, tg := range tags {
			if exclude[strings.ToLower(tg)] {
				continue
			}
			w := idf(tg)
			if w <= 0 {
				continue
			}
			if df[strings.ToLower(tg)] > hubCap {
				hubs = append(hubs, tg)
				continue
			}
			for _, eid := range s.ids(`SELECT t.eid FROM entry_tags t JOIN entries e ON e.id=t.eid
				WHERE t.tag=? AND e.superseded_by IS NULL AND (e.valid_to IS NULL OR e.valid_to > strftime('%s','now'))`, tg) {
				if eid == id {
					continue
				}
				score[eid] += w * weight
				if hop[eid] == 0 || h < hop[eid] {
					hop[eid] = h
				}
				if h == 1 {
					addShared(eid, tg)
				} else if hop[eid] == 2 {
					if _, ok := via[eid]; !ok {
						via[eid] = from
					}
					addShared(eid, tg)
				}
			}
		}
	}
	expand(anchorTags, 1, 1, nil, 0)
	// 第二跳：从最相关的几个邻居出发，走锚点没有的标签，权重减半
	type kv struct {
		id int64
		s  float64
	}
	var first []kv
	for k, v := range score {
		first = append(first, kv{k, v})
	}
	sort.Slice(first, func(i, j int) bool { return first[i].s > first[j].s })
	ex := map[string]bool{}
	for _, t := range anchorTags {
		ex[strings.ToLower(t)] = true
	}
	maxFirst := 1.0
	if len(first) > 0 {
		maxFirst = first[0].s
	}
	for i := 0; i < len(first) && i < 5; i++ {
		expand(tagsOf(first[i].id), 0.5*first[i].s/maxFirst, 2, ex, first[i].id)
	}
	var all []kv
	for k, v := range score {
		all = append(all, kv{k, v})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].s > all[j].s })
	var out []Related
	for _, x := range all {
		if len(out) >= limit {
			break
		}
		e, err := s.Get(x.id)
		if err != nil || !e.Active {
			continue
		}
		var sh []string
		for t := range shared[x.id] {
			sh = append(sh, t)
		}
		sort.Strings(sh)
		r := Related{Entry: e, Score: round3(x.s), Shared: sh, Hop: hop[x.id]}
		if r.Hop == 2 {
			r.Via = via[x.id]
		}
		out = append(out, r)
	}
	return out, hubs, nil
}
