// Package server：同一套 JSON 接口同时挂在 Unix socket（本机，免令牌）和 HTTP（回环地址，经 E5 网关对外）上。
package server

import (
	"compress/gzip"
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"l5mem/internal/store"
)

//go:embed ui.html
var uiHTML []byte

type Config struct {
	Socket  string // Unix socket 路径（空 = 不开）
	HTTP    string // 例如 127.0.0.1:19110（空 = 不开）
	Token   string // HTTP 上写操作要带的令牌（X-L5-Token 或 Authorization: Bearer）
	DataDir string
	WatchAt string // 每天几点做维护巡检，例如 10:17
	Notify  string // 巡检有发现时 POST 到这个地址（可选）
	Version string
	Keep    int // 每日自动备份保留几份（0 = 不备份）
}

type Server struct {
	st  *store.Store
	cfg Config
	mux *http.ServeMux
}

func New(st *store.Store, cfg Config) *Server {
	s := &Server{st: st, cfg: cfg, mux: http.NewServeMux()}
	s.routes()
	return s
}

// 只读接口：经网关的浏览器页面也能用；其余接口在 HTTP 上要令牌。
var readOnly = map[string]bool{"search": true, "get": true, "history": true, "due": true, "relate": true, "tags": true,
	"stats": true, "metrics": true, "health": true, "aliases": true}

// 不记使用日志的维护类命令。
var maintenance = map[string]bool{"stats": true, "consolidate": true, "doctor": true, "repair": true, "reclassify": true,
	"metrics": true, "export": true, "import": true, "import-sqlite": true, "health": true, "tags": true, "watch": true}

type ctxKey int

const viaSocket ctxKey = 1

func (s *Server) routes() {
	s.mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(uiHTML)
	})
	s.mux.HandleFunc("/api/v1/", s.api)
}

func (s *Server) authorized(r *http.Request, op string) bool {
	if readOnly[op] && (r.Method == http.MethodGet || op != "aliases") {
		return true
	}
	return s.authorizedWrite(r)
}

func (s *Server) authorizedWrite(r *http.Request) bool {
	if r.Context().Value(viaSocket) != nil || s.cfg.Token == "" {
		return true
	}
	tok := r.Header.Get("X-L5-Token")
	if tok == "" {
		tok = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	return subtle.ConstantTimeCompare([]byte(tok), []byte(s.cfg.Token)) == 1
}

type apiError struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

// req 是所有接口共用的参数包：POST JSON 或 GET 查询串都行。
type req struct {
	store.AddInput
	Q           string   `json:"q"`
	Limit       int      `json:"limit"`
	Tag         string   `json:"tag"`
	History     bool     `json:"history"`
	Explain     bool     `json:"explain"`
	ID          int64    `json:"id"`
	Old         int64    `json:"old"`
	New         int64    `json:"new"`
	Reason      string   `json:"reason"`
	By          string   `json:"by"`
	Note        string   `json:"note"`
	Days        int      `json:"days"`
	Min         int      `json:"min"`   // links：最近至少一起出现几次
	Prior       int      `json:"prior"` // links：以前最多一起出现过几次（0 = 从没有过）
	SinceLast   bool     `json:"since_last"`
	SinceHours  float64  `json:"since_hours"`
	Path        string   `json:"path"`
	TopicPrefix string   `json:"topic_prefix"`
	Since       string   `json:"since"` // 2026-11-01 / 2026-11-01 08:00:00 / 7d / 24h
	Until       string   `json:"until"`
	NoFold      bool     `json:"no_fold"`
	Names       []string `json:"names"` // 同义词组
	Name        string   `json:"name"`
}

// parseWhen：「2026-11-01」「2026-11-01 08:00:00」「7d」「24h」→ unix 秒；空 = 0。
func parseWhen(v string) (int64, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, nil
	}
	if n, err := strconv.Atoi(strings.TrimRight(v, "dh")); err == nil && (strings.HasSuffix(v, "d") || strings.HasSuffix(v, "h")) {
		d := time.Duration(n) * time.Hour
		if strings.HasSuffix(v, "d") {
			d *= 24
		}
		return time.Now().Add(-d).Unix(), nil
	}
	for _, f := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(f, v, time.Local); err == nil {
			return t.Unix(), nil
		}
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.Unix(), nil
	}
	return 0, fmt.Errorf("时间看不懂：%s（写 2026-11-01、2026-11-01 08:00:00、7d 或 24h）", v)
}

