// l5：第五层记忆。一个程序两种用法：
//
//	l5 serve ...      起服务（Unix socket + HTTP + 网页）
//	l5 <命令> ...     命令行客户端（装成 memo 也行：memo search 订阅）
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"l5mem/internal/client"
	"l5mem/internal/server"
	"l5mem/internal/store"
)

var version = "dev"

const usage = `L5 记忆库 %s

起服务：
  l5 serve [--db 路径] [--data 目录] [--socket 路径] [--http 127.0.0.1:19110] [--watch-at 10:17]

写：
  l5 add <主题> <正文> [-k 类型] [-t 标签1,标签2] [-s 出处] [--vol stable|slow|volatile] [--unverified] [--supersedes 旧id,..] [--meta JSON] [--recheck-days N]
       类型：原则 principle / 决策 decision / 判断 judgment / 教训 lesson / 事实 fact（默认）/ 备忘 memo
  l5 supersede <旧id> <新id>          新值取代旧值（旧值不删，--history 能回看）
  l5 retract <id> [--reason 原因]      撤回一条错的（没有新值时用）
  l5 verify <id> [--note 怎么核的]     核对过，重新计时
  l5 vol <id> stable|slow|volatile    手动改生命周期

查：
  l5 search <词…> [-n 8] [-k 类型] [--tag 标签] [--topic 主题前缀] [--since 7d|2026-11-01] [--until 日期] [--history] [--explain] [--no-fold]
  l5 get <id>      l5 history <id|主题>      l5 relate <id> [-n 8]
  l5 due [--days 0]       到期该核对的      l5 tags [-n 50]      l5 stats

同义词（检索时一起查）：
  l5 alias add 英伟达 NVIDIA NVDA      l5 alias ls      l5 alias rm NVDA      l5 alias load deploy/aliases.txt

维护：
  l5 consolidate | doctor | repair | reclassify | watch
  l5 metrics [--since-last] [--hours 24]
  l5 export > 备份.jsonl      l5 import 备份.jsonl      l5 import-sqlite 旧库.db（只能在服务器本机）

通用：--json 输出原始 JSON。连接：默认本机 socket %s；远程用 L5_URL=http://192.168.1.158:9110 和 L5_TOKEN；
      L5_CALLER=机器人名 用来在使用日报里区分是谁在用。
`

func main() {
	log.SetFlags(log.LstdFlags)
	args := os.Args[1:]
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Printf(usage, version, client.DefaultSocket)
		return
	}
	if args[0] == "version" || args[0] == "--version" {
		fmt.Println(version)
		return
	}
	if args[0] == "serve" {
		serve(args[1:])
		return
	}
	if err := runClient(args); err != nil {
		fmt.Fprintln(os.Stderr, "错误："+err.Error())
		os.Exit(1)
	}
}

// ---------------- 参数解析：位置参数 + 任意位置的 --flag 值 ----------------

type argv struct {
	pos   []string
	flags map[string]string
}

var boolFlags = map[string]bool{"json": true, "history": true, "explain": true, "unverified": true, "since-last": true, "no-fold": true}
var short = map[string]string{"k": "kind", "t": "tags", "s": "source", "n": "limit"}

func parse(args []string) argv {
	a := argv{flags: map[string]string{}}
	for i := 0; i < len(args); i++ {
		x := args[i]
		if strings.HasPrefix(x, "-") && len(x) > 1 && !isNumber(x) {
			k := strings.TrimLeft(x, "-")
			v := ""
			if eq := strings.Index(k, "="); eq >= 0 {
				k, v = k[:eq], k[eq+1:]
			} else if !boolFlags[k] && i+1 < len(args) {
				v = args[i+1]
				i++
			} else {
				v = "1"
			}
			if l, ok := short[k]; ok {
				k = l
			}
			a.flags[k] = v
			continue
		}
		a.pos = append(a.pos, x)
	}
	return a
}

func isNumber(s string) bool { _, err := strconv.ParseFloat(s, 64); return err == nil }

func (a argv) get(k, d string) string {
	if v, ok := a.flags[k]; ok {
		return v
	}
	return d
}

func (a argv) has(k string) bool { _, ok := a.flags[k]; return ok }

func (a argv) id(i int) (int64, error) {
	if len(a.pos) <= i {
		return 0, fmt.Errorf("缺少 id")
	}
	return strconv.ParseInt(strings.TrimPrefix(a.pos[i], "#"), 10, 64)
}

func ids(s string) []int64 {
	var out []int64
	for _, x := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '，' }) {
		if v, err := strconv.ParseInt(strings.TrimPrefix(x, "#"), 10, 64); err == nil {
			out = append(out, v)
		}
	}
	return out
}

