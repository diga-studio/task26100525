package reviewforge

import (
	"errors"
	"sync"
)

// Review 是一条待导入的评论。
type Review struct {
	ID   string
	Text string
}

// ReviewResult 是单次读取返回的一致快照。
type ReviewResult struct {
	ReviewID          string
	Version           uint64 // 生效词典版本，0 表示尚无词典
	Auto              AutoResult
	Adjudication      Category
	HasAdjudication   bool
	Final             string // 最终情感：裁定优先，否则自动结果
	NeedsAdjudication bool
}

// ErrInvalidCategory 表示裁定不是三种情感类别之一。
var ErrInvalidCategory = errors.New("reviewforge: adjudication must be positive, negative or neutral")

// ErrInvalidLexicon 表示词典包含非法分值。
var ErrInvalidLexicon = errors.New("reviewforge: lexicon scores must be non-zero integers in [-5, 5]")

// Engine 是本地情感判读引擎，数据保存在进程内存中，并发安全。
type Engine struct {
	mu            sync.Mutex
	cond          *sync.Cond
	reviews       map[string]string         // 全部已导入评论，导入后正文不变
	lexicons      map[uint64]map[string]int // 各已接受版本的词典
	latest        uint64                    // 最新接受的版本号
	effective     uint64                    // 当前生效版本，0 表示尚无词典
	results       map[string]AutoResult     // 生效版本下每条评论的自动结果
	adjudications map[string]Category
	closed        bool
	workerDone    chan struct{}
}

// NewEngine 创建一个空引擎并启动后台重算协程。
func NewEngine() *Engine {
	e := &Engine{
		reviews:       make(map[string]string),
		lexicons:      make(map[uint64]map[string]int),
		results:       make(map[string]AutoResult),
		adjudications: make(map[string]Category),
		workerDone:    make(chan struct{}),
	}
	e.cond = sync.NewCond(&e.mu)
	go e.recomputeLoop()
	return e
}

// Close 停止后台重算协程。
func (e *Engine) Close() {
	e.mu.Lock()
	e.closed = true
	e.cond.Signal()
	e.mu.Unlock()
	<-e.workerDone
}

// ImportReviews 批量导入评论；调用方保证 ID 不重复。
// 导入成功后立即可查，自动结果使用当时的生效词典。
func (e *Engine) ImportReviews(reviews []Review) {
	e.mu.Lock()
	defer e.mu.Unlock()
	lexicon := e.lexicons[e.effective]
	for _, r := range reviews {
		e.reviews[r.ID] = r.Text
		e.results[r.ID] = Analyze(r.Text, lexicon)
	}
}

// SubmitLexicon 提交完整词典，返回严格递增的版本号，不等待历史重算完成。
func (e *Engine) SubmitLexicon(entries map[string]int) (uint64, error) {
	copied := make(map[string]int, len(entries))
	for word, score := range entries {
		if score == 0 || score < -5 || score > 5 {
			return 0, ErrInvalidLexicon
		}
		copied[word] = score
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.latest++
	e.lexicons[e.latest] = copied
	e.cond.Signal()
	return e.latest, nil
}

// SetAdjudication 设置或修改人工裁定；裁定只接受三种情感类别。
func (e *Engine) SetAdjudication(id string, c Category) error {
	switch c {
	case CategoryPositive, CategoryNegative, CategoryNeutral:
	default:
		return ErrInvalidCategory
	}
	e.mu.Lock()
	e.adjudications[id] = c
	e.mu.Unlock()
	return nil
}

// ClearAdjudication 清除人工裁定；最终情感立即恢复为当前生效词典的自动结果。
func (e *Engine) ClearAdjudication(id string) {
	e.mu.Lock()
	delete(e.adjudications, id)
	e.mu.Unlock()
}

// GetResult 读取指定评论的结果；版本、自动结果与裁定取自同一可见时刻。
func (e *Engine) GetResult(id string) (ReviewResult, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.reviews[id]; !ok {
		return ReviewResult{}, false
	}
	res := ReviewResult{
		ReviewID: id,
		Version:  e.effective,
		Auto:     e.results[id],
	}
	if c, ok := e.adjudications[id]; ok {
		res.Adjudication = c
		res.HasAdjudication = true
		res.Final = string(c)
	} else {
		res.Final = res.Auto.Category
		res.NeedsAdjudication = e.effective > 0 && res.Auto.Confidence < 60
	}
	return res, true
}

// EffectiveVersion 返回当前生效词典版本。
func (e *Engine) EffectiveVersion() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.effective
}

// Lexicon 返回指定版本词典的副本，供调用方独立重算校验。
func (e *Engine) Lexicon(version uint64) (map[string]int, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	src, ok := e.lexicons[version]
	if !ok {
		return nil, false
	}
	copied := make(map[string]int, len(src))
	for word, score := range src {
		copied[word] = score
	}
	return copied, true
}

// recomputeLoop 是后台重算协程：始终朝最新接受的版本推进，
// 尚未生效的旧版本在更高版本被接受后直接跳过。
func (e *Engine) recomputeLoop() {
	defer close(e.workerDone)
	for {
		e.mu.Lock()
		for !e.closed && e.latest <= e.effective {
			e.cond.Wait()
		}
		if e.closed {
			e.mu.Unlock()
			return
		}
		target := e.latest
		lexicon := e.lexicons[target]
		reviewCount := len(e.reviews)
		e.mu.Unlock()

		results := make(map[string]AutoResult, reviewCount)
		for {
			e.mu.Lock()
			var pending []Review
			for id, text := range e.reviews {
				if _, done := results[id]; !done {
					pending = append(pending, Review{ID: id, Text: text})
				}
			}
			if len(pending) == 0 {
				// 持锁状态下没有遗漏的评论：一次生效，覆盖重算期间的新增。
				if target > e.effective {
					e.effective = target
					e.results = results
				}
				e.mu.Unlock()
				break
			}
			e.mu.Unlock()
			// 锁外判读，不阻塞导入、读取与裁定。
			for _, r := range pending {
				results[r.ID] = Analyze(r.Text, lexicon)
			}
		}
	}
}
