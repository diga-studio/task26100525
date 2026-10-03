package reviewforge

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	scaleReviews     = 50000
	scaleWords       = 80
	scaleLexiconSize = 10000
)

func buildLexicon(seed int) map[string]int {
	lex := make(map[string]int, scaleLexiconSize)
	for i := 0; i < scaleLexiconSize; i++ {
		score := (i+seed)%10 - 5
		if score >= 0 {
			score++
		}
		lex[fmt.Sprintf("w%d", i)] = score
	}
	return lex
}

func buildText(id int) string {
	var b strings.Builder
	b.Grow(scaleWords * 6)
	for j := 0; j < scaleWords; j++ {
		fmt.Fprintf(&b, "w%d ", (id*31+j*7)%12000) // 1/6 的词未命中词典
	}
	return b.String()
}

type latencyRecorder struct {
	mu     sync.Mutex
	reads  []time.Duration
	adjuds []time.Duration
}

func (r *latencyRecorder) addRead(d time.Duration) {
	r.mu.Lock()
	r.reads = append(r.reads, d)
	r.mu.Unlock()
}

func (r *latencyRecorder) addAdjud(d time.Duration) {
	r.mu.Lock()
	r.adjuds = append(r.adjuds, d)
	r.mu.Unlock()
}

func percentile(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(ds))
	copy(sorted, ds)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(p * float64(len(sorted)-1))
	return sorted[idx]
}

// TestScaleConcurrency 在 5 万条评论、1 万词条的规模下：
// 重算期间维持每秒 200 条新增及 16 路并发查询和裁定，
// 校验 P99 时延、最新词典 5 秒内生效，并持续检查版本与结果一致性。
func TestScaleConcurrency(t *testing.T) {
	e := NewEngine()
	defer e.Close()

	// 预置 5 万条评论，每批 200 条导入并计时
	texts := make([]string, 0, scaleReviews+scaleReviews)
	importBatchMax := time.Duration(0)
	for i := 0; i < scaleReviews; i += 200 {
		batch := make([]Review, 0, 200)
		for j := 0; j < 200; j++ {
			text := buildText(i + j)
			texts = append(texts, text)
			batch = append(batch, Review{ID: fmt.Sprintf("r%d", i+j), Text: text})
		}
		start := time.Now()
		e.ImportReviews(batch)
		if d := time.Since(start); d > importBatchMax {
			importBatchMax = d
		}
	}
	t.Logf("preload: %d reviews, max 200-batch import = %v", scaleReviews, importBatchMax)
	if importBatchMax > time.Second {
		t.Fatalf("200-review import batch took %v > 1s", importBatchMax)
	}

	v1, _ := e.SubmitLexicon(buildLexicon(0))
	waitEffective(t, e, v1, 10*time.Second)

	// 负载：1 个导入协程（每秒 200 条）+ 16 路并发查询和裁定
	var rec latencyRecorder
	var inconsistencies atomic.Int64
	var textsMu sync.RWMutex
	var nextID atomic.Int64
	nextID.Store(scaleReviews)
	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() { // 每秒 200 条新增评论
		defer wg.Done()
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				batch := make([]Review, 0, 20)
				for j := 0; j < 20; j++ {
					id := nextID.Add(1) - 1
					text := buildText(int(id))
					textsMu.Lock()
					texts = append(texts, text)
					textsMu.Unlock()
					batch = append(batch, Review{ID: fmt.Sprintf("r%d", id), Text: text})
				}
				e.ImportReviews(batch)
			}
		}
	}()

	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			i := worker
			for {
				select {
				case <-stop:
					return
				default:
				}
				idx := i % int(nextID.Load())
				id := fmt.Sprintf("r%d", idx)
				start := time.Now()
				res, ok := e.GetResult(id)
				rec.addRead(time.Since(start))
				if ok {
					lex, _ := e.Lexicon(res.Version)
					textsMu.RLock()
					text := texts[idx]
					textsMu.RUnlock()
					if want := Analyze(text, lex); want != res.Auto {
						inconsistencies.Add(1)
					}
				}
				if i%7 == 0 { // 穿插裁定与清除
					start = time.Now()
					_ = e.SetAdjudication(id, CategoryPositive)
					rec.addAdjud(time.Since(start))
					res, ok = e.GetResult(id)
					if !ok || !res.HasAdjudication || res.Final != string(CategoryPositive) {
						inconsistencies.Add(1) // 已返回成功的裁定必须在随后读取中可见
					}
					e.ClearAdjudication(id)
				}
				i += 16
			}
		}(w)
	}

	// 负载下连续提交两个新词典，随后停止提交
	time.Sleep(time.Second)
	if _, err := e.SubmitLexicon(buildLexicon(3)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	vLast, _ := e.SubmitLexicon(buildLexicon(7))
	submitDone := time.Now()
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()

	waitEffective(t, e, vLast, 10*time.Second)
	effectiveIn := time.Since(submitDone)
	t.Logf("latest lexicon v%d effective in %v after submissions stopped", vLast, effectiveIn)
	if effectiveIn > 5*time.Second {
		t.Fatalf("latest lexicon took %v > 5s to become effective", effectiveIn)
	}
	if got := e.EffectiveVersion(); got != vLast {
		t.Fatalf("effective version = %d, want %d", got, vLast)
	}
	if n := inconsistencies.Load(); n > 0 {
		t.Fatalf("%d version/result inconsistencies observed", n)
	}

	p50 := percentile(rec.reads, 0.50)
	p99 := percentile(rec.reads, 0.99)
	aP99 := percentile(rec.adjuds, 0.99)
	t.Logf("reads: n=%d p50=%v p99=%v; adjudications: n=%d p99=%v",
		len(rec.reads), p50, p99, len(rec.adjuds), aP99)
	if p99 > 100*time.Millisecond {
		t.Fatalf("read P99 = %v > 100ms", p99)
	}
	if aP99 > 100*time.Millisecond {
		t.Fatalf("adjudication P99 = %v > 100ms", aP99)
	}

	// 抽样验证最终状态下全部评论均可查询且与最后词典一致
	lex, _ := e.Lexicon(vLast)
	total := int(nextID.Load())
	for i := 0; i < total; i += 997 {
		res, ok := e.GetResult(fmt.Sprintf("r%d", i))
		if !ok {
			t.Fatalf("imported review r%d not queryable", i)
		}
		if res.Version != vLast {
			t.Fatalf("review r%d at version %d, want %d", i, res.Version, vLast)
		}
		if want := Analyze(texts[i], lex); want != res.Auto {
			t.Fatalf("review r%d: auto=%+v, recompute=%+v", i, res.Auto, want)
		}
	}
	t.Logf("total reviews imported: %d", total)
}