// ---------------- 服务 ----------------

func serve(args []string) {
	a := parse(args)
	dataDir := a.get("data", envOr("L5_DATA", "/srv/e5-data/l5mem"))
	dbPath := a.get("db", envOr("L5_DB", filepath.Join(dataDir, "l5.db")))
	os.MkdirAll(filepath.Dir(dbPath), 0o750)
	st, err := store.Open(dbPath, filepath.Join(dataDir, "logs"))
	if err != nil {
		log.Fatalf("打开记忆库失败：%v", err)
	}
	defer st.Close()
	cfg := server.Config{
		Socket: a.get("socket", envOr("L5_SOCKET", client.DefaultSocket)), HTTP: a.get("http", envOr("L5_HTTP", "127.0.0.1:19110")),
		Token: envOr("L5_TOKEN", ""), DataDir: dataDir, WatchAt: a.get("watch-at", envOr("L5_WATCH_AT", "10:17")),
		Notify: envOr("L5_NOTIFY_URL", ""), Version: version, Keep: envInt("L5_BACKUP_KEEP", 14),
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	stats, _ := st.Stats()
	log.Printf("L5 %s 启动：%s（%d 条，其中有效 %d）", version, dbPath, stats.Entries, stats.Active)
	if err := server.New(st, cfg).Run(ctx); err != nil {
		log.Fatal(err)
	}
	log.Printf("已停止")
}

func envInt(k string, d int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return d
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// ---------------- 客户端命令 ----------------

func runClient(args []string) error {
	c := client.New()
	cmd := args[0]
	a := parse(args[1:])
	asJSON := a.has("json")
	q := url.Values{}
	var out any = &json.RawMessage{}
	var body any
	op := cmd
	switch cmd {
	case "add":
		if len(a.pos) < 2 {
			return fmt.Errorf("用法：l5 add <主题> <正文> [-k 类型] [-t 标签]")
		}
		in := store.AddInput{Topic: a.pos[0], Body: strings.Join(a.pos[1:], " "), Kind: a.get("kind", "fact"),
			Tags: []string{a.get("tags", "")}, Source: a.get("source", ""), Volatility: a.get("vol", a.get("volatility", "")),
			Unverified: a.has("unverified"), Supersedes: ids(a.get("supersedes", "")), Caller: c.Caller}
		if m := a.get("meta", ""); m != "" {
			in.Meta = json.RawMessage(m)
		}
		if d, err := strconv.Atoi(a.get("recheck-days", "0")); err == nil {
			in.RecheckDays = d
		}
		body = in
	case "search", "s":
		op = "search"
		q.Set("q", strings.Join(a.pos, " "))
		q.Set("limit", a.get("limit", "8"))
		for _, k := range []string{"kind", "tag"} {
			if v := a.get(k, ""); v != "" {
				q.Set(k, v)
			}
		}
		if v := a.get("topic", ""); v != "" {
			q.Set("topic_prefix", v)
		}
		if a.has("history") {
			q.Set("history", "1")
		}
		if a.has("explain") {
			q.Set("explain", "1")
		}
		if a.has("no-fold") {
			q.Set("no_fold", "1")
		}
		for _, k := range []string{"since", "until"} {
			if v := a.get(k, ""); v != "" {
				q.Set(k, v)
			}
		}
	case "alias", "aliases":
		op = "aliases"
		sub := ""
		if len(a.pos) > 0 {
			sub = a.pos[0]
		}
		switch sub {
		case "", "ls", "list":
		case "add":
			if len(a.pos) < 3 {
				return fmt.Errorf("用法：l5 alias add 写法1 写法2 [写法3…]")
			}
			body = map[string]any{"names": a.pos[1:]}
		case "load":
			if len(a.pos) < 2 {
				return fmt.Errorf("用法：l5 alias load 文件（每行一组，写法之间用空格或 / 分开，# 开头是注释）")
			}
			b, err := os.ReadFile(a.pos[1])
			if err != nil {
				return err
			}
			n := 0
			for _, ln := range strings.Split(string(b), "\n") {
				ln = strings.TrimSpace(ln)
				if ln == "" || strings.HasPrefix(ln, "#") {
					continue
				}
				names := strings.FieldsFunc(ln, func(r rune) bool { return r == '/' || r == '|' || r == '｜' || r == '\t' })
				if len(names) < 2 {
					names = strings.Fields(ln)
				}
				for i := range names {
					names[i] = strings.TrimSpace(names[i])
				}
				if err := c.Call("aliases", nil, map[string]any{"names": names}, nil); err != nil {
					fmt.Fprintf(os.Stderr, "跳过「%s」：%v\n", ln, err)
					continue
				}
				n++
			}
			fmt.Printf("载入 %d 组同义词\n", n)
			return nil
		case "rm", "remove", "del":
			if len(a.pos) < 2 {
				return fmt.Errorf("用法：l5 alias rm 写法")
			}
			body = map[string]any{"name": a.pos[1]}
		default:
			return fmt.Errorf("用法：l5 alias add|ls|rm")
		}
	case "get":
		id, err := a.id(0)
		if err != nil {
			return err
		}
		q.Set("id", fmt.Sprint(id))
	case "history":
		if len(a.pos) == 0 {
			return fmt.Errorf("用法：l5 history <id|主题>")
		}
		if id, err := strconv.ParseInt(strings.TrimPrefix(a.pos[0], "#"), 10, 64); err == nil {
			q.Set("id", fmt.Sprint(id))
		} else {
			q.Set("topic", strings.Join(a.pos, " "))
		}
	case "relate":
		id, err := a.id(0)
		if err != nil {
			return err
		}
		q.Set("id", fmt.Sprint(id))
		q.Set("limit", a.get("limit", "8"))
	case "supersede":
		o, err1 := a.id(0)
		n, err2 := a.id(1)
		if err1 != nil || err2 != nil {
			return fmt.Errorf("用法：l5 supersede <旧id> <新id>")
		}
		body = map[string]any{"old": o, "new": n}
	case "retract":
		id, err := a.id(0)
		if err != nil {
			return err
		}
		body = map[string]any{"id": id, "reason": a.get("reason", "")}
	case "verify":
		id, err := a.id(0)
		if err != nil {
			return err
		}
		body = map[string]any{"id": id, "note": a.get("note", ""), "by": a.get("by", "")}
	case "vol", "volatility":
		op = "volatility"
		id, err := a.id(0)
		if err != nil || len(a.pos) < 2 {
			return fmt.Errorf("用法：l5 vol <id> stable|slow|volatile")
		}
		body = map[string]any{"id": id, "volatility": a.pos[1]}
	case "due":
		q.Set("days", a.get("days", "0"))
		q.Set("limit", a.get("limit", "100"))
	case "tags":
		q.Set("limit", a.get("limit", "50"))
	case "stats", "doctor":
	case "consolidate", "repair", "reclassify", "watch":
		body = map[string]any{}
	case "metrics":
		if a.has("since-last") {
			q.Set("since_last", "1")
		}
		q.Set("since_hours", a.get("hours", "24"))
	case "export":
		return c.Call("export", nil, nil, os.Stdout)
	case "import":
		if len(a.pos) == 0 {
			return fmt.Errorf("用法：l5 import 文件.jsonl")
		}
		f, err := os.Open(a.pos[0])
		if err != nil {
			return err
		}
		defer f.Close()
		body = f
	case "import-sqlite":
		if len(a.pos) == 0 {
			return fmt.Errorf("用法：l5 import-sqlite 旧库.db")
		}
		p, _ := filepath.Abs(a.pos[0])
		body = map[string]any{"path": p}
	default:
		return fmt.Errorf("不认识的命令 %q，l5 help 看用法", cmd)
	}
	t0 := time.Now()
	if err := c.Call(op, q, body, out); err != nil {
		return err
	}
	raw := *out.(*json.RawMessage)
	if asJSON {
		os.Stdout.Write(raw)
		fmt.Println()
		return nil
	}
	return pretty(op, raw, time.Since(t0))
}

// ---------------- 人读输出 ----------------

var kindLabel = store.KindLabel
var volLabel = map[string]string{"stable": "长期", "slow": "缓变", "volatile": "快变"}

func line(e *store.Entry) string {
	flags := []string{volLabel[e.Volatility]}
	if !e.Active {
		if e.SupersededBy != nil {
			flags = append(flags, fmt.Sprintf("已被 #%d 取代", *e.SupersededBy))
		} else {
			flags = append(flags, "已撤回")
		}
	}
	if e.Stale {
		flags = append(flags, fmt.Sprintf("该核对了（逾期 %d 天）", e.Overdue))
	}
	if !e.Verified {
		flags = append(flags, "未核实")
	}
	tags := ""
	if len(e.Tags) > 0 {
		tags = "  #" + strings.Join(e.Tags, " #")
	}
	src := ""
	if e.Source != "" {
		src = "\n      出处：" + e.Source
	}
	return fmt.Sprintf("#%d [%s] %s（%s，%s）%s\n      %s%s", e.ID, kindLabel[e.Kind], e.Topic, strings.Join(flags, "，"),
		e.CreatedAt[:10], tags, strings.ReplaceAll(e.Body, "\n", "\n      "), src)
}

func pretty(op string, raw json.RawMessage, d time.Duration) error {
	switch op {
	case "add":
		var r store.AddResult
		json.Unmarshal(raw, &r)
		if r.Existed {
			fmt.Printf("已存在 #%d（内容相同，没有重复写入）", r.ID)
		} else {
			fmt.Printf("已写入 #%d（%s）", r.ID, volLabel[r.Volatility])
		}
		if len(r.Superseded) > 0 {
			fmt.Printf("，取代了 %v", r.Superseded)
		}
		fmt.Println()
	case "aliases":
		var r struct {
			Groups  []store.AliasGroup `json:"groups"`
			Canon   string             `json:"canon"`
			Names   []string           `json:"names"`
			Removed string             `json:"removed"`
		}
		json.Unmarshal(raw, &r)
		switch {
		case r.Removed != "":
			fmt.Printf("已移出：%s\n", r.Removed)
		case r.Canon != "":
			fmt.Printf("同义词组「%s」：%s\n", r.Canon, strings.Join(r.Names, " / "))
		case len(r.Groups) == 0:
			fmt.Println("还没有同义词组。l5 alias add 英伟达 NVIDIA Nvidia")
		default:
			for _, g := range r.Groups {
				fmt.Printf("%s：%s\n", g.Canon, strings.Join(g.Names, " / "))
			}
		}
	case "search":
		var r store.SearchResult
		json.Unmarshal(raw, &r)
		if len(r.Hits) == 0 {
			fmt.Println("没有找到。")
		}
		if len(r.Expanded) > 0 {
			fmt.Printf("（同义写法一起查了：%s）\n", strings.Join(r.Expanded, "、"))
		}
		for _, h := range r.Hits {
			fmt.Printf("%s\n      得分 %.3f", line(h.Entry), h.Score)
			if h.Explain != nil {
				var rs []string
				for k, v := range h.Explain.Routes {
					rs = append(rs, fmt.Sprintf("%s#%d", k, v))
				}
				sort.Strings(rs)
				fmt.Printf("  = 相关 %.3f · 邻近 %.3f · 新鲜 %.3f · 重要 %.3f · 标签 +%.2f  [%s]", h.Explain.Lex, h.Explain.Prox,
					h.Explain.Rec, h.Explain.Imp, h.Explain.TagBoost, strings.Join(rs, " "))
			}
			fmt.Println()
		}
		extra := ""
		if r.Cached {
			extra = "，缓存"
		}
		if len(r.HubTags) > 0 {
			extra += "，放弃了太泛的标签：" + strings.Join(r.HubTags, "、")
		}
		fmt.Printf("— %d 条，服务端 %.2f ms%s\n", len(r.Hits), r.Ms, extra)
	case "get", "verify", "volatility":
		var e store.Entry
		json.Unmarshal(raw, &e)
		fmt.Println(line(&e))
		if e.LastVerifiedAt != "" {
			fmt.Printf("      上次核对：%s %s %s\n", e.LastVerifiedAt, e.LastVerifiedBy, e.LastVerifiedNote)
		}
	case "history", "due":
		var r struct {
			Entries []*store.Entry `json:"entries"`
		}
		json.Unmarshal(raw, &r)
		if len(r.Entries) == 0 {
			fmt.Println(map[string]string{"history": "没有记录。", "due": "没有到期要核对的。"}[op])
		}
		for _, e := range r.Entries {
			fmt.Println(line(e))
		}
	case "relate":
		var r struct {
			Entries []struct {
				*store.Entry
				Score  float64  `json:"score"`
				Shared []string `json:"shared"`
				Hop    int      `json:"hop"`
				Via    int64    `json:"via"`
			} `json:"entries"`
			Hubs []string `json:"hub_tags"`
		}
		json.Unmarshal(raw, &r)
		if len(r.Entries) == 0 {
			fmt.Println("没有相关记忆（没有共享的稀有标签）。")
		}
		for _, e := range r.Entries {
			how := "直接"
			if e.Hop == 2 {
				how = fmt.Sprintf("经由 #%d", e.Via)
			}
			fmt.Printf("%s\n      关联 %.2f（%s，共享：%s）\n", line(e.Entry), e.Score, how, strings.Join(e.Shared, "、"))
		}
	case "tags":
		var r struct {
			Tags []store.TagCount `json:"tags"`
		}
		json.Unmarshal(raw, &r)
		for _, t := range r.Tags {
			fmt.Printf("%6d  %s\n", t.Count, t.Tag)
		}
	default:
		var v any
		json.Unmarshal(raw, &v)
		b, _ := json.MarshalIndent(v, "", "  ")
		fmt.Println(string(b))
	}
	return nil
}
