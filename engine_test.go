package reviewforge

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func waitEffective(t *testing.T, e *Engine, version uint64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if e.EffectiveVersion() >= version {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("version %d not effective within %v (effective=%d)", version, timeout, e.EffectiveVersion())
}

func TestImportWithoutLexicon(t *testing.T) {
	e := NewEngine()
	defer e.Close()
	e.ImportReviews([]Review{{ID: "r1", Text: "great product"}})
	res, ok := e.GetResult("r1")
	if !ok {
		t.Fatal("imported review must be immediately readable")
	}
	if res.Version != 0 || res.Auto.Category != AutoPending || res.Final != AutoPending {
		t.Fatalf("expected pending-analysis at version 0, got %+v", res)
	}
	if res.NeedsAdjudication {
		t.Fatal("no lexicon: must not be marked needs-adjudication")
	}
	if _, ok := e.GetResult("missing"); ok {
		t.Fatal("unknown review must not be readable")
	}
}

func TestAnalyzeScoring(t *testing.T) {
	lex := map[string]int{"good": 3, "bad": -2, "awful": -5}
	cases := []struct {
		name       string
		text       string
		category   string
		score      int
		confidence int
	}{
		{"repeated words", "good good good", "positive", 9, 100},
		{"cancel out", "good bad", "positive", 1, 20},
		{"net negative", "good awful", "negative", -2, 25},
		{"complete miss", "nothing matches here", "neutral", 0, 0},
		{"separators and case", "GOOD,bad!AWFUL123good", "negative", -1, 7},
		{"empty text", "", "neutral", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Analyze(tc.text, lex)
			if got.Category != tc.category || got.Score != tc.score || got.Confidence != tc.confidence {
				t.Fatalf("Analyze(%q) = %+v, want category=%s score=%d confidence=%d",
					tc.text, got, tc.category, tc.score, tc.confidence)
			}
		})
	}
	if got := Analyze("good", nil); got.Category != AutoPending {
		t.Fatalf("nil lexicon must yield pending-analysis, got %+v", got)
	}
}

func TestSubmitLexiconValidation(t *testing.T) {
	e := NewEngine()
	defer e.Close()
	for _, score := range []int{0, -6, 6, 100} {
		if _, err := e.SubmitLexicon(map[string]int{"x": score}); err != ErrInvalidLexicon {
			t.Fatalf("score %d: expected ErrInvalidLexicon, got %v", score, err)
		}
	}
	v1, err := e.SubmitLexicon(map[string]int{"ok": 1})
	if err != nil || v1 != 1 {
		t.Fatalf("first lexicon: version=%d err=%v", v1, err)
	}
	v2, _ := e.SubmitLexicon(map[string]int{"ok": 2})
	if v2 != 2 {
		t.Fatalf("versions must be strictly increasing, got %d then %d", v1, v2)
	}
}

func TestAdjudicationLifecycle(t *testing.T) {
	e := NewEngine()
	defer e.Close()
	e.ImportReviews([]Review{{ID: "r1", Text: "good"}})
	v, _ := e.SubmitLexicon(map[string]int{"good": 5})
	waitEffective(t, e, v, 5*time.Second)

	if err := e.SetAdjudication("r1", "bogus"); err != ErrInvalidCategory {
		t.Fatalf("invalid category: expected ErrInvalidCategory, got %v", err)
	}
	if err := e.SetAdjudication("r1", CategoryNegative); err != nil {
		t.Fatal(err)
	}
	res, _ := e.GetResult("r1")
	if !res.HasAdjudication || res.Adjudication != CategoryNegative || res.Final != "negative" {
		t.Fatalf("adjudication must override auto result, got %+v", res)
	}
	if res.Auto.Category != "positive" {
		t.Fatalf("auto result must stay computed, got %+v", res.Auto)
	}
	if res.NeedsAdjudication {
		t.Fatal("adjudicated review must not be marked needs-adjudication")
	}

	// 修改裁定
	if err := e.SetAdjudication("r1", CategoryNeutral); err != nil {
		t.Fatal(err)
	}
	res, _ = e.GetResult("r1")
	if res.Final != "neutral" {
		t.Fatalf("modified adjudication must take effect, got %+v", res)
	}

	// 清除后恢复当前生效词典的自动结果
	e.ClearAdjudication("r1")
	res, _ = e.GetResult("r1")
	if res.HasAdjudication || res.Final != res.Auto.Category || res.Final != "positive" {
		t.Fatalf("after clear, final must revert to auto, got %+v", res)
	}
}