func parseReq(r *http.Request, op string) (*req, error) {
	q := &req{}
	if r.Method == http.MethodPost && op != "import" {
		body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			return nil, err
		}
		if len(strings.TrimSpace(string(body))) > 0 {
			if err := json.Unmarshal(body, q); err != nil {
				return nil, fmt.Errorf("请求不是合法 JSON：%w", err)
			}
		}
	}
	v := r.URL.Query()
	str := func(k string, p *string) {
		if x := v.Get(k); x != "" {
			*p = x
		}
	}
	str("q", &q.Q)
	str("kind", &q.Kind)
	str("tag", &q.Tag)
	str("topic", &q.Topic)
	str("topic_prefix", &q.TopicPrefix)
	str("caller", &q.Caller)
	str("since", &q.Since)
	str("until", &q.Until)
	str("name", &q.Name)
	if x, err := strconv.Atoi(v.Get("limit")); err == nil {
		q.Limit = x
	}
	if x, err := strconv.ParseInt(v.Get("id"), 10, 64); err == nil {
		q.ID = x
	}
	if x, err := strconv.Atoi(v.Get("days")); err == nil {
		q.Days = x
	}
	if x, err := strconv.Atoi(v.Get("min")); err == nil {
		q.Min = x
	}
	if x, err := strconv.Atoi(v.Get("prior")); err == nil {
		q.Prior = x
	}
	if x, err := strconv.ParseFloat(v.Get("since_hours"), 64); err == nil {
		q.SinceHours = x
	}
	if x, err := strconv.Atoi(v.Get("recheck_days")); err == nil {
		q.RecheckDays = x
	}
	for k, p := range map[string]*bool{"history": &q.History, "explain": &q.Explain, "since_last": &q.SinceLast, "no_fold": &q.NoFold} {
		if x := v.Get(k); x == "1" || x == "true" {
			*p = true
		}
	}
	if c := r.Header.Get("X-L5-Caller"); c != "" && q.Caller == "" {
		q.Caller = c
	}
	if q.Caller == "" {
		q.Caller = "anonymous"
	}
	return q, nil
}

