package store

import (
	"bytes"
	"path/filepath"
	"sync"
	"testing"
)

func newTest(t *testing.T) *Store {
	t.Helper()
	d := t.TempDir()
	s, err := Open(filepath.Join(d, "l5.db"), d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustAdd(t *testing.T, s *Store, in AddInput) int64 {
	t.Helper()
	r, err := s.Add(in)
	if err != nil {
		t.Fatal(err)
	}
	return r.ID
}

func TestAddSearchSupersede(t *testing.T) {
	s := newTest(t)
	a := mustAdd(t, s, AddInput{Kind: "fact", Topic: "福叔订阅", Body: "福叔付了 Grok Bot 订阅，每月 30 美元", Tags: []string{"订阅,Grok"}})
	mustAdd(t, s, AddInput{Kind: "memo", Topic: "作息", Body: "福叔一般晚上十一点睡，早上七点起", Tags: []string{"作息"}})
	mustAdd(t, s, AddInput{Kind: "principle", Topic: "配图原则", Body: "公众号配图必须注明图源，不用来路不明的图", Tags: []string{"配图", "公众号"}})

	// 幂等：同内容再写返回已存在
	r, _ := s.Add(AddInput{Kind: "事实", Topic: "福叔订阅", Body: "福叔付了 Grok Bot 订阅，每月 30 美元"})
	if !r.Existed || r.ID != a {
		t.Fatalf("应当返回已存在 #%d，得到 %+v", a, r)
	}
	if r.Retagged {
		t.Fatal("没带标签的重复写入不该改标签")
	}
	r2, _ := s.Add(AddInput{Kind: "fact", Topic: "福叔订阅", Body: "福叔付了 Grok Bot 订阅，每月 30 美元", Tags: []string{"订阅", "算力、芯片与成本"}})
	if !r2.Retagged {
		t.Fatal("同内容换标签应当更新标签")
	}
	if e, _ := s.Get(a); len(e.Tags) != 2 || e.Tags[1] != "算力、芯片与成本" {
		t.Fatalf("顿号不该拆标签：%v", e.Tags)
	}
	if res, _ := s.Search(SearchOpts{Q: "算力、芯片与成本"}); len(res.Hits) == 0 || !res.TagTop1 {
		t.Fatal("新标签应当能点查到")
	}
	if r.Volatility != "volatile" {
		t.Fatalf("订阅应当是 volatile，得到 %s", r.Volatility)
	}
	for _, q := range []string{"作息", "配图", "订阅", "Grok", "grok bot", "公众号配图", "图"} {
		res, err := s.Search(SearchOpts{Q: q, Limit: 5, Explain: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Hits) == 0 {
			t.Fatalf("%q 检索不到（路：%v）", q, res.Routes)
		}
	}
	// 标签点查顶到第一
	res, _ := s.Search(SearchOpts{Q: "作息", Limit: 3})
	if !res.TagTop1 {
		t.Fatalf("作息应由标签路抬到第一：%+v", res.Hits[0].Explain)
	}
	// 取代：新值上来，旧值默认不出现，history 能看到
	b := mustAdd(t, s, AddInput{Kind: "fact", Topic: "福叔订阅", Body: "Grok Bot 已并入 The X Subscription，Premium 30 美元含 Cursor Pro", Supersedes: []int64{a}})
	res, _ = s.Search(SearchOpts{Q: "订阅", Limit: 10})
	for _, h := range res.Hits {
		if h.ID == a {
			t.Fatal("被取代的旧值不该出现在默认检索里")
		}
	}
	res, _ = s.Search(SearchOpts{Q: "订阅", Limit: 10, History: true})
	found := false
	for _, h := range res.Hits {
		found = found || h.ID == a
	}
	if !found {
		t.Fatal("history 检索应当能看到旧值")
	}
	hist, _ := s.History(b, "")
	if len(hist) != 2 || hist[0].ID != a || hist[1].ID != b {
		t.Fatalf("取代链不对：%v", hist)
	}
	// 体检、重建
	dr, _ := s.Doctor()
	if !dr.OK {
		t.Fatalf("体检不通过：%v", dr.Problems)
	}
	if n, err := s.Repair(); err != nil || n != 4 {
		t.Fatalf("重建：%d %v", n, err)
	}
	if res, _ := s.Search(SearchOpts{Q: "作息"}); len(res.Hits) == 0 {
		t.Fatal("重建后检索不到")
	}
}

func TestDueVerifyRelate(t *testing.T) {
	s := newTest(t)
	id := mustAdd(t, s, AddInput{Topic: "价格", Body: "某模型 API 价格每百万 token 2 美元", Unverified: true, Tags: []string{"价格", "API"}})
	e, _ := s.Get(id)
	if e.Volatility != "volatile" || e.RecheckEpoch == nil {
		t.Fatalf("应当 volatile 且有核对期：%+v", e)
	}
	due, _ := s.Due(8, 10)
	if len(due) != 1 {
		t.Fatalf("未核实的 7 天内应到期：%d", len(due))
	}
	if _, err := s.Verify(id, "研究员", "官网定价页核对"); err != nil {
		t.Fatal(err)
	}
	due, _ = s.Due(8, 10)
	if len(due) != 0 {
		t.Fatal("核对后不应再到期")
	}
	x := mustAdd(t, s, AddInput{Topic: "赛博鸡蛋 A", Body: "事件 A", Tags: []string{"赛博鸡蛋", "x"}})
	y := mustAdd(t, s, AddInput{Topic: "赛博鸡蛋 B", Body: "事件 B", Tags: []string{"赛博鸡蛋", "x"}})
	for i := 0; i < 30; i++ {
		mustAdd(t, s, AddInput{Topic: "其他", Body: "噪音 " + string(rune('a'+i%26)) + string(rune('0'+i/26)), Tags: []string{"x"}})
	}
	rel, _, err := s.Relate(x, 5)
	if err != nil || len(rel) == 0 || rel[0].ID != y {
		t.Fatalf("共享稀有标签的应排第一：%v %v", rel, err)
	}
}

func TestConcurrentWritesAndExportImport(t *testing.T) {
	s := newTest(t)
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				// 一半是重复内容：并发写同一条应当返回已存在而不是报错
				body := "共享事实 " + string(rune('A'+i%25))
				if i%2 == 0 {
					body = "worker" + string(rune('a'+w)) + " 的事实 " + string(rune('A'+i%25)) + string(rune('0'+i/25))
				}
				if _, err := s.Add(AddInput{Topic: "并发", Body: body}); err != nil {
					t.Error(err)
				}
				s.Search(SearchOpts{Q: "事实"})
			}
		}(w)
	}
	wg.Wait()
	var buf bytes.Buffer
	n, fp, err := s.Export(&buf)
	if err != nil {
		t.Fatal(err)
	}
	s2 := newTest(t)
	rep, err := s2.Import(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Inserted != n || rep.Fingerprint != fp || !rep.Verified {
		t.Fatalf("导入不无损：%d/%d fp %s vs %s problems %v", rep.Inserted, n, rep.Fingerprint, fp, rep.Problems)
	}
}
