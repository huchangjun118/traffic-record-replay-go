# traffic-record-replay-go

进程内 HTTP 流量录制与确定性回放。整条链路只依赖本进程（录制侧的底层
`RoundTripper` 由调用方提供，测试中可用 `httptest`），回放模式**绝不**
发起真实网络访问。

## 快速开始

```go
// 录制：真实请求透传并落盘
store := replay.NewStore("my-case", nil, masker, nil)
client := &http.Client{Transport: replay.NewTransport(replay.ModeRecord, store, nil)}
client.Get("https://api.example.com/users")
store.SaveTo("cassette.json") // 原子落盘

// 回放：只命中本地记录
loaded, _ := replay.LoadFrom("cassette.json", nil, nil)
client = &http.Client{Transport: replay.NewTransport(replay.ModeReplay, loaded, nil)}
client.Get("https://api.example.com/users") // 不发任何真实请求
```

模式必须显式给出（`ModeRecord` / `ModeReplay`，或 `ParseMode("record"|"replay")`）。
回放未命中时返回 `*ErrNoMatch`（包含方法与 URL），不透传、不编造响应。

## 录制文件格式

落盘文件为 JSON 信封，带版本与校验和：

```json
{
  "version": 1,
  "checksum": "<对 payload 字节的 SHA-256（hex）>",
  "payload": {
    "version": 1,
    "name": "my-case",
    "interactions": [
      {
        "id": 1,
        "request":  {"method": "GET", "url": "...", "header": {...}, "body": "<base64>"},
        "response": {"status_code": 200, "header": {...}, "body": "<base64>"}
      }
    ]
  }
}
```

- **原子写**：先写同目录临时文件并 fsync，再 rename。写入中途失败或进程被杀
  只会残留 `.cassette-*.tmp`，目标文件要么整体可用、要么保持旧内容。
- **加载校验**：JSON 不合法、payload 为空、校验和不符、结构字段缺失 →
  `*ErrCorruptCassette`（`replay.IsCorrupt(err)` 判定）；
  版本超出 `[MinSupportedVersion, FormatVersion]` → `*ErrUnsupportedVersion`
  （`replay.IsUnsupportedVersion(err)` 判定）。两类错误可区分，加载失败
  不会返回部分可用的数据。
- **版本演进**：当前 `FormatVersion = 1`，`MinSupportedVersion = 1`。
  声明兼容区间内的旧版本记录可直接回放；未来升级格式时在加载处做迁移。

## 匹配规则

匹配基于**匹配键**，由 `MatchConfig` 控制：

| 配置项 | 默认 | 说明 |
| --- | --- | --- |
| `MatchBody` | false | 是否将请求体纳入匹配 |
| `MatchHeaders` | 空 | 需要参与匹配的头部名（大小写不敏感） |
| `IgnoreQueryOrder` | true | 查询参数按键排序归一 |
| `NormalizeBodyWhitespace` | false | JSON 请求体重编码为紧凑形式（键序确定） |

方法（大小写归一）、scheme/host（小写归一）、路径、查询参数始终参与匹配。
头部名大小写、查询参数顺序、JSON 空白等不改变语义的差异会被归一。

**选取规则（确定性）**：匹配键相同的候选中，选被消费次数最少者；并列时选
`id` 最小（录制最早）者，选中后消费计数加一。因此：

- 同一请求重复出现 → 按录制顺序轮询各条记录；
- 请求到达顺序变化 / 并发交错 → 每个键的选取序列只取决于该键下的请求次数，
  与其他键无关，重复运行与不同并发度下结果逐字节一致。

匹配不上 → `*ErrNoMatch`；候选多条时按上述规则裁决，无歧义分支。

## 脱敏

`MaskConfig` 在**写入存储前**生效，落盘内容不含真实凭据：

```go
masker := replay.NewMasker(replay.MaskConfig{
    Headers:    []string{"Authorization", "Cookie"}, // 头部（大小写不敏感）
    BodyFields: []string{"password", "token"},       // JSON 顶层字段
    Replacement: "***",                               // 缺省 "***"
})
```

回放只读已脱敏的记录，全程不接触真实凭据。

## 并发与取消

- 每个用例持有独立 `Store`，实例间零共享，天然隔离；实例内部互斥锁保护，
  读写一致，慢用例不阻塞其他实例。
- 查找经匹配键索引（哈希表），不随记录条数线性退化
  （2 万条记录下约 1µs/次，见 `BenchmarkLookupLargeCassette`）。
- `Lookup` 与回放 `RoundTrip` 都接受 `context.Context`；取消立即返回
  `context.Canceled`，不消费记录、不留下半成品状态。

## 可观测指标

`store.Metrics().Hits()` / `.Misses()` 返回累计命中/未命中次数（原子计数）。

## 测试

```sh
go test ./replay/ -race          # 全部测试（含并发）
go test ./replay/ -bench Lookup  # 查找性能基准
```

覆盖：模式切换、匹配归一化与歧义裁决、重复请求轮询、脱敏落盘、
格式兼容与损坏/版本拒绝、原子写恢复、并发隔离与确定性、取消。