func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	op := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/"), "/")
	if !s.authorized(r, op) {
		writeJSON(w, http.StatusUnauthorized, apiError{"需要令牌：请求头 X-L5-Token"})
		return
	}
	q, err := parseReq(r, op)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{err.Error()})
		return
	}
	t0 := time.Now()
	ev := store.UsageEvent{Caller: q.Caller, Cmd: op}
	out, code, err := s.dispatch(w, r, op, q, &ev)
	if out == nil && err == nil { // 已经直接写了响应（导出）
		return
	}
	ev.Ms = float64(time.Since(t0).Microseconds()) / 1000
	if !maintenance[op] && q.Caller != "watchdog" && err == nil {
		s.st.Usage.Log(ev)
	}
	if err != nil {
		if code == 0 {
			code = http.StatusBadRequest
			if errors.Is(err, store.ErrNotFound) {
				code = http.StatusNotFound
			}
		}
		writeJSON(w, code, apiError{err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) dispatch(w http.ResponseWriter, r *http.Request, op string, q *req, ev *store.UsageEvent) (any, int, error) {
	st := s.st
	switch op {
	case "health":
		return map[string]any{"ok": true, "version": s.cfg.Version, "db": st.Path()}, 0, nil
	case "add":
		if r.Method != http.MethodPost {
			return nil, http.StatusMethodNotAllowed, errors.New("add 要用 POST")
		}
		res, err := st.Add(q.AddInput)
		if err == nil {
			ev.ID = res.ID
			ev.Q = q.Topic
		}
		return res, 0, err
	case "search":
		topic := q.TopicPrefix
		since, err := parseWhen(q.Since)
		if err != nil {
			return nil, 0, err
		}
		until, err := parseWhen(q.Until)
		if err != nil {
			return nil, 0, err
		}
		res, err := st.Search(store.SearchOpts{Q: q.Q, Limit: q.Limit, Kind: q.Kind, Tag: q.Tag, Topic: topic,
			History: q.History, Explain: q.Explain, Caller: q.Caller, Since: since, Until: until, NoFold: q.NoFold})
		if err == nil {
			ev.Q, ev.Hits, ev.Cached, ev.TagCands = q.Q, len(res.Hits), res.Cached, res.TagCands
			tt := res.TagTop1
			ev.TagTop1 = &tt
		}
		return res, 0, err
	case "aliases":
		switch r.Method {
		case http.MethodGet:
			return map[string]any{"groups": st.Aliases()}, 0, nil
		case http.MethodPost:
			if !s.authorizedWrite(r) {
				return nil, http.StatusUnauthorized, errors.New("需要令牌：请求头 X-L5-Token")
			}
			if len(q.Names) == 0 && q.Name != "" { // POST {"name": x} = 把 x 移出它的组
				if err := st.RemoveAlias(q.Name); err != nil {
					return nil, 0, err
				}
				return map[string]any{"ok": true, "removed": q.Name}, 0, nil
			}
			return wrap(st.AddAliases(q.Names))
		case http.MethodDelete:
			if !s.authorizedWrite(r) {
				return nil, http.StatusUnauthorized, errors.New("需要令牌：请求头 X-L5-Token")
			}
			if err := st.RemoveAlias(q.Name); err != nil {
				return nil, 0, err
			}
			return map[string]any{"ok": true}, 0, nil
		}
		return nil, http.StatusMethodNotAllowed, errors.New("aliases 用 GET / POST / DELETE")
	case "get":
		e, err := st.Get(q.ID)
		if e != nil {
			ev.ID, ev.Hits = q.ID, 1
		}
		return e, 0, err
	case "history":
		list, err := st.History(q.ID, q.Topic)
		ev.ID, ev.Q, ev.Hits = q.ID, q.Topic, len(list)
		return map[string]any{"entries": list}, 0, err
	case "supersede":
		ev.ID = q.Old
		if err := st.Supersede(q.Old, q.New); err != nil {
			return nil, 0, err
		}
		return map[string]any{"ok": true, "old": q.Old, "new": q.New}, 0, nil
	case "retract":
		ev.ID = q.ID
		if err := st.Retract(q.ID, q.Reason, q.Caller); err != nil {
			return nil, 0, err
		}
		return map[string]any{"ok": true, "id": q.ID}, 0, nil
	case "verify":
		ev.ID = q.ID
		by := q.By
		if by == "" {
			by = q.Caller
		}
		e, err := st.Verify(q.ID, by, q.Note)
		return e, 0, err
	case "volatility":
		if err := st.SetVolatility(q.ID, q.Volatility); err != nil {
			return nil, 0, err
		}
		e, err := st.Get(q.ID)
		return e, 0, err
	case "due":
		list, err := st.Due(q.Days, q.Limit)
		ev.Hits = len(list)
		return map[string]any{"entries": list}, 0, err
	case "relate":
		list, hubs, err := st.Relate(q.ID, q.Limit)
		ev.ID, ev.Hits = q.ID, len(list)
		return map[string]any{"entries": list, "hub_tags": hubs}, 0, err
	case "mind":
		return wrap(st.Mind(q.Limit))
	case "links":
		return wrap(st.Links(q.Days, q.Min, q.Prior, q.Limit))
	case "tags":
		list, err := st.Tags(q.Limit)
		return map[string]any{"tags": list}, 0, err
	case "stats":
		return wrap(st.Stats())
	case "consolidate":
		return wrap(st.Consolidate())
	case "doctor":
		return wrap(st.Doctor())
	case "repair":
		n, err := st.Repair()
		return map[string]any{"rebuilt": n}, 0, err
	case "reclassify":
		n, err := st.Reclassify()
		return map[string]any{"changed": n}, 0, err
	case "metrics":
		since := time.Now().Add(-24 * time.Hour)
		if q.SinceHours > 0 {
			since = time.Now().Add(-time.Duration(q.SinceHours * float64(time.Hour)))
		}
		return wrap(st.Metrics(since, q.SinceLast))
	case "watch":
		return wrap(s.Watch())
	case "export":
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		n, fp, err := st.Export(w)
		if err != nil {
			log.Printf("导出中断：%v", err)
		} else {
			log.Printf("导出 %d 条，指纹 %s", n, fp)
		}
		return nil, 0, nil
	case "import":
		if r.Method != http.MethodPost {
			return nil, http.StatusMethodNotAllowed, errors.New("import 要用 POST（正文是 JSONL）")
		}
		return wrap(st.Import(io.LimitReader(r.Body, 2<<30)))
	case "import-sqlite":
		if r.Context().Value(viaSocket) == nil {
			return nil, http.StatusForbidden, errors.New("从本机文件导入只能走 Unix socket")
		}
		return wrap(st.ImportSQLite(q.Path))
	}
	return nil, http.StatusNotFound, fmt.Errorf("没有这个接口：%s", op)
}

func wrap[T any](v T, err error) (any, int, error) {
	if err != nil {
		return nil, 0, err
	}
	return v, 0, nil
}

// WatchReport 每日巡检：维护 + 逾期升级 + 使用日报增量。没有发现就 Silent=true。
type WatchReport struct {
	At          string                   `json:"at"`
	Silent      bool                     `json:"silent"`
	Consolidate *store.ConsolidateReport `json:"consolidate"`
	Metrics     *store.MetricsReport     `json:"metrics"`
	Doctor      *store.DoctorReport      `json:"doctor"`
	Backup      string                   `json:"backup,omitempty"`     // 今天的备份文件
	BackupErr   string                   `json:"backup_err,omitempty"` // 备份失败原因
	Mind        []store.MindTag          `json:"mind,omitempty"`       // 心头前几个（只是附带，不影响是否通知）
	NewLinks    []store.Link             `json:"new_links,omitempty"`
}

// backup 每日把全库导出成 backups/l5-YYYY-MM-DD.jsonl.gz（含同义词组），只留最近 Keep 份。
func (s *Server) backup() (string, string) {
	dir := filepath.Join(s.cfg.DataDir, "backups")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err.Error()
	}
	name := filepath.Join(dir, "l5-"+time.Now().Format("2006-01-02")+".jsonl.gz")
	tmp := name + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return "", err.Error()
	}
	zw := gzip.NewWriter(f)
	n, fp, err := s.st.Export(zw)
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return "", err.Error()
	}
	if err := os.Rename(tmp, name); err != nil {
		return "", err.Error()
	}
	old, _ := filepath.Glob(filepath.Join(dir, "l5-*.jsonl.gz"))
	sort.Strings(old)
	for len(old) > s.cfg.Keep {
		os.Remove(old[0])
		old = old[1:]
	}
	return fmt.Sprintf("%s（%d 条，指纹 %s）", filepath.Base(name), n, fp), ""
}