func TestNeedsAdjudicationFlag(t *testing.T) {
	e := NewEngine()
	defer e.Close()
	e.ImportReviews([]Review{
		{ID: "low", Text: "good bad"},   // S=1, A=5 → 置信度 20
		{ID: "high", Text: "good good"}, // 置信度 100
	})
	v, _ := e.SubmitLexicon(map[string]int{"good": 3, "bad": -2})
	waitEffective(t, e, v, 5*time.Second)
	res, _ := e.GetResult("low")
	if !res.NeedsAdjudication {
		t.Fatalf("confidence %d < 60 must be flagged, got %+v", res.Auto.Confidence, res)
	}
	res, _ = e.GetResult("high")
	if res.NeedsAdjudication {
		t.Fatalf("confidence %d >= 60 must not be flagged", res.Auto.Confidence)
	}
}

// TestConcurrentSubmitAndImport 连续提交多个词典且同时持续导入：
// 每次读取按其生效版本独立重算必须一致，最终生效版本必须是最后提交的版本。
func TestConcurrentSubmitAndImport(t *testing.T) {
	e := NewEngine()
	defer e.Close()

	texts := map[string]string{}
	var textsMu sync.Mutex
	stop := make(chan struct{})
	var wg sync.WaitGroup

	// 持续导入
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			id := fmt.Sprintf("r%d", i)
			text := fmt.Sprintf("alpha beta gamma review number %d good bad", i)
			e.ImportReviews([]Review{{ID: id, Text: text}})
			textsMu.Lock()
			texts[id] = text
			textsMu.Unlock()
		}
	}()

	// 持续读取并按生效版本独立重算校验
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			textsMu.Lock()
			var id, text string
			for id, text = range texts {
				break
			}
			textsMu.Unlock()
			if id == "" {
				continue
			}
			res, ok := e.GetResult(id)
			if !ok {
				t.Errorf("imported review %s disappeared", id)
				return
			}
			lex, ok := e.Lexicon(res.Version)
			if res.Version > 0 && !ok {
				t.Errorf("effective version %d has no lexicon", res.Version)
				return
			}
			if want := Analyze(text, lex); want != res.Auto {
				t.Errorf("review %s version %d: auto=%+v, independent recompute=%+v",
					id, res.Version, res.Auto, want)
				return
			}
		}
	}()

	// 连续提交 5 个词典
	var lastVersion uint64
	for i := 1; i <= 5; i++ {
		v, err := e.SubmitLexicon(map[string]int{"good": i, "bad": -i, "alpha": 1})
		if err != nil {
			t.Fatal(err)
		}
		lastVersion = v
		time.Sleep(20 * time.Millisecond)
	}
	close(stop)
	wg.Wait()

	waitEffective(t, e, lastVersion, 10*time.Second)
	if got := e.EffectiveVersion(); got != lastVersion {
		t.Fatalf("final effective version = %d, want last submitted %d", got, lastVersion)
	}

	// 全部成功导入的评论均可查询，且与最后词典的重算一致
	lex, _ := e.Lexicon(lastVersion)
	textsMu.Lock()
	defer textsMu.Unlock()
	if len(texts) == 0 {
		t.Fatal("expected some imports")
	}
	for id, text := range texts {
		res, ok := e.GetResult(id)
		if !ok {
			t.Fatalf("imported review %s not queryable", id)
		}
		if res.Version != lastVersion {
			t.Fatalf("review %s at version %d, want %d", id, res.Version, lastVersion)
		}
		if want := Analyze(text, lex); want != res.Auto {
			t.Fatalf("review %s: auto=%+v, recompute=%+v", id, res.Auto, want)
		}
	}
}

// TestAdjudicationSurvivesRecompute 重算期间的裁定不得被后台覆盖，
// 清除后必须使用读取时的生效词典。
func TestAdjudicationSurvivesRecompute(t *testing.T) {
	e := NewEngine()
	defer e.Close()
	e.ImportReviews([]Review{{ID: "r1", Text: "good"}})

	v1, _ := e.SubmitLexicon(map[string]int{"good": 5})
	waitEffective(t, e, v1, 5*time.Second)
	if err := e.SetAdjudication("r1", CategoryNegative); err != nil {
		t.Fatal(err)
	}

	// 触发重算；裁定必须保留，自动结果仍须更新
	v2, _ := e.SubmitLexicon(map[string]int{"good": -4})
	waitEffective(t, e, v2, 5*time.Second)
	res, _ := e.GetResult("r1")
	if !res.HasAdjudication || res.Adjudication != CategoryNegative || res.Final != "negative" {
		t.Fatalf("adjudication must survive recompute, got %+v", res)
	}
	if res.Version != v2 || res.Auto.Category != "negative" || res.Auto.Score != -4 {
		t.Fatalf("auto result must be recomputed under v%d, got %+v", v2, res)
	}

	// 清除裁定后立即使用当前生效词典（v2）的自动结果
	e.ClearAdjudication("r1")
	res, _ = e.GetResult("r1")
	if res.HasAdjudication || res.Final != "negative" || res.Version != v2 {
		t.Fatalf("after clear, final must use effective version %d auto result, got %+v", v2, res)
	}
}
