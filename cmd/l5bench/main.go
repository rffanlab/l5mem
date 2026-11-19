// l5bench：可复现的检索基准（固定随机种子）。对照 Python v2.6 的 BENCH 口径：1k/10k/50k 条、冷缓存 p50、2 字高频/稀有词、
// recall@5、4 进程（这里是 4 个并发写读者）混合读写。
//
//	go run ./cmd/l5bench -sizes 1000,10000,50000
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"l5mem/internal/store"
)

var words = strings.Fields(`作息 发布 配图 订阅 价格 额度 模型 算力 芯片 融资 估值 公众号 视频 剪辑 字幕 封面 选题 素材 标题 摘要
数据库 部署 服务器 代理 节点 网关 日志 性能 延迟 缓存 索引 检索 标签 联想 核对 过期 取代 历史 决策 教训 原则 偏好 口味 风格
福叔 研究员 机器人 团队 周报 日报 研判 事件 来源 证据 推断 可信度 关注 方向 竞争 开源 闭源 发布会 财报 营收 毛利 现金流 监管
审批 电力 土地 社区 数据中心 存储 显卡 推理 训练 微调 评测 基准 榜单 排名 版本 截止 套餐 会员 账单 优惠 汇率 利率 股价 市值`)

var rare = []string{"赛博鸡蛋", "量子芝麻", "墨境", "汽水音乐", "灵枢", "青铜葵花", "太初", "玄牝"}

func sentence(r *rand.Rand, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(words[r.Intn(len(words))])
		if r.Intn(5) == 0 {
			b.WriteString("，")
		}
	}
	b.WriteString("。")
	return b.String()
}

func pctl(xs []float64, p float64) float64 {
	sort.Float64s(xs)
	return xs[int(p*float64(len(xs)-1))]
}

func main() {
	sizesF := flag.String("sizes", "1000,10000,50000", "库大小")
	dir := flag.String("dir", os.TempDir(), "临时目录")
	flag.Parse()
	fmt.Println("| 规模 | 写入 条/秒 | 2字高频 p50 | 2字稀有 p50 | 4字短语 p50 | 主题前缀 p50 | 标签点查 p50 | 缓存命中 p50 | recall@5 | 并发 4 读写 |")
	fmt.Println("|---|---|---|---|---|---|---|---|---|---|")
	for _, sz := range strings.Split(*sizesF, ",") {
		n, _ := strconv.Atoi(sz)
		run(n, *dir)
	}
}

func run(n int, dir string) {
	r := rand.New(rand.NewSource(42))
	d, _ := os.MkdirTemp(dir, "l5bench")
	defer os.RemoveAll(d)
	st, err := store.Open(filepath.Join(d, "b.db"), "")
	if err != nil {
		panic(err)
	}
	defer st.Close()
	// 批量导入（JSONL 路径，和真实迁移一样）
	pr, pw := ioPipe()
	type probe struct {
		id    int64
		topic string
	}
	var probes []probe
	go func() {
		for i := 1; i <= n; i++ {
			topic := words[r.Intn(len(words))] + words[r.Intn(len(words))]
			body := sentence(r, 12+r.Intn(30))
			tags := []string{words[r.Intn(len(words))], "x"}
			if i%97 == 0 {
				body += rare[r.Intn(len(rare))]
				tags = append(tags, "稀有")
			}
			topic = fmt.Sprintf("%s-%d", topic, i)
			if i%(n/20+1) == 0 {
				probes = append(probes, probe{int64(i), topic})
			}
			fmt.Fprintf(pw, `{"id":%d,"kind":"fact","topic":%q,"body":%q,"tags":%q,"created_epoch":%d}`+"\n", i, topic, body,
				strings.Join(tags, ","), time.Now().Add(-time.Duration(r.Intn(400*24))*time.Hour).Unix())
		}
		pw.Close()
	}()
	t0 := time.Now()
	rep, err := st.Import(pr)
	if err != nil {
		panic(err)
	}
	ins := float64(rep.Inserted) / (rep.InsertMs / 1000)
	_ = t0
	if !rep.Verified {
		fmt.Println("导入自检没通过：", rep.Problems)
	}
	_ = ins
	meas := func(qs []string, opts func(q string) store.SearchOpts) float64 {
		var lat []float64
		for rep := 0; rep < 3; rep++ {
			for _, q := range qs {
				o := opts(q)
				o.NoCache = true
				t := time.Now()
				st.Search(o)
				lat = append(lat, float64(time.Since(t).Microseconds())/1000)
			}
		}
		return pctl(lat, 0.5)
	}
	plain := func(q string) store.SearchOpts {
		return store.SearchOpts{Q: q, Limit: 8, NoFold: os.Getenv("NOFOLD") != ""}
	}
	hi := meas([]string{"发布", "作息", "配图", "订阅", "模型", "算力", "数据", "推理"}, plain)
	lo := meas([]string{"芝麻", "鸡蛋", "墨境", "灵枢", "葵花", "玄牝"}, plain)
	phrase := meas([]string{"数据中心", "汽水音乐", "赛博鸡蛋", "青铜葵花"}, plain)
	var topics []string
	for _, p := range probes {
		topics = append(topics, strings.SplitN(p.topic, "-", 2)[0])
	}
	topicP := meas(topics, plain)
	tagP := meas([]string{"稀有"}, plain)
	// 缓存命中
	st.Search(store.SearchOpts{Q: "发布", Limit: 8})
	var cl []float64
	for i := 0; i < 200; i++ {
		t := time.Now()
		st.Search(store.SearchOpts{Q: "发布", Limit: 8})
		cl = append(cl, float64(time.Since(t).Microseconds())/1000)
	}
	// recall@5：用每条探针自己的主题全名去找自己
	hit := 0
	for _, p := range probes {
		res, _ := st.Search(store.SearchOpts{Q: p.topic, Limit: 5, NoCache: true})
		for _, h := range res.Hits {
			if h.ID == p.id {
				hit++
				break
			}
		}
	}
	// 并发：4 个写读者，各 100 次（写 1 次 + 读 4 次）
	var wg sync.WaitGroup
	var errs int
	var mu sync.Mutex
	tc := time.Now()
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rr := rand.New(rand.NewSource(int64(w)))
			for i := 0; i < 100; i++ {
				if _, err := st.Add(store.AddInput{Topic: "并发", Body: fmt.Sprintf("worker %d %d %s", w, i, sentence(rr, 8))}); err != nil {
					mu.Lock()
					errs++
					mu.Unlock()
				}
				for k := 0; k < 4; k++ {
					st.Search(store.SearchOpts{Q: words[rr.Intn(len(words))], Limit: 8, NoCache: true})
				}
			}
		}(w)
	}
	wg.Wait()
	fmt.Printf("| %d | %.0f | %.2f ms | %.2f ms | %.2f ms | %.2f ms | %.2f ms | %.3f ms | %.2f | %d 错误 / %.1f s |\n",
		n, ins, hi, lo, phrase, topicP, tagP, pctl(cl, 0.5), float64(hit)/float64(len(probes)), errs, time.Since(tc).Seconds())
}

func ioPipe() (*pipeReader, *pipeWriter) { r, w := newPipe(); return r, w }
