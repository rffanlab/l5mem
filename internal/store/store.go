// Package store 是 L5 记忆库本体：SQLite 单文件（WAL），entries 是本体，FTS/标签表只是索引、随时可重建。
package store

import (
	"database/sql"
	"fmt"
	"math"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const SchemaVersion = 1

// Kind 基线重要性：原则 > 决策 > 判断 > 教训 > 事实 > 备忘。
var KindBase = map[string]float64{
	"principle": 1.0, "decision": 0.9, "judgment": 0.85, "lesson": 0.85, "fact": 0.6, "memo": 0.45,
}

// 中文别名 → 内部 kind。
var KindAlias = map[string]string{
	"原则": "principle", "决策": "decision", "判断": "judgment", "研判": "judgment", "教训": "lesson",
	"事实": "fact", "备忘": "memo", "笔记": "memo",
}

var KindLabel = map[string]string{
	"principle": "原则", "decision": "决策", "judgment": "判断", "lesson": "教训", "fact": "事实", "memo": "备忘",
}

// 生命周期：stable 永不核对；slow 半年；volatile 一个月；未核实的缩到 7 天。
var RecheckDays = map[string]int{"stable": 0, "slow": 180, "volatile": 30}

const UnverifiedDays = 7

// Weights 排序权重，可用环境变量调（L5_W_LEX 等）。
type Weights struct {
	Lex, Prox, Rec, Imp float64
	HalfLifeDays        float64 // 新鲜度半衰期
	StableFlat          bool    // stable 记忆不随时间衰减（原则/教训不该被昨天的备忘挤下去）
	TagBoost            float64 // 查询词正好是标签名时的额外加分
	RRFK                float64
}

func envF(k string, d float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(k), 64); err == nil {
		return v
	}
	return d
}

func DefaultWeights() Weights {
	return Weights{
		Lex: envF("L5_W_LEX", 0.45), Prox: envF("L5_W_PROX", 0.15), Rec: envF("L5_W_REC", 0.25),
		Imp: envF("L5_W_IMP", 0.15), HalfLifeDays: envF("L5_HALF_LIFE_DAYS", 30),
		StableFlat: os.Getenv("L5_STABLE_FLAT") != "0", TagBoost: envF("L5_TAG_BOOST", 0.10), RRFK: 60,
	}
}

// Store 记忆库。写操作串行（单写者），读并发；检索结果有进程内 LRU，任何写入都会让缓存失效。
type Store struct {
	db      *sql.DB
	path    string
	wmu     sync.Mutex
	gen     atomic.Uint64 // 写入代数：缓存失效用
	W       Weights
	cache   *lru
	hitsMu  sync.Mutex
	hits    map[int64]int // 待回写的访问次数（读路径零写库）
	Usage   *UsageLog
	hubCapN int
	metaMu  sync.RWMutex
	metas   map[int64]*candMeta // 轻量元数据常驻内存：候选筛选和打分不用再回库
	// 同义词组：小写名 → 组名；组名 → 所有写法
	aliasMu     sync.RWMutex
	aliasOf     map[string]string
	aliasGroups map[string][]string
}

