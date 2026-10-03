package reviewforge

// Category 是三种情感类别之一。
type Category string

const (
	CategoryPositive Category = "positive"
	CategoryNegative Category = "negative"
	CategoryNeutral  Category = "neutral"
)

// AutoPending 表示尚无生效词典时的自动结果。
const AutoPending = "pending-analysis"

// AutoResult 是一次自动判读的结果。
type AutoResult struct {
	Category   string // positive/negative/neutral，或 AutoPending
	Score      int    // 命中分值总和 S
	Confidence int    // 100*|S|/A 向下取整；A 为 0 时为 0
}

// Analyze 用给定词典对正文判读。lexicon 为 nil 表示无生效词典。
func Analyze(text string, lexicon map[string]int) AutoResult {
	if lexicon == nil {
		return AutoResult{Category: AutoPending}
	}
	sum := 0
	absSum := 0
	forEachWord(text, func(w string) {
		if score, ok := lexicon[w]; ok {
			sum += score
			if score < 0 {
				absSum -= score
			} else {
				absSum += score
			}
		}
	})
	res := AutoResult{Score: sum}
	switch {
	case sum > 0:
		res.Category = string(CategoryPositive)
	case sum < 0:
		res.Category = string(CategoryNegative)
	default:
		res.Category = string(CategoryNeutral)
	}
	if absSum > 0 {
		absVal := sum
		if absVal < 0 {
			absVal = -absVal
		}
		res.Confidence = 100 * absVal / absSum
	}
	return res
}

// forEachWord 对正文中每段连续 ASCII 字母回调其小写形式，其余字符视为分隔符。
func forEachWord(text string, fn func(string)) {
	start := -1
	flush := func(end int) {
		if start >= 0 {
			buf := make([]byte, end-start)
			for i := start; i < end; i++ {
				c := text[i]
				if c >= 'A' && c <= 'Z' {
					c += 'a' - 'A'
				}
				buf[i-start] = c
			}
			fn(string(buf))
			start = -1
		}
	}
	for i := 0; i < len(text); i++ {
		c := text[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			if start < 0 {
				start = i
			}
		} else {
			flush(i)
		}
	}
	flush(len(text))
}
