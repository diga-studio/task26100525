package reviewforge

// analyze 用给定词典对正文判读：正文中连续 ASCII 字母构成一个词，
// 统一转小写，其余字符为分隔符。返回命中分值总和 S 与命中绝对值之和 A。
func analyze(text string, dict map[string]int) (sum int, absSum int) {
	var word [64]byte
	n := 0
	overflow := false
	flush := func() {
		if n > 0 && !overflow {
			if v, ok := dict[string(word[:n])]; ok {
				sum += v
				if v < 0 {
					absSum -= v
				} else {
					absSum += v
				}
			}
		}
		n = 0
		overflow = false
	}
	for i := 0; i < len(text); i++ {
		c := text[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if 'a' <= c && c <= 'z' {
			if n < len(word) {
				word[n] = c
				n++
			} else {
				// 超长词不可能命中词典，忽略其剩余字母
				overflow = true
			}
			continue
		}
		flush()
	}
	flush()
	return sum, absSum
}

// autoFrom 由 S/A 计算自动结果：S 正负零对应三种情感；
// 置信度为 100*|S|/A 向下取整，A 为 0 时置信度为 0。
func autoFrom(sum, absSum int) AutoResult {
	var s Sentiment
	switch {
	case sum > 0:
		s = SentimentPositive
	case sum < 0:
		s = SentimentNegative
	default:
		s = SentimentNeutral
	}
	conf := 0
	if absSum > 0 {
		abs := sum
		if abs < 0 {
			abs = -abs
		}
		conf = abs * 100 / absSum
	}
	return AutoResult{Sentiment: s, Confidence: conf}
}
