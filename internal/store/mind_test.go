package store

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ago(days int) string {
	return time.Now().Add(-time.Duration(days) * 24 * time.Hour).Format("2006-01-02 15:04:05")
}

func findTag(ts []MindTag, name string) (int, *MindTag) {
	for i := range ts {
		if strings.EqualFold(ts[i].Tag, name) {
			return i, &ts[i]
		}
	}
	return -1, nil
}

func TestMindAndLinks(t *testing.T) {
	s := newTest(t)
	// 60 条百天前的旧记忆，都挂「宏观」（枢纽大词），3 条挂老话题「利率」「房地产」（一对老连接）
	for i := 0; i < 60; i++ {
		tags := []string{"宏观", fmt.Sprintf("杂项%d", i)}
		if i%20 == 0 {
			tags = append(tags, "利率", "房地产")
		}
		mustAdd(t, s, AddInput{Topic: fmt.Sprintf("旧事 %d", i), Body: fmt.Sprintf("百天前的旧记录第 %d 条", i), Tags: tags, CreatedAt: ago(100)})
	}
	cited := mustAdd(t, s, AddInput{Topic: "老判断", Body: "电力是算力的硬约束", Kind: "judgment", Tags: []string{"电力"}, CreatedAt: ago(200)})
	// 这几天：「算力租赁」「电力」新连在一起；系统标签不该出现
	mustAdd(t, s, AddInput{Topic: "租赁价", Body: "H100 租赁价下跌", Tags: []string{"算力租赁", "电价", "每日沉思", "ic-event-12"}, CreatedAt: ago(1)})
	mustAdd(t, s, AddInput{Topic: "电费", Body: "数据中心电费占比上升", Tags: []string{"算力租赁", "电价"}, CreatedAt: ago(2)})
	mustAdd(t, s, AddInput{Topic: "利率一次", Body: "利率和房地产又一起提了一次", Tags: []string{"利率", "房地产"}, CreatedAt: ago(1)})
	mustAdd(t, s, AddInput{Topic: "利率两次", Body: "利率和房地产再一起提一次", Tags: []string{"利率", "房地产"}, CreatedAt: ago(1)})
	// 引用旧判断：它的标签「电力」被想起
	mustAdd(t, s, AddInput{Topic: "推论", Body: "租赁价下跌而电费上升，利润被两头挤", Kind: "judgment", Tags: []string{"推论标签"}, Cites: []int64{cited}})
	// 网页浏览不算，机器人检索算
	before, _ := s.Mind(30)
	_, e0 := findTag(before.Tags, "电力")
	if e0 == nil {
		t.Fatal("被引用的「电力」应该在心头里")
	}
	a0 := e0.Activation
	s.Search(SearchOpts{Q: "电力", Caller: "web", NoCache: true})
	m1, _ := s.Mind(30)
	_, e1 := findTag(m1.Tags, "电力")
	if e1.Activation != a0 {
		t.Fatalf("网页浏览不该算被想起：%v → %v", a0, e1.Activation)
	}
	s.Search(SearchOpts{Q: "电力", Caller: "hermes", NoCache: true})
	m, err := s.Mind(30)
	if err != nil {
		t.Fatal(err)
	}
	_, e2 := findTag(m.Tags, "电力")
	if e2.Activation <= a0 {
		t.Fatalf("机器人检索命中应该提高激活度：%v → %v", a0, e2.Activation)
	}
	for _, x := range m.Tags {
		if x.Tag == "每日沉思" || x.Tag == "ic-event-12" {
			t.Fatalf("系统标签不该出现在心头：%s", x.Tag)
		}
		if x.Tag == "宏观" {
			t.Fatal("枢纽大词应该放进背景，不排名")
		}
	}
	if _, bg := findTag(m.Background, "宏观"); bg == nil {
		t.Fatal("「宏观」应该在背景里")
	}
	iNew, rent := findTag(m.Tags, "算力租赁")
	iOld, _ := findTag(m.Tags, "杂项3")
	if rent == nil || rent.Trend != "new" {
		t.Fatalf("「算力租赁」应该是新冒出来的：%+v", rent)
	}
	if iOld >= 0 && iOld < iNew {
		t.Fatal("百天前只用过一次的标签不该排在这几天常用的前面")
	}
	if len(rent.Recent) == 0 {
		t.Fatal("心头标签应该带上最近的几条")
	}

	l, err := s.Links(7, 2, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, x := range l.Links {
		got = append(got, x.A+"⟷"+x.B)
	}
	j := strings.Join(got, " ")
	if !strings.Contains(j, "算力租赁") || !strings.Contains(j, "电价") {
		t.Fatalf("应该发现新连接 算力租赁⟷电价：%v", got)
	}
	if strings.Contains(j, "利率") {
		t.Fatalf("利率⟷房地产以前就一起出现过，不算新连接：%v", got)
	}
	// 放宽「以前出现过」的上限，老连接才出来
	l2, _ := s.Links(7, 2, 100, 10)
	found := false
	for _, x := range l2.Links {
		if (x.A == "利率" && x.B == "房地产") || (x.A == "房地产" && x.B == "利率") {
			found = x.Prior == 3
		}
	}
	if !found {
		t.Fatalf("prior=100 时应该列出老连接并给出以前的次数：%+v", l2.Links)
	}

	// 引用关系进了 meta
	r, _ := s.Search(SearchOpts{Q: "推论", Limit: 1, NoCache: true})
	if len(r.Hits) == 0 || !strings.Contains(string(r.Hits[0].Meta), fmt.Sprint(cited)) {
		t.Fatal("cites 应该存进 meta")
	}
}

func TestMindBackfillOnUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l5.db")
	s, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	mustAdd(t, s, AddInput{Topic: "a", Body: "老库里的一条", Tags: []string{"老标签"}, CreatedAt: ago(3)})
	s.db.Exec(`DROP TABLE tag_daily`) // 模拟升级前的老库
	s.Close()
	s2, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	m, _ := s2.Mind(10)
	if _, x := findTag(m.Tags, "老标签"); x == nil {
		t.Fatal("老库第一次打开应该用写入时间补出使用历史")
	}
}
