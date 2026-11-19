package store

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var pw = strings.Fields(`作息 发布 配图 订阅 价格 额度 模型 算力 芯片 融资 估值 公众号 视频 剪辑 字幕 封面 选题 素材 标题 摘要 数据库 部署 服务器 代理 节点 网关 日志 性能 延迟 缓存 索引 检索 标签 联想 核对 过期 取代 历史 决策 教训 原则 偏好 口味 风格 福叔 研究员 机器人 团队 周报 日报 研判 事件 来源 证据 推断 可信度 关注 方向 竞争 开源 闭源 发布会 财报 营收 毛利 现金流 监管 审批 电力 土地 社区 数据中心 存储 显卡 推理 训练 微调 评测 基准 榜单 排名 版本 截止 套餐 会员 账单 优惠 汇率 利率 股价 市值`)

func TestProfSearch(t *testing.T) {
	if os.Getenv("L5_PROF") == "" {
		t.Skip("性能剖析：L5_PROF=1 go test -run TestProfSearch -v")
	}
	d := t.TempDir()
	s, _ := Open(filepath.Join(d, "p.db"), "")
	defer s.Close()
	r := rand.New(rand.NewSource(1))
	var b strings.Builder
	for i := 0; i < 10000; i++ {
		var sb strings.Builder
		for k := 0; k < 12+r.Intn(30); k++ {
			sb.WriteString(pw[r.Intn(len(pw))])
		}
		fmt.Fprintf(&b, `{"id":%d,"kind":"fact","topic":"%s%s-%d","body":%q,"tags":"%s,x"}`+"\n", i+1, pw[r.Intn(len(pw))], pw[r.Intn(len(pw))], i, sb.String(), pw[r.Intn(len(pw))])
	}
	t0 := time.Now()
	s.Import(strings.NewReader(b.String()))
	t.Logf("import %v", time.Since(t0))
	s.db.Exec(`CREATE VIRTUAL TABLE fx USING fts5(text, tokenize='unicode61 remove_diacritics 2')`)
	s.db.Exec(`INSERT INTO fx(rowid,text) SELECT rowid,text FROM fts_bi`)
	s.db.Exec(`CREATE VIRTUAL TABLE fn USING fts5(text, tokenize='unicode61 remove_diacritics 2', detail=none)`)
	s.db.Exec(`INSERT INTO fn(rowid,text) SELECT rowid,text FROM fts_bi`)
	for _, tb := range []string{"fts_bi", "fx", "fn"} {
		q := `SELECT rowid FROM ` + tb + ` WHERE ` + tb + ` MATCH '"发布"' AND rowid >= 8000 ORDER BY bm25(` + tb + `) LIMIT 300`
		t0 := time.Now()
		for i := 0; i < 20; i++ {
			s.ids(q)
		}
		t.Logf("bm25 %s %v", tb, time.Since(t0)/20)
	}
	pq := parseQuery("发布")
	for name, q := range map[string]string{
		"win_floor":  `SELECT rowid FROM fts_bi WHERE fts_bi MATCH '"发布"' ORDER BY rowid DESC LIMIT 1 OFFSET 599`,
		"win_bm25":   `SELECT rowid FROM fts_bi WHERE fts_bi MATCH '"发布"' AND rowid >= 8000 ORDER BY bm25(fts_bi) LIMIT 300`,
		"win_norank": `SELECT rowid FROM fts_bi WHERE fts_bi MATCH '"发布"' AND rowid >= 8000 LIMIT 300`,
		"imp":        `SELECT id FROM entries WHERE kind IN ('principle','decision','judgment','lesson') AND id < 8000 AND id IN (SELECT rowid FROM fts_bi WHERE fts_bi MATCH '"发布"') ORDER BY created_epoch DESC LIMIT 100`,
		"tagdf":      `SELECT count(*) FROM (SELECT 1 FROM entry_tags WHERE tag='发布' LIMIT 97)`,
		"meta300":    `SELECT id,kind,topic,created_epoch,volatility,use_count FROM entries WHERE id IN (SELECT rowid FROM fts_bi WHERE fts_bi MATCH '"发布"' LIMIT 300)`,
		"bi":         `SELECT rowid FROM fts_bi WHERE fts_bi MATCH '"发布"' ORDER BY bm25(fts_bi) LIMIT 300`,
		"bi_norank":  `SELECT rowid FROM fts_bi WHERE fts_bi MATCH '"发布"' LIMIT 300`,
		"bi_count":   `SELECT count(*) FROM fts_bi WHERE fts_bi MATCH '"发布"'`,
		"bi_rank":    `SELECT rowid FROM fts_bi WHERE fts_bi MATCH '"发布"' ORDER BY rank LIMIT 300`,
		"topic":      `SELECT id FROM entries WHERE topic >= '发布' AND topic < '发布` + "\U0010ffff" + `' ORDER BY created_epoch DESC LIMIT 100`,
	} {
		t0 := time.Now()
		for i := 0; i < 20; i++ {
			s.ids(q)
		}
		t.Logf("%-10s %v (n=%d)", name, time.Since(t0)/20, len(s.ids(q)))
	}
	_ = pq
	traceSearch = true
	s.Search(SearchOpts{Q: "发布", NoCache: true})
	traceSearch = false
	for _, q := range []string{"发布", "作息", "数据", "芝麻", "数据中心"} {
		t0 = time.Now()
		for i := 0; i < 20; i++ {
			s.Search(SearchOpts{Q: q, NoCache: true})
		}
		t.Logf("search %s %v", q, time.Since(t0)/20)
	}
}
