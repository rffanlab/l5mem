package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Entry 一条记忆。
type Entry struct {
	ID               int64           `json:"id"`
	Kind             string          `json:"kind"`
	Topic            string          `json:"topic"`
	Body             string          `json:"body"`
	Tags             []string        `json:"tags"`
	Source           string          `json:"source"`
	CreatedAt        string          `json:"created_at"`
	CreatedEpoch     int64           `json:"created_epoch"`
	Caller           string          `json:"caller,omitempty"`
	ValidTo          *int64          `json:"valid_to,omitempty"`
	SupersededBy     *int64          `json:"superseded_by,omitempty"`
	Volatility       string          `json:"volatility"`
	Verified         bool            `json:"verified"`
	RecheckEpoch     *int64          `json:"recheck_epoch,omitempty"`
	LastVerifiedAt   string          `json:"last_verified_at,omitempty"`
	LastVerifiedBy   string          `json:"last_verified_by,omitempty"`
	LastVerifiedNote string          `json:"last_verified_note,omitempty"`
	UseCount         int             `json:"use_count"`
	ContentHash      string          `json:"content_hash"`
	Meta             json.RawMessage `json:"meta,omitempty"`
	// 派生
	Active  bool `json:"active"`
	Stale   bool `json:"stale,omitempty"` // 到了核对期还没核
	Overdue int  `json:"overdue_days,omitempty"`
}

// AddInput 写入参数。
type AddInput struct {
	Kind       string          `json:"kind"`
	Topic      string          `json:"topic"`
	Body       string          `json:"body"`
	Tags       []string        `json:"tags"`
	Source     string          `json:"source"`
	Caller     string          `json:"caller"`
	Volatility string          `json:"volatility"` // 空 = 自动分级
	Unverified bool            `json:"unverified"` // 没有证据：核对周期缩到 7 天
	Supersedes []int64         `json:"supersedes"` // 新值取代这些旧值
	Meta       json.RawMessage `json:"meta"`
	CreatedAt  string          `json:"created_at"` // 导入时可指定
	// RecheckDays：自定这条多少天后到期核对（覆盖按易变程度的默认周期）。
	// 例：一条「60 天内见分晓」的假设，填 60，到期就进待核对清单。
	RecheckDays int `json:"recheck_days"`
	// Cites：这条是拿哪几条旧记忆推出来的。被引用的记忆，它们的标签算「被想起」一次（心头的激活度 +3）。
	Cites []int64 `json:"cites"`
}

// AddResult：Existed=true 表示内容已存在（幂等），返回原条目 id。
type AddResult struct {
	ID         int64   `json:"id"`
	Existed    bool    `json:"existed"`
	Volatility string  `json:"volatility"`
	Superseded []int64 `json:"superseded,omitempty"`
	Retagged   bool    `json:"retagged,omitempty"`
}

// retag 换标签并重建这一条的索引（调用方持有写锁）。
func (s *Store) retag(id int64, tags []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var kind, topic, body string
	if err := tx.QueryRow(`SELECT kind, topic, body FROM entries WHERE id=?`, id).Scan(&kind, &topic, &body); err != nil {
		return err
	}
	for _, q := range []string{`UPDATE entries SET tags=? WHERE id=?`} {
		if _, err := tx.Exec(q, strings.Join(tags, ","), id); err != nil {
			return err
		}
	}
	for _, q := range []string{`DELETE FROM fts_bi WHERE rowid=?`, `DELETE FROM fts_tri WHERE rowid=?`, `DELETE FROM fts_imp WHERE rowid=?`,
		`DELETE FROM entry_tags WHERE eid=?`} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	if err := indexEntry(tx, id, kind, topic, body, tags); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.bump()
	return nil
}

var ErrNotFound = errors.New("没有这条记忆")

func NormKind(k string) (string, error) {
	k = strings.TrimSpace(strings.ToLower(k))
	if k == "" {
		return "fact", nil
	}
	if v, ok := KindAlias[k]; ok {
		return v, nil
	}
	if _, ok := KindBase[k]; ok {
		return k, nil
	}
	return "", fmt.Errorf("不认识的类型 %q（可用：principle/decision/judgment/lesson/fact/memo 或 原则/决策/判断/教训/事实/备忘）", k)
}

func ContentHash(kind, topic, body string) string {
	h := sha256.Sum256([]byte(kind + "\x1f" + normalize(topic) + "\x1f" + normalize(body)))
	return hex.EncodeToString(h[:])
}

func indexText(topic, body string, tags []string) (bi, tri string) {
	raw := topic + "\n" + strings.Join(tags, " ") + "\n" + body
	return bigramize(raw), raw
}

