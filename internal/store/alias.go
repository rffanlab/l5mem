package store

import (
	"errors"
	"sort"
	"strings"
)

// 同义词组（别名）：「英伟达 / NVIDIA / Nvidia / NVDA」是同一个东西。
// 检索时，查询里的词命中某个组，就把组里所有写法一起查（每个词内部 OR，词与词之间仍然 AND）；
// 标签点查、邻近度打分也按组算。写入不受影响：原文怎么写就怎么存。
//
// 存储：aliases(name 唯一、不分大小写, canon 组名)。组名就是组里第一个名字。

const aliasSchema = `CREATE TABLE IF NOT EXISTS aliases(name TEXT PRIMARY KEY COLLATE NOCASE, canon TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS ix_aliases_canon ON aliases(canon);`

func (s *Store) loadAliases() error {
	rows, err := s.db.Query(`SELECT name, canon FROM aliases`)
	if err != nil {
		return err
	}
	defer rows.Close()
	byName := map[string]string{}
	groups := map[string][]string{}
	for rows.Next() {
		var n, c string
		if rows.Scan(&n, &c) == nil {
			byName[strings.ToLower(n)] = c
			groups[c] = append(groups[c], n)
		}
	}
	s.aliasMu.Lock()
	s.aliasOf, s.aliasGroups = byName, groups
	s.aliasMu.Unlock()
	return nil
}

// Expand 返回一个词的所有写法（含它自己）；不在任何组里就只有它自己。
func (s *Store) Expand(term string) []string {
	s.aliasMu.RLock()
	defer s.aliasMu.RUnlock()
	c, ok := s.aliasOf[strings.ToLower(strings.TrimSpace(term))]
	if !ok {
		return []string{term}
	}
	out := []string{term}
	for _, n := range s.aliasGroups[c] {
		if !strings.EqualFold(n, term) {
			out = append(out, n)
		}
	}
	return out
}

// AliasGroup 一组同义写法。
type AliasGroup struct {
	Canon string   `json:"canon"`
	Names []string `json:"names"`
}

func (s *Store) Aliases() []AliasGroup {
	s.aliasMu.RLock()
	defer s.aliasMu.RUnlock()
	out := make([]AliasGroup, 0, len(s.aliasGroups))
	for c, ns := range s.aliasGroups {
		cp := append([]string(nil), ns...)
		sort.Strings(cp)
		out = append(out, AliasGroup{Canon: c, Names: cp})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Canon < out[j].Canon })
	return out
}

// AddAliases 把这些名字归为一组；其中已经在别的组里的，两组合并。
func (s *Store) AddAliases(names []string) (*AliasGroup, error) {
	var clean []string
	seen := map[string]bool{}
	for _, n := range names {
		n = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(n), "#"))
		if n == "" || seen[strings.ToLower(n)] {
			continue
		}
		seen[strings.ToLower(n)] = true
		clean = append(clean, n)
	}
	if len(clean) < 2 {
		return nil, errors.New("至少要两个不同的写法")
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// 已有的组：合并进第一个出现的组
	canon := ""
	merge := map[string]bool{}
	for _, n := range clean {
		var c string
		if tx.QueryRow(`SELECT canon FROM aliases WHERE name=?`, n).Scan(&c) == nil {
			if canon == "" {
				canon = c
			}
			merge[c] = true
		}
	}
	if canon == "" {
		canon = clean[0]
	}
	for c := range merge {
		if c != canon {
			if _, err := tx.Exec(`UPDATE aliases SET canon=? WHERE canon=?`, canon, c); err != nil {
				return nil, err
			}
		}
	}
	for _, n := range clean {
		if _, err := tx.Exec(`INSERT INTO aliases(name, canon) VALUES(?,?) ON CONFLICT(name) DO UPDATE SET canon=excluded.canon`, n, canon); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if err := s.loadAliases(); err != nil {
		return nil, err
	}
	s.bump()
	g := AliasGroup{Canon: canon, Names: s.Expand(canon)}
	sort.Strings(g.Names)
	return &g, nil
}

// RemoveAlias 把一个写法移出它的组（组里只剩一个时整组删除）。
func (s *Store) RemoveAlias(name string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	var canon string
	if err := s.db.QueryRow(`SELECT canon FROM aliases WHERE name=?`, strings.TrimSpace(name)).Scan(&canon); err != nil {
		return ErrNotFound
	}
	if _, err := s.db.Exec(`DELETE FROM aliases WHERE name=?`, strings.TrimSpace(name)); err != nil {
		return err
	}
	var left []string
	rows, _ := s.db.Query(`SELECT name FROM aliases WHERE canon=?`, canon)
	for rows != nil && rows.Next() {
		var n string
		rows.Scan(&n)
		left = append(left, n)
	}
	if rows != nil {
		rows.Close()
	}
	if len(left) == 1 {
		s.db.Exec(`DELETE FROM aliases WHERE canon=?`, canon)
	} else if len(left) > 1 && strings.EqualFold(canon, name) {
		s.db.Exec(`UPDATE aliases SET canon=? WHERE canon=?`, left[0], canon)
	}
	s.loadAliases()
	s.bump()
	return nil
}