func Open(path string, usageDir string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=10000&_synchronous=NORMAL&_foreign_keys=off&_txlock=immediate&cache=private", path)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	db.SetConnMaxIdleTime(0)
	s := &Store{db: db, path: path, W: DefaultWeights(), cache: newLRU(512), hits: map[int64]int{}, hubCapN: 12}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	for _, p := range []string{"PRAGMA cache_size=-65536", "PRAGMA temp_store=MEMORY", "PRAGMA mmap_size=268435456"} {
		db.Exec(p)
	}
	if usageDir != "" {
		s.Usage = NewUsageLog(usageDir)
	}
	if err := s.loadMetas(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.loadAliases(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	s.FlushHits()
	if s.Usage != nil {
		s.Usage.Close()
	}
	return s.db.Close()
}

func (s *Store) DB() *sql.DB  { return s.db }
func (s *Store) Path() string { return s.path }

const schema = `
CREATE TABLE IF NOT EXISTS entries(
  id INTEGER PRIMARY KEY,
  kind TEXT NOT NULL,
  topic TEXT NOT NULL DEFAULT '',
  body TEXT NOT NULL,
  tags TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  created_epoch INTEGER NOT NULL,
  caller TEXT NOT NULL DEFAULT '',
  valid_to INTEGER,
  superseded_by INTEGER,
  volatility TEXT NOT NULL DEFAULT 'slow',
  volatility_explicit INTEGER NOT NULL DEFAULT 0,
  verified INTEGER NOT NULL DEFAULT 1,
  recheck_epoch INTEGER,
  last_verified_at TEXT,
  last_verified_epoch INTEGER,
  last_verified_by TEXT,
  last_verified_note TEXT,
  use_count INTEGER NOT NULL DEFAULT 0,
  content_hash TEXT NOT NULL UNIQUE,
  meta TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS ix_entries_topic ON entries(topic COLLATE BINARY);
CREATE INDEX IF NOT EXISTS ix_entries_active ON entries(superseded_by, valid_to);
CREATE INDEX IF NOT EXISTS ix_entries_recheck ON entries(recheck_epoch) WHERE recheck_epoch IS NOT NULL;
CREATE INDEX IF NOT EXISTS ix_entries_created ON entries(created_epoch);
CREATE INDEX IF NOT EXISTS ix_entries_kind ON entries(kind, created_epoch);
CREATE TABLE IF NOT EXISTS entry_tags(tag TEXT NOT NULL COLLATE NOCASE, eid INTEGER NOT NULL, PRIMARY KEY(tag, eid)) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS ix_entry_tags_eid ON entry_tags(eid);
CREATE VIRTUAL TABLE IF NOT EXISTS fts_bi USING fts5(text, tokenize='unicode61 remove_diacritics 2');
CREATE VIRTUAL TABLE IF NOT EXISTS fts_tri USING fts5(text, tokenize='trigram');
CREATE TABLE IF NOT EXISTS kv(k TEXT PRIMARY KEY, v TEXT NOT NULL);
`

func (s *Store) migrate() error {
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("建表失败：%w", err)
	}
	if _, err := s.db.Exec(aliasSchema); err != nil {
		return fmt.Errorf("建同义词表失败：%w", err)
	}
	_, err := s.db.Exec(`INSERT INTO kv(k,v) VALUES('schema_version',?) ON CONFLICT(k) DO NOTHING`, strconv.Itoa(SchemaVersion))
	return err
}

func now() time.Time { return time.Now() }

func fmtTime(t time.Time) string { return t.Local().Format("2006-01-02 15:04:05") }

// recency：按半衰期指数衰减。
func (s *Store) recency(createdEpoch int64, volatility string, t time.Time) float64 {
	if s.W.StableFlat && volatility == "stable" {
		return 1
	}
	days := t.Sub(time.Unix(createdEpoch, 0)).Hours() / 24
	if days < 0 {
		days = 0
	}
	return math.Pow(0.5, days/s.W.HalfLifeDays)
}

func importance(kind string, useCount int) float64 {
	b, ok := KindBase[kind]
	if !ok {
		b = 0.5
	}
	v := b + math.Log1p(float64(useCount))/20
	if v > 1 {
		v = 1
	}
	return v
}

// Generation 当前写入代数（缓存键的一部分）。
func (s *Store) Generation() uint64 { return s.gen.Load() }

func (s *Store) bump() { s.gen.Add(1); s.cache.purge() }

// ---------------- 常驻内存的轻量元数据 ----------------

const metaCols = `id,kind,topic,created_epoch,volatility,use_count,superseded_by IS NULL,COALESCE(valid_to,0)`

func scanMeta(sc interface{ Scan(...any) error }) (*candMeta, error) {
	m := &candMeta{}
	var notSup int
	err := sc.Scan(&m.id, &m.kind, &m.topic, &m.created, &m.volatility, &m.useCount, &notSup, &m.validTo)
	m.notSuperseded = notSup == 1
	return m, err
}

func (s *Store) loadMetas() error {
	rows, err := s.db.Query(`SELECT ` + metaCols + ` FROM entries`)
	if err != nil {
		return err
	}
	defer rows.Close()
	all := map[int64]*candMeta{}
	for rows.Next() {
		m, err := scanMeta(rows)
		if err != nil {
			return err
		}
		all[m.id] = m
	}
	s.metaMu.Lock()
	s.metas = all
	s.metaMu.Unlock()
	return rows.Err()
}

// refreshMetas 写入后刷新受影响的条目。
func (s *Store) refreshMetas(ids ...int64) {
	for _, id := range ids {
		m, err := scanMeta(s.db.QueryRow(`SELECT `+metaCols+` FROM entries WHERE id=?`, id))
		s.metaMu.Lock()
		if err == nil {
			s.metas[id] = m
		} else {
			delete(s.metas, id)
		}
		s.metaMu.Unlock()
	}
}
