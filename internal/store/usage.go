package store

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// UsageEvent 一次调用的使用记录（usage.jsonl 一行）。维护命令和看门狗自己的调用不记。
type UsageEvent struct {
	TS       string  `json:"ts"`
	Caller   string  `json:"caller"`
	Cmd      string  `json:"cmd"`
	Q        string  `json:"q,omitempty"`
	Ms       float64 `json:"ms"`
	Hits     int     `json:"hits"`
	TagTop1  *bool   `json:"tag_top1,omitempty"`
	TagCands int     `json:"tag_cands,omitempty"`
	Cached   bool    `json:"cached,omitempty"`
	ID       int64   `json:"id,omitempty"`
}

type UsageLog struct {
	dir string
	ch  chan UsageEvent
	wg  sync.WaitGroup
}

func NewUsageLog(dir string) *UsageLog {
	os.MkdirAll(dir, 0o750)
	u := &UsageLog{dir: dir, ch: make(chan UsageEvent, 4096)}
	u.wg.Add(1)
	go u.loop()
	return u
}

func (u *UsageLog) Path() string { return filepath.Join(u.dir, "usage.jsonl") }

// Log 异步记录，不阻塞请求；队列满了直接丢（日志不能拖慢检索）。
func (u *UsageLog) Log(e UsageEvent) {
	if u == nil {
		return
	}
	if e.TS == "" {
		e.TS = time.Now().Format(time.RFC3339Nano)
	}
	select {
	case u.ch <- e:
	default:
	}
}

func (u *UsageLog) loop() {
	defer u.wg.Done()
	f, err := os.OpenFile(u.Path(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		for range u.ch {
		}
		return
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 64<<10)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for {
		select {
		case e, ok := <-u.ch:
			if !ok {
				w.Flush()
				return
			}
			enc.Encode(e)
		case <-tick.C:
			w.Flush()
		}
	}
}

func (u *UsageLog) Close() {
	close(u.ch)
	u.wg.Wait()
}
