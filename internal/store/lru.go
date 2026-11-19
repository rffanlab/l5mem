package store

import (
	"container/list"
	"sync"
)

// lru 进程内检索缓存。任何写入都会整体清空（单进程单写者，最简单也最不会读到旧数据）。
type lru struct {
	mu  sync.Mutex
	cap int
	ll  *list.List
	m   map[string]*list.Element
}

type lruItem struct {
	k string
	v any
}

func newLRU(n int) *lru { return &lru{cap: n, ll: list.New(), m: map[string]*list.Element{}} }

func (c *lru) get(k string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[k]; ok {
		c.ll.MoveToFront(e)
		return e.Value.(*lruItem).v, true
	}
	return nil, false
}

func (c *lru) put(k string, v any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[k]; ok {
		e.Value.(*lruItem).v = v
		c.ll.MoveToFront(e)
		return
	}
	c.m[k] = c.ll.PushFront(&lruItem{k, v})
	for c.ll.Len() > c.cap {
		e := c.ll.Back()
		c.ll.Remove(e)
		delete(c.m, e.Value.(*lruItem).k)
	}
}

func (c *lru) purge() {
	c.mu.Lock()
	c.ll.Init()
	c.m = map[string]*list.Element{}
	c.mu.Unlock()
}
