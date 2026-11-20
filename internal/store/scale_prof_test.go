package store

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 规模剖析：L5_SCALE=200000 go test -tags sqlite_fts5 -run TestScaleProf -v ./internal/store/
func TestScaleProf(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("L5_SCALE"))
	if n == 0 {
		t.Skip("L5_SCALE=200000")
	}
	d := os.Getenv("L5_SCALE_DIR")
	if d == "" {
		d = t.TempDir()
	}
	db := filepath.Join(d, fmt.Sprintf("scale-%d.db", n))
	_, statErr := os.Stat(db)
	tOpen := time.Now()
	s, _ := Open(db, "")
	t.Logf("open %v", time.Since(tOpen))
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	t.Logf("heap after open %.0f MB (metas %d)", float64(ms.HeapAlloc)/1e6, len(s.metas))
	defer s.Close()
	r := rand.New(rand.NewSource(1))
	if statErr != nil {
		var b strings.Builder
		for i := 0; i < n; i++ {
			var sb strings.Builder
			for k := 0; k < 12+r.Intn(30); k++ {
				sb.WriteString(pw[r.Intn(len(pw))])
			}
			kind := "fact"
			if i%10 == 0 {
				kind = "judgment"
			}
			fmt.Fprintf(&b, `{"id":%d,"kind":"%s","topic":"%s%s-%d","body":%q,"tags":"%s,x","created_epoch":%d}`+"\n", i+1, kind, pw[r.Intn(len(pw))], pw[r.Intn(len(pw))], i, sb.String(), pw[r.Intn(len(pw))], time.Now().Unix()-int64(r.Intn(400*86400)))
		}
		t0 := time.Now()
		s.Import(strings.NewReader(b.String()))
		t.Logf("import %v", time.Since(t0))
	}
	fi, _ := os.Stat(db)
	t.Logf("db %.0f MB", float64(fi.Size())/1e6)
	m := func(name, q string, args ...any) {
		s.ids(q, args...)
		t0 := time.Now()
		for i := 0; i < 10; i++ {
			s.ids(q, args...)
		}
		t.Logf("%-12s %8v (n=%d)", name, time.Since(t0)/10, len(s.ids(q, args...)))
	}
	m("bi_count", `SELECT count(*) FROM (SELECT 1 FROM fts_bi WHERE fts_bi MATCH '"发布"' LIMIT 1501)`)
	m("bi_countall", `SELECT count(*) FROM fts_bi WHERE fts_bi MATCH '"发布"'`)
	m("floor", `SELECT rowid FROM fts_bi WHERE fts_bi MATCH '"发布"' ORDER BY rowid DESC LIMIT 1 OFFSET 599`)
	var floor int64
	s.db.QueryRow(`SELECT rowid FROM fts_bi WHERE fts_bi MATCH '"发布"' ORDER BY rowid DESC LIMIT 1 OFFSET 599`).Scan(&floor)
	m("win_bm25", `SELECT rowid FROM fts_bi WHERE fts_bi MATCH '"发布"' AND rowid >= ? ORDER BY bm25(fts_bi) LIMIT 300`, floor)
	m("imp", `SELECT id FROM entries WHERE kind IN ('principle','decision','judgment','lesson') AND id < ? AND id IN (SELECT rowid FROM fts_bi WHERE fts_bi MATCH '"发布"') ORDER BY created_epoch DESC LIMIT 100`, floor)
	m("imp_new", `SELECT rowid FROM fts_imp WHERE fts_imp MATCH '"发布"' AND rowid < ? ORDER BY rowid DESC LIMIT 100`, floor)
	m("topic", `SELECT id FROM entries WHERE topic >= '发布' AND topic < '发布`+"\U0010ffff"+`' ORDER BY created_epoch DESC LIMIT 100`)
	m("tri", `SELECT rowid FROM fts_tri WHERE fts_tri MATCH '"数据中心"' LIMIT 300`)
	mq := `"数据" AND "据中" AND "中心"`
	m("q4_count", `SELECT count(*) FROM (SELECT 1 FROM fts_bi WHERE fts_bi MATCH ? LIMIT 1501)`, mq)
	m("q4_floor", `SELECT rowid FROM fts_bi WHERE fts_bi MATCH ? ORDER BY rowid DESC LIMIT 1 OFFSET 599`, mq)
	m("q4_bm25all", `SELECT rowid FROM fts_bi WHERE fts_bi MATCH ? ORDER BY bm25(fts_bi) LIMIT 300`, mq)
	m("q4_imp", `SELECT id FROM entries WHERE kind IN ('principle','decision','judgment','lesson') AND id < 190000 AND id IN (SELECT rowid FROM fts_bi WHERE fts_bi MATCH ?) ORDER BY created_epoch DESC LIMIT 100`, mq)
	m("q4_tri_all", `SELECT count(*) FROM fts_tri WHERE fts_tri MATCH '"数据中心"'`)
	m("q4_tri_bm25", `SELECT rowid FROM fts_tri WHERE fts_tri MATCH '"数据中心"' ORDER BY bm25(fts_tri) LIMIT 300`)
	m("tag_df", `SELECT count(*) FROM (SELECT 1 FROM entry_tags WHERE tag='发布' LIMIT 97)`)
	traceSearch = true
	for _, q := range []string{"发布", "数据中心", "芝麻"} {
		fmt.Println("== trace", q)
		s.Search(SearchOpts{Q: q, NoCache: true})
	}
	traceSearch = false
	for _, q := range []string{"发布", "作息", "数据", "芝麻", "数据中心", "发布 模型"} {
		s.Search(SearchOpts{Q: q, NoCache: true})
		t0 := time.Now()
		for i := 0; i < 10; i++ {
			s.Search(SearchOpts{Q: q, NoCache: true})
		}
		t.Logf("search %-8s %v", q, time.Since(t0)/10)
	}
}
