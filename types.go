package reviewforge

// Sentiment 是情感类别。
type Sentiment string

const (
	SentimentPositive Sentiment = "Positive"
	SentimentNegative Sentiment = "Negative"
	SentimentNeutral  Sentiment = "Neutral"
	// SentimentPending 表示尚无生效词典，自动结果待分析。
	SentimentPending Sentiment = "pending-analysis"
)

// Comment 是一条待导入的评论，只含唯一 ID 和正文。
type Comment struct {
	ID   string
	Text string
}

// AutoResult 是词典对一条评论的自动判读结果。
type AutoResult struct {
	Sentiment  Sentiment
	Confidence int // 0-100
}

// CommentResult 是一次读取返回的完整快照。
type CommentResult struct {
	Version     uint64     // 生效词典版本，0 表示尚无生效词典
	Auto        AutoResult // 生效词典下的自动结果
	Manual      *Sentiment // 人工裁定，nil 表示无
	Final       Sentiment  // 最终情感：人工裁定优先，否则自动结果
	NeedsReview bool       // 自动结果存在、置信度低于 60 且无人工裁定
}