func recheckAt(vol string, verified bool, from time.Time, days int) *int64 {
	if days > 0 {
		e := from.Add(time.Duration(days) * 24 * time.Hour).Unix()
		return &e
	}
	return recheckFor(vol, verified, from)
}

func recheckFor(vol string, verified bool, from time.Time) *int64 {
	days := RecheckDays[vol]
	if !verified && vol != "stable" {
		days = UnverifiedDays
	}
	if days == 0 {
		return nil
	}
	e := from.Add(time.Duration(days) * 24 * time.Hour).Unix()
	return &e
}

// Add 写入一条记忆。内容哈希唯一：同样的内容第二次写入返回「已存在 #id」而不是报错（并发安全、可重放）。
func (s *Store) Add(in AddInput) (*AddResult, error) {
	kind, err := NormKind(in.Kind)
	if err != nil {
		return nil, err
	}
	body := strings.TrimSpace(in.Body)
	if body == "" {
		return nil, errors.New("正文不能为空")
	}
	topic := strings.TrimSpace(in.Topic)
	tags := splitTags(strings.Join(in.Tags, ","))
	vol := strings.ToLower(strings.TrimSpace(in.Volatility))
	explicit := 0
	if vol != "" {
		if _, ok := RecheckDays[vol]; !ok {
			return nil, fmt.Errorf("volatility 只能是 stable / slow / volatile")
		}
		explicit = 1
	} else {
		vol = Classify(kind, topic, body, tags)
	}
	t := now()
	if in.CreatedAt != "" {
		if pt, err := time.ParseInLocation("2006-01-02 15:04:05", in.CreatedAt, time.Local); err == nil {
			t = pt
		} else if pt, err := time.Parse(time.RFC3339, in.CreatedAt); err == nil {
			t = pt
		}
	}
	meta := "{}"
	if len(in.Meta) > 0 && json.Valid(in.Meta) {
		meta = string(in.Meta)
	}
	if len(in.Cites) > 0 { // 引用关系也存进 meta，回头能追
		var m map[string]any
		if json.Unmarshal([]byte(meta), &m) != nil || m == nil {
			m = map[string]any{}
		}
		m["cites"] = in.Cites
		if b, err := json.Marshal(m); err == nil {
			meta = string(b)
		}
	}
	hash := ContentHash(kind, topic, body)
	verified := !in.Unverified

	s.wmu.Lock()
	defer s.wmu.Unlock()
	var existing int64
	err = s.db.QueryRow(`SELECT id FROM entries WHERE content_hash=?`, hash).Scan(&existing)
	if err == nil {
		// 已存在：如果这次带了取代关系，照样补上（重放安全）
		res := &AddResult{ID: existing, Existed: true, Volatility: vol}
		// 同样的内容带了不同的标签：以这次为准（标签是检索的捷径，写的人最清楚该挂什么）
		var oldTags string
		s.db.QueryRow(`SELECT tags FROM entries WHERE id=?`, existing).Scan(&oldTags)
		if len(tags) > 0 && strings.Join(tags, ",") != strings.Join(splitTags(oldTags), ",") {
			if err := s.retag(existing, tags); err != nil {
				return nil, err
			}
			res.Retagged = true
		}
		if len(in.Supersedes) > 0 {
			tx, err := s.db.Begin()
			if err != nil {
				return nil, err
			}
			done, err := supersedeTx(tx, in.Supersedes, existing, t)
			if err != nil {
				tx.Rollback()
				return nil, err
			}
			if err := tx.Commit(); err != nil {
				return nil, err
			}
			res.Superseded = done
			s.bump()
			s.refreshMetas(append(done, existing)...)
		}
		return res, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	r, err := tx.Exec(`INSERT INTO entries(kind,topic,body,tags,source,created_at,created_epoch,caller,volatility,
		volatility_explicit,verified,recheck_epoch,content_hash,meta) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(content_hash) DO NOTHING`,
		kind, topic, body, strings.Join(tags, ","), strings.TrimSpace(in.Source), fmtTime(t), t.Unix(), in.Caller, vol,
		explicit, boolInt(verified), recheckAt(vol, verified, t, in.RecheckDays), hash, meta)
	if err != nil {
		return nil, err
	}
	id, _ := r.LastInsertId()
	if n, _ := r.RowsAffected(); n == 0 { // 极端并发：别人刚写进去
		tx.Rollback()
		s.db.QueryRow(`SELECT id FROM entries WHERE content_hash=?`, hash).Scan(&id)
		return &AddResult{ID: id, Existed: true, Volatility: vol}, nil
	}
	if err := indexEntry(tx, id, kind, topic, body, tags); err != nil {
		return nil, err
	}
	res := &AddResult{ID: id, Volatility: vol}
	if len(in.Supersedes) > 0 {
		done, err := supersedeTx(tx, in.Supersedes, id, t)
		if err != nil {
			return nil, err
		}
		res.Superseded = done
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.bump()
	s.refreshMetas(append(res.Superseded, id)...)
	s.useTags(tags, useWrite, t)
	s.useEntries(in.Cites, useCite)
	return res, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ImportantKinds：这些类型的记忆另外进一张小索引 fts_imp。
// 高频词命中几万条时，主索引只在最新的一段里按相关度排；旧的原则/决策/判断/教训从小索引里补，
// 不用在几万条命中里逐条按类型筛（20 万条时这一步从 11 ms 降到亚毫秒）。
var ImportantKinds = map[string]bool{"principle": true, "decision": true, "judgment": true, "lesson": true}

func indexEntry(tx *sql.Tx, id int64, kind, topic, body string, tags []string) error {
	bi, tri := indexText(topic, body, tags)
	if _, err := tx.Exec(`INSERT INTO fts_bi(rowid,text) VALUES(?,?)`, id, bi); err != nil {
		return err
	}
	if ImportantKinds[kind] {
		if _, err := tx.Exec(`INSERT INTO fts_imp(rowid,text) VALUES(?,?)`, id, bi); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO fts_tri(rowid,text) VALUES(?,?)`, id, tri); err != nil {
		return err
	}
	for _, tg := range tags {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO entry_tags(tag,eid) VALUES(?,?)`, tg, id); err != nil {
			return err
		}
	}
	return nil
}

func supersedeTx(tx *sql.Tx, olds []int64, newID int64, t time.Time) ([]int64, error) {
	var done []int64
	for _, old := range olds {
		if old == newID {
			continue
		}
		r, err := tx.Exec(`UPDATE entries SET superseded_by=?, valid_to=? WHERE id=? AND superseded_by IS NULL`, newID, t.Unix(), old)
		if err != nil {
			return nil, err
		}
		if n, _ := r.RowsAffected(); n > 0 {
			done = append(done, old)
		} else {
			var x int64
			if err := tx.QueryRow(`SELECT id FROM entries WHERE id=?`, old).Scan(&x); err != nil {
				return nil, fmt.Errorf("要取代的 #%d 不存在", old)
			}
		}
	}
	return done, nil
}

// Supersede 用 newID 取代 old（旧值不删，--history 永远能回看）。
func (s *Store) Supersede(old, newID int64) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	var x int64
	if err := s.db.QueryRow(`SELECT id FROM entries WHERE id=?`, newID).Scan(&x); err != nil {
		return fmt.Errorf("新条目 #%d 不存在", newID)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := supersedeTx(tx, []int64{old}, newID, now()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.bump()
	s.refreshMetas(old, newID)
	return nil
}

// Retract 撤回一条错误的记忆（没有新值可取代时用）：设 valid_to，不删。
func (s *Store) Retract(id int64, reason, by string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	note, _ := json.Marshal(map[string]string{"retracted_reason": reason, "retracted_by": by, "retracted_at": fmtTime(now())})
	r, err := s.db.Exec(`UPDATE entries SET valid_to=?, meta=json_patch(meta, ?) WHERE id=? AND valid_to IS NULL`,
		now().Unix(), string(note), id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	s.bump()
	s.refreshMetas(id)
	return nil
}

// Verify 核对过：记下谁、什么时候、怎么核的，按生命周期重新计时。
func (s *Store) Verify(id int64, by, note string) (*Entry, error) {
	s.wmu.Lock()
	t := now()
	var vol string
	if err := s.db.QueryRow(`SELECT volatility FROM entries WHERE id=?`, id).Scan(&vol); err != nil {
		s.wmu.Unlock()
		return nil, ErrNotFound
	}
	_, err := s.db.Exec(`UPDATE entries SET verified=1, last_verified_at=?, last_verified_epoch=?, last_verified_by=?,
		last_verified_note=?, recheck_epoch=? WHERE id=?`, fmtTime(t), t.Unix(), by, note, recheckFor(vol, true, t), id)
	s.wmu.Unlock()
	if err != nil {
		return nil, err
	}
	s.bump()
	s.useEntries([]int64{id}, useVerify)
	return s.Get(id)
}

// SetVolatility 显式改生命周期级别。
func (s *Store) SetVolatility(id int64, vol string) error {
	if _, ok := RecheckDays[vol]; !ok {
		return fmt.Errorf("volatility 只能是 stable / slow / volatile")
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	var verified int
	var created int64
	if err := s.db.QueryRow(`SELECT verified, COALESCE(last_verified_epoch, created_epoch) FROM entries WHERE id=?`, id).Scan(&verified, &created); err != nil {
		return ErrNotFound
	}
	_, err := s.db.Exec(`UPDATE entries SET volatility=?, volatility_explicit=1, recheck_epoch=? WHERE id=?`,
		vol, recheckFor(vol, verified == 1, time.Unix(created, 0)), id)
	if err == nil {
		s.bump()
		s.refreshMetas(id)
	}
	return err
}

const entryCols = `id,kind,topic,body,tags,source,created_at,created_epoch,caller,valid_to,superseded_by,volatility,verified,
	recheck_epoch,COALESCE(last_verified_at,''),COALESCE(last_verified_by,''),COALESCE(last_verified_note,''),use_count,content_hash,meta`

func scanEntry(sc interface{ Scan(...any) error }) (*Entry, error) {
	var e Entry
	var tags, meta string
	var verified int
	var validTo, sup, recheck sql.NullInt64
	err := sc.Scan(&e.ID, &e.Kind, &e.Topic, &e.Body, &tags, &e.Source, &e.CreatedAt, &e.CreatedEpoch, &e.Caller, &validTo,
		&sup, &e.Volatility, &verified, &recheck, &e.LastVerifiedAt, &e.LastVerifiedBy, &e.LastVerifiedNote, &e.UseCount,
		&e.ContentHash, &meta)
	if err != nil {
		return nil, err
	}
	e.Tags = splitTags(tags)
	if e.Tags == nil {
		e.Tags = []string{}
	}
	e.Verified = verified == 1
	if validTo.Valid {
		e.ValidTo = &validTo.Int64
	}
	if sup.Valid {
		e.SupersededBy = &sup.Int64
	}
	if recheck.Valid {
		e.RecheckEpoch = &recheck.Int64
	}
	if meta != "" && meta != "{}" {
		e.Meta = json.RawMessage(meta)
	}
	t := now().Unix()
	e.Active = e.SupersededBy == nil && (e.ValidTo == nil || *e.ValidTo > t)
	if e.Active && e.RecheckEpoch != nil && *e.RecheckEpoch <= t {
		e.Stale = true
		e.Overdue = int((t - *e.RecheckEpoch) / 86400)
	}
	return &e, nil
}

func (s *Store) Get(id int64) (*Entry, error) {
	e, err := scanEntry(s.db.QueryRow(`SELECT `+entryCols+` FROM entries WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return e, err
}

// History：某条记忆的完整取代链（向前追到最早、向后追到最新）；或某主题下的全部（含已取代）。
func (s *Store) History(id int64, topic string) ([]*Entry, error) {
	var out []*Entry
	if id > 0 {
		seen := map[int64]bool{}
		// 往前：谁被它取代
		var chain []int64
		var walkBack func(x int64)
		walkBack = func(x int64) {
			rows, err := s.db.Query(`SELECT id FROM entries WHERE superseded_by=?`, x)
			if err != nil {
				return
			}
			var prev []int64
			for rows.Next() {
				var p int64
				rows.Scan(&p)
				prev = append(prev, p)
			}
			rows.Close()
			for _, p := range prev {
				if !seen[p] {
					seen[p] = true
					walkBack(p)
					chain = append(chain, p)
				}
			}
		}
		seen[id] = true
		walkBack(id)
		chain = append(chain, id)
		cur := id
		for i := 0; i < 100; i++ { // 往后：它被谁取代
			var nx sql.NullInt64
			if err := s.db.QueryRow(`SELECT superseded_by FROM entries WHERE id=?`, cur).Scan(&nx); err != nil || !nx.Valid || seen[nx.Int64] {
				break
			}
			seen[nx.Int64] = true
			chain = append(chain, nx.Int64)
			cur = nx.Int64
		}
		for _, x := range chain {
			if e, err := s.Get(x); err == nil {
				out = append(out, e)
			}
		}
		return out, nil
	}
	rows, err := s.db.Query(`SELECT `+entryCols+` FROM entries WHERE topic >= ? AND topic < ? ORDER BY created_epoch, id LIMIT 500`,
		topic, topic+"\U0010ffff")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		if e, err := scanEntry(rows); err == nil {
			out = append(out, e)
		}
	}
	return out, rows.Err()
}