func (s *Server) Watch() (*WatchReport, error) {
	rep := &WatchReport{At: time.Now().Format("2006-01-02 15:04:05")}
	c, err := s.st.Consolidate()
	if err != nil {
		return nil, err
	}
	rep.Consolidate = c
	rep.Metrics, _ = s.st.Metrics(time.Now().Add(-24*time.Hour), true)
	rep.Doctor, _ = s.st.Doctor()
	if s.cfg.Keep > 0 {
		rep.Backup, rep.BackupErr = s.backup()
		if rep.BackupErr != "" {
			log.Printf("每日备份失败：%s", rep.BackupErr)
		}
	}
	if m, err := s.st.Mind(8); err == nil {
		rep.Mind = m.Tags
	}
	if l, err := s.st.Links(7, 2, 0, 5); err == nil {
		rep.NewLinks = l.Links
	}
	rep.Silent = len(c.Overdue7) == 0 && (rep.Metrics == nil || (rep.Metrics.Empty && len(rep.Metrics.Verdicts) == 0)) &&
		(rep.Doctor == nil || rep.Doctor.OK) && rep.BackupErr == ""
	dir := filepath.Join(s.cfg.DataDir, "reports")
	os.MkdirAll(dir, 0o750)
	b, _ := json.MarshalIndent(rep, "", " ")
	os.WriteFile(filepath.Join(dir, time.Now().Format("2006-01-02")+".json"), b, 0o640)
	if !rep.Silent && s.cfg.Notify != "" {
		c := &http.Client{Timeout: 15 * time.Second}
		if resp, err := c.Post(s.cfg.Notify, "application/json", strings.NewReader(string(b))); err == nil {
			resp.Body.Close()
		} else {
			log.Printf("巡检通知发送失败：%v", err)
		}
	}
	return rep, nil
}

