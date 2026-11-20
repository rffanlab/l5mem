package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// L5_MIND_SCALE=200000 go test -tags sqlite_fts5 -run TestMindScale -v ./internal/store/
func TestMindScale(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("L5_MIND_SCALE"))
	if n == 0 {
		t.Skip("设 L5_MIND_SCALE 才跑")
	}
	s, err := Open(filepath.Join(t.TempDir(), "l5.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	t0 := time.Now()
	for i := 0; i < n; i++ {
		age := i % 365
		tags := []string{fmt.Sprintf("话题%d", i%3000), fmt.Sprintf("公司%d", (i*7)%800), "宏观"}
		s.Add(AddInput{Topic: fmt.Sprintf("t%d", i), Body: fmt.Sprintf("第 %d 条记录 %d", i, i*31), Tags: tags,
			CreatedAt: time.Now().Add(-time.Duration(age) * 24 * time.Hour).Format("2006-01-02 15:04:05")})
	}
	s.FlushHits()
	t.Logf("写入 %d 条 %v", n, time.Since(t0))
	for i := 0; i < 2; i++ {
		t1 := time.Now()
		m, _ := s.Mind(12)
		d1 := time.Since(t1)
		t2 := time.Now()
		l, _ := s.Links(7, 2, 0, 10)
		t.Logf("第%d次 mind %v（%d 个），links %v（%d 个）", i+1, d1, len(m.Tags), time.Since(t2), len(l.Links))
	}
}
