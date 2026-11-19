package store

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"
)

func hitIDs(r *SearchResult) []int64 {
	var out []int64
	for _, h := range r.Hits {
		out = append(out, h.ID)
	}
	return out
}

func contains(ids []int64, id int64) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func TestAliases(t *testing.T) {
	s := newTest(t)
	en := mustAdd(t, s, AddInput{Topic: "NVIDIA 财报", Body: "NVIDIA data center revenue beat estimates，数据中心收入超预期"})
	cn := mustAdd(t, s, AddInput{Topic: "英伟达禁售", Body: "英伟达暂停向部分客户出货 H200"})
	other := mustAdd(t, s, AddInput{Topic: "AMD 新品", Body: "AMD 发布 MI400"})

	// 没配同义词：查「英伟达」找不到英文那条
	r, _ := s.Search(SearchOpts{Q: "英伟达", Limit: 10})
	if contains(hitIDs(r), en) {
		t.Fatal("没配同义词时不应该找到英文条目")
	}
	if _, err := s.AddAliases([]string{"英伟达", "NVIDIA", "Nvidia", "NVDA"}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"英伟达", "nvidia", "NVDA", "英伟达财报"} {
		r, err := s.Search(SearchOpts{Q: q, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		ids := hitIDs(r)
		if q != "英伟达财报" && (!contains(ids, en) || !contains(ids, cn)) {
			t.Fatalf("查 %q 应当中英文都找到：%v（扩展 %v）", q, ids, r.Expanded)
		}
		if q == "英伟达财报" && !contains(ids, en) {
			t.Fatalf("「英伟达财报」应当找到「NVIDIA 财报」：%v（扩展 %v）", ids, r.Expanded)
		}
		if contains(ids, other) {
			t.Fatalf("查 %q 不该找到 AMD", q)
		}
	}
	// 合并：已有组里的名字和新名字放一起 → 并进原组
	g, _ := s.AddAliases([]string{"NVDA", "老黄家"})
	// NVIDIA 和 Nvidia 只差大小写，算同一个写法（检索本来就不分大小写）
	if len(g.Names) != 4 {
		t.Fatalf("应当合并成 4 个写法：%v", g.Names)
	}
	if err := s.RemoveAlias("老黄家"); err != nil {
		t.Fatal(err)
	}
	if len(s.Expand("英伟达")) != 3 {
		t.Fatalf("移出后剩 3 个：%v", s.Expand("英伟达"))
	}
	// 导出 / 导入带着同义词组
	var buf bytes.Buffer
	if _, _, err := s.Export(&buf); err != nil {
		t.Fatal(err)
	}
	d := t.TempDir()
	s2, err := Open(filepath.Join(d, "b.db"), d)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	rep, err := s2.Import(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Aliases != 1 || len(s2.Expand("nvda")) != 3 {
		t.Fatalf("导入后同义词组不对：%+v %v", rep, s2.Expand("nvda"))
	}
}

func TestFoldNearDuplicates(t *testing.T) {
	s := newTest(t)
	base := "CoreWeave 第三季度收入 36.6 亿美元，积压订单 1325 亿美元，债务超过 470 亿美元，利息支出占收入比例持续上升"
	a := mustAdd(t, s, AddInput{Topic: "CoreWeave 财报", Body: base})
	b := mustAdd(t, s, AddInput{Topic: "CoreWeave 财报", Body: base + "。"})
	c := mustAdd(t, s, AddInput{Topic: "CoreWeave 融资", Body: "CoreWeave 发行 20 亿美元高息债，用 GPU 做抵押"})
	// 两条几乎一样的财报排在最前（主题命中 + 更新），只给 2 个名额时应当是一条财报 + 融资
	r, _ := s.Search(SearchOpts{Q: "CoreWeave 财报", Limit: 2})
	ids := hitIDs(r)
	if !(contains(ids, c) && (contains(ids, a) != contains(ids, b))) || r.Folded == 0 {
		t.Fatalf("前两条应当是一条财报 + 融资（近似重复挪后）：%v folded=%d", ids, r.Folded)
	}
	r, _ = s.Search(SearchOpts{Q: "CoreWeave 财报", Limit: 2, NoFold: true})
	if r.Folded != 0 || !(contains(hitIDs(r), a) && contains(hitIDs(r), b)) {
		t.Fatalf("NoFold 时两条财报都该在：%v", hitIDs(r))
	}
}

func TestSinceUntilAndRecheckDays(t *testing.T) {
	s := newTest(t)
	old := mustAdd(t, s, AddInput{Topic: "旧闻", Body: "算力价格三月份下降", CreatedAt: "2026-03-01 10:00:00"})
	nw := mustAdd(t, s, AddInput{Topic: "新闻", Body: "算力价格十一月份上涨"})
	cut := time.Date(2026, 6, 1, 0, 0, 0, 0, time.Local).Unix()
	r, _ := s.Search(SearchOpts{Q: "算力价格", Since: cut})
	if contains(hitIDs(r), old) || !contains(hitIDs(r), nw) {
		t.Fatalf("since 过滤不对：%v", hitIDs(r))
	}
	r, _ = s.Search(SearchOpts{Q: "", Until: cut, Limit: 10})
	if !contains(hitIDs(r), old) || contains(hitIDs(r), nw) {
		t.Fatalf("until 过滤（无检索词）不对：%v", hitIDs(r))
	}
	h := mustAdd(t, s, AddInput{Kind: "judgment", Topic: "假设/某公司", Body: "某公司下季度会下调指引", Volatility: "volatile",
		Unverified: true, RecheckDays: 60})
	e, _ := s.Get(h)
	days := float64(*e.RecheckEpoch-e.CreatedEpoch) / 86400
	if days < 59.9 || days > 60.1 {
		t.Fatalf("recheck_days=60 应当 60 天后到期，实际 %.1f 天", days)
	}
}
