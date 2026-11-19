package store

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// MetricsReport 使用日报：谁在用、各命令 p50/p95、标签命中率、标签路 vs 非标签路延迟、联想使用、空手而归率。
// 附带预设判决（写在报告里，不靠事后找理由）。
type MetricsReport struct {
	Since         string                    `json:"since"`
	Until         string                    `json:"until"`
	Events        int                       `json:"events"`
	Callers       map[string]map[string]int `json:"callers"`    // caller → cmd → 次数
	Latency       map[string][2]float64     `json:"latency_ms"` // cmd → [p50, p95]
	Searches      int                       `json:"searches"`
	EmptySearches int                       `json:"empty_searches"`
	CachedRatio   float64                   `json:"cached_ratio"`
	TagTop1Ratio  float64                   `json:"tag_top1_ratio"`
	TagP50        float64                   `json:"tag_route_p50_ms"`
	NonTagP50     float64                   `json:"non_tag_p50_ms"`
	Relates       int                       `json:"relates"`
	RelateP50     float64                   `json:"relate_p50_ms"`
	Entries       int                       `json:"entries"`
	EntriesBefore int                       `json:"entries_before"`
	Tags          int                       `json:"tags"`
	Verdicts      []string                  `json:"verdicts"`
	Empty         bool                      `json:"empty"` // --since-last 且没有新使用：静默
}

type metricsSnap struct {
	TS      string `json:"ts"`
	Entries int    `json:"entries"`
}

func pct(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sort.Float64s(xs)
	i := int(math.Ceil(p*float64(len(xs)))) - 1
	if i < 0 {
		i = 0
	}
	return round3(xs[i])
}

// Metrics 读 usage.jsonl 出报告。sinceLast=true 时从上次快照起算并更新快照。
func (s *Store) Metrics(since time.Time, sinceLast bool) (*MetricsReport, error) {
	if s.Usage == nil {
		return nil, fmt.Errorf("没有开启使用日志")
	}
	snapPath := filepath.Join(s.Usage.dir, "metrics_snapshot.json")
	var snap metricsSnap
	if sinceLast {
		if b, err := os.ReadFile(snapPath); err == nil {
			json.Unmarshal(b, &snap)
			if t, err := time.Parse(time.RFC3339Nano, snap.TS); err == nil {
				since = t
			}
		}
	}
	rep := &MetricsReport{Callers: map[string]map[string]int{}, Latency: map[string][2]float64{}, Since: since.Format(time.RFC3339),
		Until: time.Now().Format(time.RFC3339), EntriesBefore: snap.Entries}
	lat := map[string][]float64{}
	var tagLat, nonTagLat, relLat []float64
	tagTop, cached := 0, 0
	f, err := os.Open(s.Usage.Path())
	if err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			var e UsageEvent
			if json.Unmarshal(sc.Bytes(), &e) != nil {
				continue
			}
			t, err := time.Parse(time.RFC3339Nano, e.TS)
			if err != nil || !t.After(since) {
				continue
			}
			rep.Events++
			if rep.Callers[e.Caller] == nil {
				rep.Callers[e.Caller] = map[string]int{}
			}
			rep.Callers[e.Caller][e.Cmd]++
			lat[e.Cmd] = append(lat[e.Cmd], e.Ms)
			switch e.Cmd {
			case "search":
				rep.Searches++
				if e.Hits == 0 {
					rep.EmptySearches++
				}
				if e.Cached {
					cached++
					continue
				}
				if e.TagCands > 0 {
					tagLat = append(tagLat, e.Ms)
				} else {
					nonTagLat = append(nonTagLat, e.Ms)
				}
				if e.TagTop1 != nil && *e.TagTop1 {
					tagTop++
				}
			case "relate":
				rep.Relates++
				relLat = append(relLat, e.Ms)
			}
		}
		f.Close()
	}
	for cmd, xs := range lat {
		rep.Latency[cmd] = [2]float64{pct(append([]float64{}, xs...), 0.5), pct(append([]float64{}, xs...), 0.95)}
	}
	if rep.Searches > 0 {
		rep.CachedRatio = round3(float64(cached) / float64(rep.Searches))
		if n := rep.Searches - cached; n > 0 {
			rep.TagTop1Ratio = round3(float64(tagTop) / float64(n))
		}
	}
	rep.TagP50, rep.NonTagP50, rep.RelateP50 = pct(tagLat, 0.5), pct(nonTagLat, 0.5), pct(relLat, 0.5)
	st, _ := s.Stats()
	if st != nil {
		rep.Entries, rep.Tags = st.Entries, st.Tags
	}
	// 预设判决线
	if rep.Searches-cached >= 20 {
		switch {
		case rep.TagTop1Ratio >= 0.5:
			rep.Verdicts = append(rep.Verdicts, fmt.Sprintf("标签命中率 %.0f%%：标签打得准，标签路设计成立", rep.TagTop1Ratio*100))
		case rep.TagTop1Ratio < 0.2:
			rep.Verdicts = append(rep.Verdicts, fmt.Sprintf("标签命中率只有 %.0f%%：大家在乱打标签，考虑收回标签加分（L5_TAG_BOOST=0）", rep.TagTop1Ratio*100))
		}
	}
	if len(tagLat) >= 10 && len(nonTagLat) >= 10 && rep.TagP50 > 2*rep.NonTagP50 {
		rep.Verdicts = append(rep.Verdicts, fmt.Sprintf("标签路 p50 %.2fms 超过非标签路 %.2fms 的 2 倍：枢纽标签漏水，回炉修扇出上限", rep.TagP50, rep.NonTagP50))
	}
	if sinceLast && rep.Relates == 0 && time.Since(since) >= 14*24*time.Hour {
		rep.Verdicts = append(rep.Verdicts, "联想连续两周零使用：冻结联想功能的进一步投入")
	}
	if rep.Searches > 0 && float64(rep.EmptySearches)/float64(rep.Searches) > 0.5 {
		rep.Verdicts = append(rep.Verdicts, fmt.Sprintf("一半以上的检索空手而归（%d/%d）：库的覆盖跟不上需求", rep.EmptySearches, rep.Searches))
	}
	rep.Empty = sinceLast && rep.Events == 0
	if sinceLast {
		b, _ := json.Marshal(metricsSnap{TS: time.Now().Format(time.RFC3339Nano), Entries: rep.Entries})
		os.WriteFile(snapPath, b, 0o640)
	}
	return rep, nil
}
