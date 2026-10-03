# ReviewForge 本地情感判读模块

进程内情感判读库（Go），供上层直接调用，无外部服务依赖。

## API

```go
e := reviewforge.New()
defer e.Close()

e.ImportComments([]reviewforge.Comment{{ID: "c1", Text: "good bad"}})
v := e.SubmitDictionary(map[string]int{"good": 3, "bad": -2}) // 立即返回版本号，后台重算
e.SetManual("c1", reviewforge.SentimentPositive)
e.ClearManual("c1")
res, ok := e.Get("c1") // 同一可见时刻的一致快照：版本/自动/人工/最终情感
```

## 语义

- 分词：连续 ASCII 字母成词并转小写，其余为分隔符；每次出现都计分。
- 自动结果：命中总分 S 定正负零（Positive/Negative/Neutral），置信度为
  `100*|S|/A` 向下取整（A 为命中绝对值之和，A=0 时为 0）。
- 版本 0 表示无生效词典，自动结果为 `pending-analysis`。
- 词典提交不阻塞；后台始终重算最新版本，完成后对所有可见评论一次生效，
  旧版本不会再生效，重算期间新增的评论也会被覆盖。
- 人工裁定优先于自动结果，重算不改写裁定；清除后恢复为生效词典的自动结果。
- 自动结果存在、置信度 < 60 且无人工裁定时 `NeedsReview=true`。

## 测试

```sh
go test ./...        # 正确性 + 规模并发测试（报告分位数与生效耗时）
go test -short ./... # 跳过规模测试
go test -race ./...  # 竞态检测
```