// Run 启动 socket 和 HTTP 监听，以及后台任务；ctx 取消时优雅退出。
func (s *Server) Run(ctx context.Context) error {
	errc := make(chan error, 2)
	var servers []*http.Server
	if s.cfg.Socket != "" {
		os.MkdirAll(filepath.Dir(s.cfg.Socket), 0o755)
		os.Remove(s.cfg.Socket)
		ln, err := net.Listen("unix", s.cfg.Socket)
		if err != nil {
			return fmt.Errorf("监听 socket 失败：%w", err)
		}
		os.Chmod(s.cfg.Socket, 0o660)
		hs := &http.Server{Handler: s.mux, ConnContext: func(c context.Context, _ net.Conn) context.Context {
			return context.WithValue(c, viaSocket, true)
		}, ReadHeaderTimeout: 10 * time.Second}
		servers = append(servers, hs)
		go func() { errc <- hs.Serve(ln) }()
		log.Printf("Unix socket：%s", s.cfg.Socket)
	}
	if s.cfg.HTTP != "" {
		ln, err := net.Listen("tcp", s.cfg.HTTP)
		if err != nil {
			return fmt.Errorf("监听 HTTP 失败：%w", err)
		}
		hs := &http.Server{Handler: s.mux, ReadHeaderTimeout: 10 * time.Second}
		servers = append(servers, hs)
		go func() { errc <- hs.Serve(ln) }()
		log.Printf("HTTP：%s（写操作%s）", s.cfg.HTTP, map[bool]string{true: "需要令牌", false: "不需要令牌"}[s.cfg.Token != ""])
	}
	go s.background(ctx)
	select {
	case <-ctx.Done():
	case err := <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, hs := range servers {
		hs.Shutdown(sctx)
	}
	if s.cfg.Socket != "" {
		os.Remove(s.cfg.Socket)
	}
	return nil
}

func (s *Server) background(ctx context.Context) {
	flush := time.NewTicker(5 * time.Minute)
	defer flush.Stop()
	next := nextAt(s.cfg.WatchAt)
	timer := time.NewTimer(time.Until(next))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-flush.C:
			s.st.FlushHits()
		case <-timer.C:
			if rep, err := s.Watch(); err != nil {
				log.Printf("每日巡检失败：%v", err)
			} else if !rep.Silent {
				log.Printf("每日巡检：%d 条逾期 ≥7 天未核对，体检 %v", len(rep.Consolidate.Overdue7), rep.Doctor != nil && rep.Doctor.OK)
			}
			timer.Reset(time.Until(nextAt(s.cfg.WatchAt)))
		}
	}
}

func nextAt(hhmm string) time.Time {
	h, m := 10, 17
	if p := strings.Split(hhmm, ":"); len(p) == 2 {
		if x, err := strconv.Atoi(p[0]); err == nil {
			h = x
		}
		if x, err := strconv.Atoi(p[1]); err == nil {
			m = x
		}
	}
	n := time.Now()
	t := time.Date(n.Year(), n.Month(), n.Day(), h, m, 0, 0, n.Location())
	if !t.After(n) {
		t = t.Add(24 * time.Hour)
	}
	return t
}
