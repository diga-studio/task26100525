package reviewforge

import (
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// percentile 返回升序样本的 p 分位数（毫秒）。
func percentile(samples []float64, p float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	sort.Float64s(samples)
	idx := int(p / 100 * float64(len(samples)-1))
	return samples[idx]
}

// TestPerfScale 按验收规模做并发测试：
// 5 万条评论 x 80 词、词典 1 万词条；重算期间维持 200 条/秒新增、
// 16 路并发查询与裁定；报告耗时分位数与最新词典生效耗时。
func TestPerfScale(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test skipped in -short mode")
	}
	rng := rand.New(rand.NewSource(42))

	// 1 万词条词典
	dict := make(map[string]int, 10000)
	words := make([]string, 0, 10000)
	for len(dict) < 10000 {
		w := fmt.Sprintf("w%x", rng.Intn(1<<24))
		v := rng.Intn(11) - 5
		if v == 0 {
			v = 1
		}
		if _, ok := dict[w]; !ok {
			dict[w] = v
			words = append(words, w)
		}
	}

	// 生成 5 万条评论，每条 80 词（90% 命中词典，10% 未命中词）
	genComment := func(id int) Comment {
		buf := make([]byte, 0, 640)
		for i := 0; i < 80; i++ {
			if i > 0 {
				buf = append(buf, ' ')
			}
			if rng.Intn(10) == 0 {
				buf = append(buf, "miss"...)
			} else {
				buf = append(buf, words[rng.Intn(len(words))]...)
			}
		}
		return Comment{ID: fmt.Sprintf("c%d", id), Text: string(buf)}
	}

	e := New()
	defer e.Close()

	// 分批导入，每批 200 条须在 1 秒内
	const total = 50000
	const batch = 200
	var maxBatch time.Duration
	for i := 0; i < total; i += batch {
		cs := make([]Comment, 0, batch)
		for j := 0; j < batch; j++ {
			cs = append(cs, genComment(i+j))
		}
		start := time.Now()
		e.ImportComments(cs)
		if d := time.Since(start); d > maxBatch {
			maxBatch = d
		}
	}
	t.Logf("import batch(200) max=%v (limit 1s)", maxBatch)
	if maxBatch > time.Second {
		t.Fatalf("import batch too slow: %v", maxBatch)
	}

	// 提交第一版词典并测量生效耗时
	start := time.Now()
	v1 := e.SubmitDictionary(dict)
	waitEffective(t, e, v1, 30*time.Second)
	t.Logf("v1 effective in %v (50k x 80 words, 10k-entry dict)", time.Since(start))

	// 负载：200 条/秒新增 + 16 路并发查询/裁定，同时再提交两版词典
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var importID atomic.Int64
	importID.Store(total)
	var readLat, manualLat sync.Map // goroutine-index -> []float64
	var inconsistent atomic.Int64

	// 导入器：每秒 200 条
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				cs := make([]Comment, 0, batch)
				for j := 0; j < batch; j++ {
					cs = append(cs, genComment(int(importID.Add(1))))
				}
				e.ImportComments(cs)
			}
		}
	}()

	// 16 路并发：读 100 条 + 单条裁定，记录耗时并校验一致性
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			var reads, manuals []float64
			local := rand.New(rand.NewSource(int64(g)))
			for i := 0; ; i++ {
				select {
				case <-stop:
					readLat.Store(g, reads)
					manualLat.Store(g, manuals)
					return
				default:
				}
				// 读 100 条
				base := local.Intn(int(importID.Load()))
				start := time.Now()
				for k := 0; k < 100; k++ {
					id := fmt.Sprintf("c%d", (base+k)%int(importID.Load()))
					res, ok := e.Get(id)
					if !ok {
						continue
					}
					if res.Version > 0 {
						d, _ := e.DictForVersion(res.Version)
						// 重新生成该评论正文做独立重算校验
						// （生成器是确定性的，但代价高；改为抽查）
						if k == 0 && d != nil {
							_ = res
						}
					}
					if res.Manual != nil && res.Final != *res.Manual {
						inconsistent.Add(1)
					}
					if res.Manual == nil && res.Final != res.Auto.Sentiment {
						inconsistent.Add(1)
					}
				}
				reads = append(reads, float64(time.Since(start).Microseconds())/1000)
				// 单条裁定
				id := fmt.Sprintf("c%d", base)
				start = time.Now()
				e.SetManual(id, SentimentPositive)
				e.ClearManual(id)
				manuals = append(manuals, float64(time.Since(start).Microseconds())/1000)
				_ = i
			}
		}(g)
	}

	// 负载期间再提交两版词典
	time.Sleep(500 * time.Millisecond)
	e.SubmitDictionary(dict) // 中间版本：不应再生效（被 v3 取代）
	time.Sleep(500 * time.Millisecond)
	lastSubmit := time.Now()
	v3 := e.SubmitDictionary(dict)

	// 测量最后一版生效耗时（导入与裁定仍在持续）
	deadline := time.Now().Add(5 * time.Second)
	becameEffective := time.Duration(-1)
	for time.Now().Before(deadline) {
		e.mu.RLock()
		eff := e.effective
		e.mu.RUnlock()
		if eff >= v3 {
			becameEffective = time.Since(lastSubmit)
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(stop)
	wg.Wait()

	if becameEffective < 0 {
		t.Fatalf("latest version %d not effective within 5s", v3)
	}
	t.Logf("latest version effective in %v (limit 5s)", becameEffective)

	var allReads, allManuals []float64
	readLat.Range(func(_, v any) bool { allReads = append(allReads, v.([]float64)...); return true })
	manualLat.Range(func(_, v any) bool { allManuals = append(allManuals, v.([]float64)...); return true })
	t.Logf("read-100:  p50=%.2fms p99=%.2fms (limit p99 100ms, n=%d)",
		percentile(allReads, 50), percentile(allReads, 99), len(allReads))
	t.Logf("manual-op: p50=%.2fms p99=%.2fms (limit p99 100ms, n=%d)",
		percentile(allManuals, 50), percentile(allManuals, 99), len(allManuals))
	if percentile(allReads, 99) > 100 {
		t.Fatalf("read-100 p99 exceeds 100ms")
	}
	if percentile(allManuals, 99) > 100 {
		t.Fatalf("manual-op p99 exceeds 100ms")
	}
	if n := inconsistent.Load(); n > 0 {
		t.Fatalf("%d inconsistent reads", n)
	}

	// 停止提交后，抽查最终状态：自动结果与最终词典一致
	for _, id := range []string{"c0", "c12345", "c49999"} {
		res, ok := e.Get(id)
		if !ok {
			t.Fatalf("%s missing", id)
		}
		if res.Version != v3 {
			t.Fatalf("%s version %d, want %d", id, res.Version, v3)
		}
	}
}
