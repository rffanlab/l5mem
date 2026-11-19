package store

import (
	"bufio"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

type Stats struct {
	Entries     int            `json:"entries"`
	Active      int            `json:"active"`
	Superseded  int            `json:"superseded"`
	Retracted   int            `json:"retracted"`
	Tags        int            `json:"tags"`
	ByKind      map[string]int `json:"by_kind"`
	ByVol       map[string]int `json:"by_volatility"`
	Due         int            `json:"due"`
	Overdue7    int            `json:"overdue_7d"`
	Unverified  int            `json:"unverified"`
	DBBytes     int64          `json:"db_bytes"`
	Generation  uint64         `json:"generation"`
	PendingHits int            `json:"pending_hits"`
}

func (s *Store) Stats() (*Stats, error) {
	st := &Stats{ByKind: map[string]int{}, ByVol: map[string]int{}}
	t := time.Now().Unix()
	s.db.QueryRow(`SELECT count(*),
		sum(superseded_by IS NULL AND (valid_to IS NULL OR valid_to > ?)),
		sum(superseded_by IS NOT NULL),
		sum(superseded_by IS NULL AND valid_to IS NOT NULL AND valid_to <= ?),
		sum(verified=0) FROM entries`, t, t).Scan(&st.Entries, nullInt(&st.Active), nullInt(&st.Superseded), nullInt(&st.Retracted), nullInt(&st.Unverified))
	s.db.QueryRow(`SELECT count(DISTINCT tag) FROM entry_tags`).Scan(&st.Tags)
	rows, _ := s.db.Query(`SELECT kind, count(*) FROM entries WHERE superseded_by IS NULL GROUP BY kind`)
	for rows != nil && rows.Next() {
		var k string
		var n int
		rows.Scan(&k, &n)
		st.ByKind[k] = n
	}
	if rows != nil {
		rows.Close()
	}
	rows, _ = s.db.Query(`SELECT volatility, count(*) FROM entries WHERE superseded_by IS NULL GROUP BY volatility`)
	for rows != nil && rows.Next() {
		var k string
		var n int
		rows.Scan(&k, &n)
		st.ByVol[k] = n
	}
	if rows != nil {
		rows.Close()
	}
	active := `superseded_by IS NULL AND (valid_to IS NULL OR valid_to > ?)`
	s.db.QueryRow(`SELECT count(*) FROM entries WHERE recheck_epoch <= ? AND `+active, t, t).Scan(&st.Due)
	s.db.QueryRow(`SELECT count(*) FROM entries WHERE recheck_epoch <= ? AND `+active, t-7*86400, t).Scan(&st.Overdue7)
	if fi, err := os.Stat(s.path); err == nil {
		st.DBBytes = fi.Size()
	}
	if fi, err := os.Stat(s.path + "-wal"); err == nil {
		st.DBBytes += fi.Size()
	}
	st.Generation = s.Generation()
	st.PendingHits = len(s.pendingHits())
	return st, nil
}

type nullIntScanner struct{ p *int }

func (n nullIntScanner) Scan(v any) error {
	switch x := v.(type) {
	case int64:
		*n.p = int(x)
	case nil:
		*n.p = 0
	}
	return nil
}

func nullInt(p *int) sql.Scanner { return nullIntScanner{p} }

type TagCount struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

func (s *Store) Tags(limit int) ([]TagCount, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT t.tag, count(*) c FROM entry_tags t JOIN entries e ON e.id=t.eid
		WHERE e.superseded_by IS NULL GROUP BY t.tag ORDER BY c DESC, t.tag LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TagCount
	for rows.Next() {
		var tc TagCount
		rows.Scan(&tc.Tag, &tc.Count)
		out = append(out, tc)
	}
	return out, nil
}

// Doctor 体检：索引行数、孤儿标签、标签与本体不一致、FTS 完整性。
type DoctorReport struct {
	OK          bool     `json:"ok"`
	Entries     int      `json:"entries"`
	FTSBigram   int      `json:"fts_bigram"`
	FTSTrigram  int      `json:"fts_trigram"`
	TagRows     int      `json:"tag_rows"`
	OrphanTags  int      `json:"orphan_tags"`
	TagMismatch int      `json:"tag_mismatch"`
	BrokenLinks int      `json:"broken_supersede_links"`
	Problems    []string `json:"problems"`
}

func (s *Store) Doctor() (*DoctorReport, error) {
	r := &DoctorReport{Problems: []string{}}
	s.db.QueryRow(`SELECT count(*) FROM entries`).Scan(&r.Entries)
	s.db.QueryRow(`SELECT count(*) FROM fts_bi`).Scan(&r.FTSBigram)
	s.db.QueryRow(`SELECT count(*) FROM fts_tri`).Scan(&r.FTSTrigram)
	s.db.QueryRow(`SELECT count(*) FROM entry_tags`).Scan(&r.TagRows)
	s.db.QueryRow(`SELECT count(*) FROM entry_tags t LEFT JOIN entries e ON e.id=t.eid WHERE e.id IS NULL`).Scan(&r.OrphanTags)
	s.db.QueryRow(`SELECT count(*) FROM entries e WHERE e.superseded_by IS NOT NULL AND NOT EXISTS (SELECT 1 FROM entries x WHERE x.id=e.superseded_by)`).Scan(&r.BrokenLinks)
	// 标签：本体 tags 字段 vs 倒排表
	rows, err := s.db.Query(`SELECT id, tags FROM entries`)
	if err == nil {
		want := map[int64]int{}
		for rows.Next() {
			var id int64
			var tags string
			rows.Scan(&id, &tags)
			want[id] = len(splitTags(tags))
		}
		rows.Close()
		got := map[int64]int{}
		rows2, _ := s.db.Query(`SELECT eid, count(*) FROM entry_tags GROUP BY eid`)
		for rows2 != nil && rows2.Next() {
			var id int64
			var n int
			rows2.Scan(&id, &n)
			got[id] = n
		}
		if rows2 != nil {
			rows2.Close()
		}
		for id, n := range want {
			if got[id] != n {
				r.TagMismatch++
			}
		}
	}
	if r.FTSBigram != r.Entries {
		r.Problems = append(r.Problems, fmt.Sprintf("二元组索引 %d 行，本体 %d 行", r.FTSBigram, r.Entries))
	}
	if r.FTSTrigram != r.Entries {
		r.Problems = append(r.Problems, fmt.Sprintf("三元组索引 %d 行，本体 %d 行", r.FTSTrigram, r.Entries))
	}
	if r.OrphanTags > 0 {
		r.Problems = append(r.Problems, fmt.Sprintf("%d 个孤儿标签", r.OrphanTags))
	}
	if r.TagMismatch > 0 {
		r.Problems = append(r.Problems, fmt.Sprintf("%d 条记忆的标签和倒排表对不上", r.TagMismatch))
	}
	if r.BrokenLinks > 0 {
		r.Problems = append(r.Problems, fmt.Sprintf("%d 条取代关系指向不存在的条目", r.BrokenLinks))
	}
	for _, t := range []string{"fts_bi", "fts_tri"} {
		if _, err := s.db.Exec(`INSERT INTO ` + t + `(` + t + `) VALUES('integrity-check')`); err != nil {
			r.Problems = append(r.Problems, t+" 完整性检查失败："+err.Error())
		}
	}
	if err := s.db.QueryRow(`PRAGMA quick_check`).Scan(new(string)); err != nil {
		r.Problems = append(r.Problems, "quick_check："+err.Error())
	}
	r.OK = len(r.Problems) == 0
	return r, nil
}

// Repair 从本体全量重建全部索引（索引坏了不丢数据）。
func (s *Store) Repair() (int, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, q := range []string{`DELETE FROM fts_bi`, `DELETE FROM fts_tri`, `DELETE FROM entry_tags`} {
		if _, err := tx.Exec(q); err != nil {
			return 0, err
		}
	}
	rows, err := tx.Query(`SELECT id, topic, body, tags FROM entries`)
	if err != nil {
		return 0, err
	}
	type row struct {
		id               int64
		topic, body, tag string
	}
	var all []row
	for rows.Next() {
		var r row
		rows.Scan(&r.id, &r.topic, &r.body, &r.tag)
		all = append(all, r)
	}
	rows.Close()
	for _, r := range all {
		if err := indexEntry(tx, r.id, r.topic, r.body, splitTags(r.tag)); err != nil {
			return 0, err
		}
	}
	for _, t := range []string{"fts_bi", "fts_tri"} {
		tx.Exec(`INSERT INTO ` + t + `(` + t + `) VALUES('optimize')`)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	s.bump()
	return len(all), nil
}

type ConsolidateReport struct {
	HitsFlushed int      `json:"hits_flushed"`
	Due         int      `json:"due"`
	Overdue7    []*Entry `json:"overdue_7d"` // 过期 ≥7 天还没核对：升级告警
	Checkpoint  string   `json:"checkpoint"`
}

// Consolidate 维护：访问计数回写、逾期升级、WAL checkpoint。
func (s *Store) Consolidate() (*ConsolidateReport, error) {
	r := &ConsolidateReport{HitsFlushed: s.FlushHits()}
	due, _ := s.Due(0, 500)
	r.Due = len(due)
	for _, e := range due {
		if e.Overdue >= 7 {
			r.Overdue7 = append(r.Overdue7, e)
		}
	}
	var a, b, c int
	if err := s.db.QueryRow(`PRAGMA wal_checkpoint(PASSIVE)`).Scan(&a, &b, &c); err == nil {
		r.Checkpoint = fmt.Sprintf("%d/%d", c, b)
	}
	return r, nil
}

// ---------------- 导出 / 导入（无损迁移：指纹断言 + 回读自检） ----------------

func (s *Store) Export(w io.Writer) (int, string, error) {
	rows, err := s.db.Query(`SELECT ` + entryCols + `,volatility_explicit,COALESCE(last_verified_epoch,0) FROM entries ORDER BY id`)
	if err != nil {
		return 0, "", err
	}
	defer rows.Close()
	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	h := sha256.New()
	n := 0
	for rows.Next() {
		var e Entry
		var tags, meta string
		var verified, volExp int
		var validTo, sup, recheck sql.NullInt64
		var lve int64
		if err := rows.Scan(&e.ID, &e.Kind, &e.Topic, &e.Body, &tags, &e.Source, &e.CreatedAt, &e.CreatedEpoch, &e.Caller, &validTo,
			&sup, &e.Volatility, &verified, &recheck, &e.LastVerifiedAt, &e.LastVerifiedBy, &e.LastVerifiedNote, &e.UseCount,
			&e.ContentHash, &meta, &volExp, &lve); err != nil {
			return n, "", err
		}
		rec := map[string]any{"id": e.ID, "kind": e.Kind, "topic": e.Topic, "body": e.Body, "tags": tags, "source": e.Source,
			"created_at": e.CreatedAt, "created_epoch": e.CreatedEpoch, "caller": e.Caller, "volatility": e.Volatility,
			"volatility_explicit": volExp, "verified": verified, "use_count": e.UseCount, "content_hash": e.ContentHash,
			"meta": json.RawMessage(orJSON(meta)), "last_verified_at": e.LastVerifiedAt, "last_verified_by": e.LastVerifiedBy,
			"last_verified_note": e.LastVerifiedNote, "last_verified_epoch": lve}
		if validTo.Valid {
			rec["valid_to"] = validTo.Int64
		}
		if sup.Valid {
			rec["superseded_by"] = sup.Int64
		}
		if recheck.Valid {
			rec["recheck_epoch"] = recheck.Int64
		}
		fingerprint(h, e.ID, e.Kind, e.Topic, e.Body, tags, e.Source, e.CreatedAt)
		if err := enc.Encode(rec); err != nil {
			return n, "", err
		}
		n++
	}
	// 同义词组也一起导出（不计入指纹：指纹只管记忆本身）
	for _, g := range s.Aliases() {
		if err := enc.Encode(map[string]any{"alias_group": g.Canon, "names": g.Names}); err != nil {
			return n, "", err
		}
	}
	return n, hex.EncodeToString(h.Sum(nil)), bw.Flush()
}

func orJSON(s string) string {
	if s == "" || !json.Valid([]byte(s)) {
		return "{}"
	}
	return s
}

func fingerprint(h io.Writer, id int64, kind, topic, body, tags, source, created string) {
	fmt.Fprintf(h, "%d\x1f%s\x1f%s\x1f%s\x1f%s\x1f%s\x1f%s\x1e", id, kind, topic, body, normTags(tags), source, created)
}

func normTags(t string) string { return strings.Join(splitTags(t), ",") }

type ImportReport struct {
	Aliases     int      `json:"aliases,omitempty"` // 导入的同义词组
	Read        int      `json:"read"`
	Inserted    int      `json:"inserted"`
	Skipped     int      `json:"skipped_existing"`
	Fingerprint string   `json:"fingerprint"`
	Verified    bool     `json:"verified"`
	SelfFind    int      `json:"self_find_ok"`
	SelfChecked int      `json:"self_checked"`
	InsertMs    float64  `json:"insert_ms"`
	Problems    []string `json:"problems"`
}

type importRec struct {
	ID                 int64           `json:"id"`
	Kind               string          `json:"kind"`
	Topic              string          `json:"topic"`
	Body               string          `json:"body"`
	Tags               json.RawMessage `json:"tags"`
	Source             string          `json:"source"`
	CreatedAt          string          `json:"created_at"`
	CreatedEpoch       int64           `json:"created_epoch"`
	Caller             string          `json:"caller"`
	ValidTo            *int64          `json:"valid_to"`
	SupersededBy       *int64          `json:"superseded_by"`
	Volatility         string          `json:"volatility"`
	VolatilityExplicit int             `json:"volatility_explicit"`
	Verified           *int            `json:"verified"`
	RecheckEpoch       *int64          `json:"recheck_epoch"`
	LastVerifiedAt     string          `json:"last_verified_at"`
	LastVerifiedEpoch  int64           `json:"last_verified_epoch"`
	LastVerifiedBy     string          `json:"last_verified_by"`
	LastVerifiedNote   string          `json:"last_verified_note"`
	UseCount           int             `json:"use_count"`
	Meta               json.RawMessage `json:"meta"`
}

func (r *importRec) tagString() string {
	if len(r.Tags) == 0 {
		return ""
	}
	var list []string
	if json.Unmarshal(r.Tags, &list) == nil {
		return strings.Join(splitTags(strings.Join(list, ",")), ",")
	}
	var s string
	json.Unmarshal(r.Tags, &s)
	return strings.Join(splitTags(s), ",")
}

// Import 导入 JSONL（Export 的格式，或旧库转出来的同名字段）。保留 id，取代链原样搬；
// 完成后做三重断言：行数一致 → 指纹一致 → 每条用自己的主题能检索回自己。
func (s *Store) Import(r io.Reader) (*ImportReport, error) {
	rep := &ImportReport{Problems: []string{}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	var recs []importRec
	var aliasLines [][]string
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, `{"alias_group"`) {
			var g struct {
				Names []string `json:"names"`
			}
			if json.Unmarshal([]byte(line), &g) == nil && len(g.Names) > 1 {
				aliasLines = append(aliasLines, g.Names)
			}
			continue
		}
		var rec importRec
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return nil, fmt.Errorf("第 %d 行不是合法 JSON：%w", rep.Read+1, err)
		}
		recs = append(recs, rec)
		rep.Read++
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	h := sha256.New()
	tIns := time.Now()
	s.wmu.Lock()
	tx, err := s.db.Begin()
	if err != nil {
		s.wmu.Unlock()
		return nil, err
	}
	for i := range recs {
		x := &recs[i]
		kind, err := NormKind(x.Kind)
		if err != nil {
			tx.Rollback()
			s.wmu.Unlock()
			return nil, fmt.Errorf("#%d：%w", x.ID, err)
		}
		x.Kind = kind
		tags := x.tagString()
		if x.CreatedEpoch == 0 {
			if t, err := time.ParseInLocation("2006-01-02 15:04:05", x.CreatedAt, time.Local); err == nil {
				x.CreatedEpoch = t.Unix()
			} else if t, err := time.Parse(time.RFC3339, x.CreatedAt); err == nil {
				x.CreatedEpoch = t.Unix()
			} else {
				x.CreatedEpoch = time.Now().Unix()
			}
		}
		if x.CreatedAt == "" {
			x.CreatedAt = fmtTime(time.Unix(x.CreatedEpoch, 0))
		}
		if x.Volatility == "" {
			x.Volatility = Classify(kind, x.Topic, x.Body, splitTags(tags))
		}
		verified := 1
		if x.Verified != nil {
			verified = *x.Verified
		}
		if x.RecheckEpoch == nil {
			base := x.CreatedEpoch
			if x.LastVerifiedEpoch > 0 {
				base = x.LastVerifiedEpoch
			}
			x.RecheckEpoch = recheckFor(x.Volatility, verified == 1, time.Unix(base, 0))
		}
		hash := ContentHash(kind, x.Topic, x.Body)
		var idArg any
		if x.ID > 0 {
			idArg = x.ID
		}
		res, err := tx.Exec(`INSERT INTO entries(id,kind,topic,body,tags,source,created_at,created_epoch,caller,valid_to,superseded_by,
			volatility,volatility_explicit,verified,recheck_epoch,last_verified_at,last_verified_epoch,last_verified_by,last_verified_note,
			use_count,content_hash,meta) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,NULLIF(?,''),NULLIF(?,0),NULLIF(?,''),NULLIF(?,''),?,?,?)
			ON CONFLICT DO NOTHING`,
			idArg, kind, x.Topic, x.Body, tags, x.Source, x.CreatedAt, x.CreatedEpoch, x.Caller, x.ValidTo, x.SupersededBy,
			x.Volatility, x.VolatilityExplicit, verified, x.RecheckEpoch, x.LastVerifiedAt, x.LastVerifiedEpoch, x.LastVerifiedBy,
			x.LastVerifiedNote, x.UseCount, hash, orJSON(string(x.Meta)))
		if err != nil {
			tx.Rollback()
			s.wmu.Unlock()
			return nil, fmt.Errorf("#%d 写入失败：%w", x.ID, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			rep.Skipped++
			continue
		}
		id, _ := res.LastInsertId()
		x.ID = id
		if err := indexEntry(tx, id, x.Topic, x.Body, splitTags(tags)); err != nil {
			tx.Rollback()
			s.wmu.Unlock()
			return nil, err
		}
		fingerprint(h, id, kind, x.Topic, x.Body, tags, x.Source, x.CreatedAt)
		rep.Inserted++
	}
	if err := tx.Commit(); err != nil {
		s.wmu.Unlock()
		return nil, err
	}
	s.wmu.Unlock()
	s.bump()
	s.loadMetas()
	rep.InsertMs = float64(time.Since(tIns).Microseconds()) / 1000
	rep.Fingerprint = hex.EncodeToString(h.Sum(nil))

	// 断言：导入的每一条都能按 id 读回、内容一致，并且用自己的主题（或正文开头）能检索到自己
	// 指纹全量比对；检索自检超过 2000 条时均匀抽样（每条都查一遍是 O(n) 次检索）
	step := 1
	if len(recs) > 2000 {
		step = len(recs) / 2000
	}
	h2 := sha256.New()
	for i, x := range recs {
		if x.ID == 0 {
			continue
		}
		e, err := s.Get(x.ID)
		if err != nil {
			rep.Problems = append(rep.Problems, fmt.Sprintf("#%d 读不回来", x.ID))
			continue
		}
		fingerprint(h2, e.ID, e.Kind, e.Topic, e.Body, strings.Join(e.Tags, ","), e.Source, e.CreatedAt)
		if i%step != 0 {
			continue
		}
		rep.SelfChecked++
		// 用主题限定范围 + 正文开头去查，主题被很多条共用时也能定位到自己
		rs := []rune(e.Body)
		if len(rs) > 24 {
			rs = rs[:24]
		}
		q := string(rs)
		sr, err := s.Search(SearchOpts{Q: q, Topic: e.Topic, Limit: 50, History: true, NoCache: true})
		found := false
		if err == nil {
			for _, hh := range sr.Hits {
				if hh.ID == e.ID {
					found = true
					break
				}
			}
		}
		if found {
			rep.SelfFind++
		} else if len(rep.Problems) < 20 {
			rep.Problems = append(rep.Problems, fmt.Sprintf("#%d 用「%s」检索不到自己", e.ID, q))
		}
	}
	for _, names := range aliasLines {
		if _, err := s.AddAliases(names); err == nil {
			rep.Aliases++
		}
	}
	if rep.Skipped == 0 {
		rep.Verified = hex.EncodeToString(h2.Sum(nil)) == rep.Fingerprint && len(rep.Problems) == 0
	} else {
		rep.Verified = len(rep.Problems) == 0
	}
	return rep, nil
}

// ImportSQLite 从别的 L5 库（例如 Hermes 机器人的 Python 版）只读打开、按同名字段搬过来。
func (s *Store) ImportSQLite(path string) (*ImportReport, error) {
	src, err := sql.Open("sqlite3", "file:"+path+"?mode=ro&_query_only=1")
	if err != nil {
		return nil, err
	}
	defer src.Close()
	rows, err := src.Query(`SELECT * FROM entries ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("旧库没有 entries 表：%w", err)
	}
	cols, _ := rows.Columns()
	pr, pw := io.Pipe()
	go func() {
		enc := json.NewEncoder(pw)
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				pw.CloseWithError(err)
				return
			}
			rec := map[string]any{}
			for i, c := range cols {
				v := vals[i]
				if b, ok := v.([]byte); ok {
					v = string(b)
				}
				if c == "meta" {
					if s, ok := v.(string); ok && json.Valid([]byte(s)) {
						v = json.RawMessage(s)
					} else {
						continue
					}
				}
				rec[c] = v
			}
			enc.Encode(rec)
		}
		rows.Close()
		pw.Close()
	}()
	return s.Import(pr)
}
