package store

import (
	"strings"
	"time"
)

// 生命周期自动分级：偏好靠记（stable 永不核对），行情靠核（volatile 用前先核）。
// 按可信度取序：主题快变词 > 主题稳定词 > kind 稳定类 > 正文快变词 > 正文稳定词 > 默认 slow。

var fastWords = []string{
	"订阅", "价格", "定价", "售价", "报价", "额度", "限额", "配额", "截止", "期限", "到期", "版本", "排名", "榜单", "汇率", "利率",
	"股价", "市值", "估值", "融资", "营收", "收入", "财报", "季度", "发布", "上线", "下线", "停服", "套餐", "会员", "优惠",
	"库存", "余额", "账单", "费用", "价钱", "最新", "目前", "当前", "现任", "ceo", "price", "pricing", "subscription", "plan", "quota",
	"limit", "deadline", "version", "release", "rank", "ranking", "valuation", "funding", "revenue", "beta", "preview", "roadmap",
}

var stableWords = []string{
	"口味", "喜欢", "不喜欢", "讨厌", "偏好", "习惯", "风格", "原则", "教训", "性格", "生日", "籍贯", "母语", "信仰", "家人",
	"价值观", "历史", "当年", "曾经", "以前", "决定", "已决定", "约定", "规矩", "禁忌", "名字", "称呼", "preference", "principle",
	"lesson", "style", "always", "never",
}

var stableKinds = map[string]bool{"principle": true, "lesson": true, "decision": true}

func containsAny(s string, words []string) bool {
	s = strings.ToLower(s)
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

// Classify 自动判断生命周期级别。
func Classify(kind, topic, body string, tags []string) string {
	head := topic + " " + strings.Join(tags, " ")
	switch {
	case containsAny(head, fastWords):
		return "volatile"
	case containsAny(head, stableWords):
		return "stable"
	case stableKinds[kind]:
		return "stable"
	case containsAny(body, fastWords):
		return "volatile"
	case containsAny(body, stableWords):
		return "stable"
	}
	return "slow"
}

// Due 列出到了（或 days 天内将到）核对期的有效记忆，最逾期的在前。
func (s *Store) Due(days int, limit int) ([]*Entry, error) {
	if limit <= 0 {
		limit = 100
	}
	t := time.Now()
	until := t.Add(time.Duration(days) * 24 * time.Hour).Unix()
	rows, err := s.db.Query(`SELECT `+entryCols+` FROM entries WHERE recheck_epoch IS NOT NULL AND recheck_epoch <= ?
		AND superseded_by IS NULL AND (valid_to IS NULL OR valid_to > ?) ORDER BY recheck_epoch LIMIT ?`, until, t.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Entry
	for rows.Next() {
		if e, err := scanEntry(rows); err == nil {
			out = append(out, e)
		}
	}
	return out, rows.Err()
}

// Reclassify 按当前规则重算所有「没被显式指定」的条目的生命周期；返回改动数。
func (s *Store) Reclassify() (int, error) {
	rows, err := s.db.Query(`SELECT id,kind,topic,body,tags,volatility,verified,COALESCE(last_verified_epoch,created_epoch)
		FROM entries WHERE volatility_explicit=0`)
	if err != nil {
		return 0, err
	}
	type upd struct {
		id  int64
		vol string
		rc  *int64
	}
	var ups []upd
	for rows.Next() {
		var id, base int64
		var kind, topic, body, tags, vol string
		var verified int
		if err := rows.Scan(&id, &kind, &topic, &body, &tags, &vol, &verified, &base); err != nil {
			continue
		}
		nv := Classify(kind, topic, body, splitTags(tags))
		if nv != vol {
			ups = append(ups, upd{id, nv, recheckFor(nv, verified == 1, time.Unix(base, 0))})
		}
	}
	rows.Close()
	if len(ups) == 0 {
		return 0, nil
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, u := range ups {
		if _, err := tx.Exec(`UPDATE entries SET volatility=?, recheck_epoch=? WHERE id=?`, u.vol, u.rc, u.id); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	s.bump()
	s.wmu.Unlock()
	s.loadMetas()
	s.wmu.Lock()
	return len(ups), nil
}
