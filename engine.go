package reviewforge

import "sync"

// commentState 是单条评论的内部状态。
type commentState struct {
	text   string
	auto   AutoResult // 按当前生效词典计算
	manual *Sentiment // 人工裁定，不受重算影响
}

// Engine 是本地情感判读引擎，数据保存在进程内存中，并发安全。
type Engine struct {
	mu        sync.RWMutex
	comments  map[string]*commentState
	dicts     map[uint64]map[string]int // 已接受的词典，按版本保存
	effective uint64                    // 生效词典版本，0 表示尚无
	latest    uint64                    // 最新接受的版本

	notify chan struct{} // 通知后台重算
	done   chan struct{} // 后台 goroutine 退出信号
	once   sync.Once
}

// New 创建空引擎（生效版本为 0，自动结果为 pending-analysis）。
func New() *Engine {
	e := &Engine{
		comments: make(map[string]*commentState),
		dicts:    make(map[uint64]map[string]int),
		notify:   make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
	go e.recomputeLoop()
	return e
}

// Close 停止后台重算 goroutine。
func (e *Engine) Close() {
	e.once.Do(func() { close(e.done) })
}

// ImportComments 批量导入评论，导入成功后立即可查，
// 自动结果使用当时的生效词典；尚无词典则为 pending-analysis。
func (e *Engine) ImportComments(comments []Comment) {
	e.mu.Lock()
	defer e.mu.Unlock()
	dict := e.dicts[e.effective]
	for _, c := range comments {
		st := &commentState{text: c.Text}
		if e.effective == 0 {
			st.auto = AutoResult{Sentiment: SentimentPending}
		} else {
			st.auto = autoFrom(analyze(c.Text, dict))
		}
		e.comments[c.ID] = st
	}
}

// SubmitDictionary 提交完整词典，返回严格递增的版本号。
// 不等待历史重算完成；重算在后台进行，完成后新版本一次生效。
func (e *Engine) SubmitDictionary(dict map[string]int) uint64 {
	e.mu.Lock()
	e.latest++
	v := e.latest
	e.dicts[v] = dict
	e.mu.Unlock()
	select {
	case e.notify <- struct{}{}:
	default:
	}
	return v
}

// SetManual 设置或修改人工裁定，只能选三个情感类别之一。
// 评论不存在或类别非法时返回 false。
func (e *Engine) SetManual(id string, s Sentiment) bool {
	switch s {
	case SentimentPositive, SentimentNegative, SentimentNeutral:
	default:
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	st, ok := e.comments[id]
	if !ok {
		return false
	}
	st.manual = &s
	return true
}

// ClearManual 清除人工裁定，最终情感立即恢复为生效词典的自动结果。
func (e *Engine) ClearManual(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	st, ok := e.comments[id]
	if !ok {
		return false
	}
	st.manual = nil
	return true
}

// Get 读取指定评论在某一可见时刻的一致快照：
// 生效版本、自动结果、人工裁定与最终情感来自同一次加锁。
func (e *Engine) Get(id string) (CommentResult, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	st, ok := e.comments[id]
	if !ok {
		return CommentResult{}, false
	}
	res := CommentResult{
		Version: e.effective,
		Auto:    st.auto,
		Manual:  st.manual,
		Final:   st.auto.Sentiment,
	}
	if st.manual != nil {
		res.Final = *st.manual
	}
	res.NeedsReview = st.manual == nil &&
		st.auto.Sentiment != SentimentPending && st.auto.Confidence < 60
	return res, true
}

// DictForVersion 返回指定版本的词典（供测试按版本独立重算校验）。
func (e *Engine) DictForVersion(v uint64) (map[string]int, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	d, ok := e.dicts[v]
	return d, ok
}

// recomputeLoop 是唯一的后台重算 goroutine：始终重算最新接受的版本，
// 尚未生效的旧版本在更高版本被接受后不再生效。
func (e *Engine) recomputeLoop() {
	for {
		select {
		case <-e.done:
			return
		case <-e.notify:
		}
		for {
			select {
			case <-e.done:
				return
			default:
			}
			e.mu.RLock()
			v := e.latest
			if v <= e.effective {
				e.mu.RUnlock()
				break
			}
			dict := e.dicts[v]
			// 快照当前全部评论正文，不持锁计算
			type item struct {
				id   string
				text string
			}
			items := make([]item, 0, len(e.comments))
			for id, st := range e.comments {
				items = append(items, item{id, st.text})
			}
			e.mu.RUnlock()

			results := make(map[string]AutoResult, len(items))
			for _, it := range items {
				results[it.id] = autoFrom(analyze(it.text, dict))
			}

			e.mu.Lock()
			if v <= e.effective || v != e.latest {
				// 已有更高版本被接受，丢弃本次结果，重算最新版本
				e.mu.Unlock()
				continue
			}
			for id, r := range results {
				if st, ok := e.comments[id]; ok {
					st.auto = r
				}
			}
			// 覆盖重算期间新增的评论
			for id, st := range e.comments {
				if _, ok := results[id]; !ok {
					st.auto = autoFrom(analyze(st.text, dict))
				}
			}
			e.effective = v
			e.mu.Unlock()
		}
	}
}
