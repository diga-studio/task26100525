package reviewforge

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// waitEffective 等待生效版本达到 v，超时失败。
func waitEffective(t *testing.T, e *Engine, v uint64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		e.mu.RLock()
		eff := e.effective
		e.mu.RUnlock()
		if eff >= v {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("version %d not effective within %v", v, timeout)
}

// TestNoDictionary 无词典时导入，自动结果为 pending-analysis。
func TestNoDictionary(t *testing.T) {
	e := New()
	defer e.Close()
	e.ImportComments([]Comment{{ID: "c1", Text: "great product"}})
	res, ok := e.Get("c1")
	if !ok {
		t.Fatal("comment not found")
	}
	if res.Version != 0 || res.Auto.Sentiment != SentimentPending || res.Auto.Confidence != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.Final != SentimentPending {
		t.Fatalf("final should be pending, got %v", res.Final)
	}
	if res.NeedsReview {
		t.Fatal("pending result should not need review")
	}
}

// TestAnalyze 覆盖重复词、正负抵消、完全未命中与大小写/分隔符。
func TestAnalyze(t *testing.T) {
	dict := map[string]int{"good": 3, "bad": -2, "love": 5}

	// 重复词每次都计分
	s, a := analyze("good good GOOD", dict)
	if s != 9 || a != 9 {
		t.Fatalf("repeat: got S=%d A=%d", s, a)
	}
	// 正负抵消：2*good(3) + 3*bad(-2) = 0，S=0 为 Neutral，置信度为 0
	s, a = analyze("good good bad bad bad", dict)
	if s != 0 {
		t.Fatalf("cancel: got S=%d A=%d", s, a)
	}
	r := autoFrom(s, a)
	if r.Sentiment != SentimentNeutral || r.Confidence != 0 {
		t.Fatalf("cancel result: %+v", r)
	}
	// 完全未命中
	s, a = analyze("zzz qqq 123 !", dict)
	if s != 0 || a != 0 {
		t.Fatalf("miss: got S=%d A=%d", s, a)
	}
	r = autoFrom(s, a)
	if r.Sentiment != SentimentNeutral || r.Confidence != 0 {
		t.Fatalf("miss result: %+v", r)
	}
	// 分隔符与大小写
	s, a = analyze("Love!bad...GOOD-bad", dict)
	if s != 4 || a != 12 {
		t.Fatalf("separators: got S=%d A=%d", s, a)
	}
	// 置信度向下取整：S=1*5-2=3? 用 good(3) bad(-2): S=1, A=5 -> 20
	s, a = analyze("good bad", dict)
	r = autoFrom(s, a)
	if r.Sentiment != SentimentPositive || r.Confidence != 20 {
		t.Fatalf("confidence floor: %+v", r)
	}
}

// TestDictionarySwitch 提交词典后历史评论被重算，新版本一次生效。
func TestDictionarySwitch(t *testing.T) {
	e := New()
	defer e.Close()
	e.ImportComments([]Comment{
		{ID: "a", Text: "good good"},
		{ID: "b", Text: "bad"},
	})
	v1 := e.SubmitDictionary(map[string]int{"good": 2, "bad": -3})
	if v1 != 1 {
		t.Fatalf("first version should be 1, got %d", v1)
	}
	waitEffective(t, e, v1, 5*time.Second)

	res, _ := e.Get("a")
	if res.Version != 1 || res.Auto.Sentiment != SentimentPositive || res.Auto.Confidence != 100 {
		t.Fatalf("a: %+v", res)
	}
	res, _ = e.Get("b")
	if res.Auto.Sentiment != SentimentNegative || res.Auto.Confidence != 100 {
		t.Fatalf("b: %+v", res)
	}

	// 第二版词典翻转分值
	v2 := e.SubmitDictionary(map[string]int{"good": -1, "bad": 4})
	waitEffective(t, e, v2, 5*time.Second)
	res, _ = e.Get("a")
	if res.Version != 2 || res.Auto.Sentiment != SentimentNegative {
		t.Fatalf("a after v2: %+v", res)
	}
	res, _ = e.Get("b")
	if res.Auto.Sentiment != SentimentPositive {
		t.Fatalf("b after v2: %+v", res)
	}
}

// TestManualOverride 重算期间设置、修改、清除人工裁定。
func TestManualOverride(t *testing.T) {
	e := New()
	defer e.Close()
	e.ImportComments([]Comment{{ID: "x", Text: "good"}})

	// 重算期间（词典已提交未生效）设置裁定
	v1 := e.SubmitDictionary(map[string]int{"good": 5})
	if !e.SetManual("x", SentimentNegative) {
		t.Fatal("SetManual failed")
	}
	waitEffective(t, e, v1, 5*time.Second)
	res, _ := e.Get("x")
	if res.Manual == nil || *res.Manual != SentimentNegative {
		t.Fatalf("manual lost after recompute: %+v", res)
	}
	if res.Final != SentimentNegative {
		t.Fatalf("final should follow manual: %+v", res)
	}
	if res.Auto.Sentiment != SentimentPositive {
		t.Fatalf("auto should still update: %+v", res)
	}
	if res.NeedsReview {
		t.Fatal("manual set, should not need review")
	}

	// 修改裁定
	if !e.SetManual("x", SentimentNeutral) {
		t.Fatal("SetManual modify failed")
	}
	res, _ = e.Get("x")
	if res.Final != SentimentNeutral {
		t.Fatalf("final after modify: %+v", res)
	}

	// 清除裁定：最终情感恢复为生效词典的自动结果
	if !e.ClearManual("x") {
		t.Fatal("ClearManual failed")
	}
	res, _ = e.Get("x")
	if res.Manual != nil || res.Final != SentimentPositive {
		t.Fatalf("after clear: %+v", res)
	}

	// 非法类别与不存在评论
	if e.SetManual("x", SentimentPending) {
		t.Fatal("pending is not a valid manual sentiment")
	}
	if e.SetManual("nope", SentimentPositive) {
		t.Fatal("unknown comment should fail")
	}
	if e.ClearManual("nope") {
		t.Fatal("unknown comment clear should fail")
	}
}

// TestNeedsReview 置信度低于 60 且无人工裁定时标记待裁定。
func TestNeedsReview(t *testing.T) {
	e := New()
	defer e.Close()
	e.ImportComments([]Comment{
		{ID: "low", Text: "good bad"},  // S=1 A=5 -> 20
		{ID: "high", Text: "good good"}, // 100
	})
	v := e.SubmitDictionary(map[string]int{"good": 3, "bad": -2})
	waitEffective(t, e, v, 5*time.Second)

	res, _ := e.Get("low")
	if !res.NeedsReview || res.Auto.Confidence != 20 {
		t.Fatalf("low: %+v", res)
	}
	res, _ = e.Get("high")
	if res.NeedsReview {
		t.Fatalf("high: %+v", res)
	}
	// 设置裁定后不再待裁定
	e.SetManual("low", SentimentPositive)
	res, _ = e.Get("low")
	if res.NeedsReview {
		t.Fatalf("low with manual: %+v", res)
	}
}

// TestContinuousSubmitAndImport 连续提交多个词典且同时持续导入：
// 每次读取按其生效版本独立重算必须得到相同自动结果；
// 最后生效的必须是最后提交的版本；全部成功导入的评论均可查询。
func TestContinuousSubmitAndImport(t *testing.T) {
	e := New()
	defer e.Close()

	dicts := []map[string]int{
		{"alpha": 1, "beta": -1},
		{"alpha": 5, "beta": -5, "gamma": 2},
		{"alpha": -3, "gamma": 4, "delta": -2},
		{"beta": 3, "delta": 5, "alpha": 2, "gamma": -4},
	}
	texts := []string{
		"alpha alpha beta", "gamma delta!", "beta...delta alpha",
		"nothing here", "ALPHA,gamma;delta",
	}

	var mu sync.Mutex
	imported := make(map[string]string)
	stop := make(chan struct{})
	var wg sync.WaitGroup

	// 持续导入
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			id := fmt.Sprintf("c%d", i)
			text := texts[i%len(texts)]
			e.ImportComments([]Comment{{ID: id, Text: text}})
			mu.Lock()
			imported[id] = text
			mu.Unlock()
			i++
			time.Sleep(time.Millisecond)
		}
	}()

	// 并发读取一致性校验：按返回版本独立重算
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			mu.Lock()
			ids := make([]string, 0, len(imported))
			for id := range imported {
				ids = append(ids, id)
			}
			mu.Unlock()
			if len(ids) == 0 {
				time.Sleep(time.Millisecond)
				continue
			}
			id := ids[i%len(ids)]
			res, ok := e.Get(id)
			if !ok {
				continue
			}
			mu.Lock()
			text := imported[id]
			mu.Unlock()
			if res.Version == 0 {
				if res.Auto.Sentiment != SentimentPending {
					t.Errorf("v0 should be pending: %+v", res)
				}
				continue
			}
			dict, ok := e.DictForVersion(res.Version)
			if !ok {
				t.Errorf("dict for version %d missing", res.Version)
				continue
			}
			want := autoFrom(analyze(text, dict))
			if res.Auto != want {
				t.Errorf("comment %s version %d: got %+v want %+v", id, res.Version, res.Auto, want)
			}
		}
	}()

	// 连续提交词典
	var lastV uint64
	for _, d := range dicts {
		lastV = e.SubmitDictionary(d)
		time.Sleep(20 * time.Millisecond)
	}
	waitEffective(t, e, lastV, 10*time.Second)
	close(stop)
	wg.Wait()

	// 最终生效版本必须是最后提交的版本
	e.mu.RLock()
	eff := e.effective
	e.mu.RUnlock()
	if eff != lastV {
		t.Fatalf("effective=%d want %d", eff, lastV)
	}

	// 全部成功导入的评论均可查询，且自动结果与最终词典一致
	lastDict := dicts[len(dicts)-1]
	mu.Lock()
	defer mu.Unlock()
	if len(imported) == 0 {
		t.Fatal("nothing imported")
	}
	for id, text := range imported {
		res, ok := e.Get(id)
		if !ok {
			t.Fatalf("imported comment %s not queryable", id)
		}
		if res.Version != lastV {
			t.Fatalf("comment %s version %d, want %d", id, res.Version, lastV)
		}
		want := autoFrom(analyze(text, lastDict))
		if res.Auto != want {
			t.Fatalf("comment %s: got %+v want %+v", id, res.Auto, want)
		}
	}
}
