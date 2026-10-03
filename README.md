# httptape — 进程内 HTTP 流量录制与确定性回放

`httptape` 是一个纯进程内的 HTTP 流量录制/回放库，以 `http.RoundTripper`
方式接入，不依赖任何真实外部服务：录制时把经过的请求与响应完整落盘，
回放时完全不发起真实网络访问。适用于让测试稳定复现外部调用、在线问题
本地重放等场景。

## 特性

- **显式模式切换**：`ModeRecord` / `ModeReplay`，同一会话先录后放。
- **零真实网络访问**：回放 `RoundTripper` 不调用任何底层传输（即使配置了
  `Transport` 也不会被触达），命中不到时按策略明确失败，不透传、不编造。
- **确定性回放**：匹配键为方法/路径/查询/头/体的归一化哈希，重复运行与
  不同并发度下返回的状态、头部、主体逐字节一致。
- **可配置匹配规则**：方法、路径、查询参数（顺序归一）、请求头（大小写
  归一、空白归一）、请求体（空白/JSON 语义归一），支持白名单。
- **多记录裁决确定可复现**：`PolicyFirst` 恒取最先录制的记录（与到达顺序
  无关）；`PolicyRoundRobin` 按录制顺序循环选取。
- **落盘安全**：临时文件 + `fsync` + 原子 rename，崩溃/被杀不产生半截
  最终文件；文件带 SHA256 校验和，损坏在加载阶段即被拒绝。
- **版本化格式**：文件头带版本魔数，结构损坏返回 `ErrCorrupted`，版本不
  支持返回 `ErrUnsupportedVersion`，错误可区分、不崩溃。
- **脱敏**：敏感请求/响应头与 JSON 字段在写入前脱敏，回放不依赖真实凭据。
- **并发隔离**：会话即文件，不同用例使用不同会话名，各自记录不串扰；
  录制互斥锁、回放读写锁，慢用例不阻塞其他用例。
- **性能**：哈希索引 O(1) 查找，不随记录条数线性退化；提供命中/未命中
  统计；回放尊重 `context.Context` 取消。

## 安装

```bash
go get github.com/huchangjun118/traffic-record-replay-go
```

## 快速开始

```go
import "github.com/huchangjun118/traffic-record-replay-go"

// 录制
rec, err := httptape.New(httptape.Config{
    Mode:        httptape.ModeRecord,
    StoragePath: "./tapes",
    Session:     "user-service",
})
defer rec.Close()
client := rec.Client() // 或把 rec 作为 http.Client.Transport
resp, err := client.Get("https://api.example.com/users/1")

// 回放（同一会话名，完全不触达真实网络）
replayer, err := httptape.New(httptape.Config{
    Mode:        httptape.ModeReplay,
    StoragePath: "./tapes",
    Session:     "user-service",
})
defer replayer.Close()
resp, err := replayer.Client().Get("https://api.example.com/users/1")
```

## 录制格式

录制文件位于 `<StoragePath>/<Session>.tape`，采用行分隔 JSON：

```
TRTAPE1
{"seq":1,"recorded_at":"...","request":{...},"response":{...}}
{"seq":2,"recorded_at":"...","request":{...},"response":{...}}
{"type":"commit","records":2,"checksum":"<sha256 hex>"}
```

- 第 1 行为版本魔数 `TRTAPE1`。
- 第 2 行起为记录，每行一条 JSON，包含序号、录制时间、请求（方法、URL、
  头、体）与响应（状态码、状态、头、体）。
- 最后一行为提交标记：`records` 为记录条数，`checksum` 为所有记录行
  字节（含行尾换行）的 SHA256 十六进制。

### 原子落盘与崩溃恢复

1. 录制时先写入同目录下的隐藏临时文件
   `.<Session>.tape.tmp-<random>`，数据缓冲写入。
2. `Close()` 时写入提交标记、`fsync`，再原子 rename 为最终文件。
3. 进程在任意时刻被杀：
   - 最终文件要么不存在，要么是完整可用的旧/新文件（rename 原子）；
   - 临时文件残留会被自动忽略，可手动删除。
4. `Abort()` 主动放弃录制，关闭并删除临时文件，不产生最终文件。

### 加载校验

加载时依次校验，失败返回可区分错误：

| 情况 | 错误 |
| --- | --- |
| 魔数不是 `TRTAPE` 前缀（如普通文本） | `ErrCorrupted` |
| 魔数为 `TRTAPE2` 等不支持版本 | `ErrUnsupportedVersion` |
| 缺少提交标记（写入中途崩溃） | `ErrCorrupted` |
| 记录条数与提交标记不符 | `ErrCorrupted` |
| 校验和不符（内容被篡改/损坏） | `ErrCorrupted` |
| 某行 JSON 无法解析 | `ErrCorrupted` |

未来格式演进时，新版本使用新魔数；旧版本在声明兼容后可在 `parseTape`
中增加对应解析分支，不兼容则返回 `ErrUnsupportedVersion`。

## 匹配规则

回放时对入站请求与录制请求分别计算归一化哈希键，键相同即匹配。匹配
规则由 `MatchConfig` 配置：

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `Method` | `true` | 方法（转大写） |
| `Path` | `true` | 路径 |
| `Query` | `true` | 查询参数，按键排序、多值排序，与顺序无关 |
| `Headers` | `false` | 是否纳入请求头 |
| `Body` | `false` | 是否纳入请求体 |
| `QueryParams` | 空 | 非空时只匹配这些查询参数（白名单） |
| `HeaderNames` | 空 | 非空时只匹配这些请求头（白名单，大小写不敏感） |
| `NormalizeHeaderWhitespace` | `false` | 归一化请求头值两端空白 |
| `NormalizeBodyWhitespace` | `false` | 归一化请求体空白：JSON 做 `json.Compact`，其它折叠连续空白 |

默认规则为 **方法 + 路径 + 查询参数**。以下差异不改变语义，会被归一化：

- 查询参数顺序：`?a=1&b=2` 与 `?b=2&a=1` 同键；
- 头部大小写：`X-Foo` 与 `x-foo` 同键；
- 头部空白（开启归一化）：`"bar"` 与 `" bar "` 同键；
- 请求体空白（开启归一化）：`{"x":  1}` 与 `{"x":1}` 同键。

### 多条记录的裁决

同一匹配键对应多条记录时，由 `Selection` 决定：

- `PolicyFirst`（默认）：恒取序号最小（最先录制）的记录。结果与请求
  到达顺序无关，重复请求与并发请求都拿到同一响应。
- `PolicyRoundRobin`：按录制顺序依次选取、循环复用。确定且可复现，
  适合同一接口需要按顺序返回不同响应的场景。

### 未命中策略

由 `Miss` 决定：

- `MissError`（默认）：返回 `ErrNoMatch`（含方法与 URL），不透传、不
  编造。
- `MissStatus`：返回 `MissResponse` 配置的确定性状态码/头/体。

## 配置项

```go
type Config struct {
    Mode         Mode           // ModeRecord / ModeReplay，必填
    StoragePath  string         // 录制文件目录，必填
    Session      string         // 会话名（文件名），不含路径分隔符，必填
    Match        MatchConfig    // 匹配规则，零值用 DefaultMatchConfig
    Sanitize     SanitizeConfig // 脱敏配置
    Selection    SelectionPolicy // PolicyFirst（默认）/ PolicyRoundRobin
    Miss         MissPolicy     // MissError（默认）/ MissStatus
    MissResponse MissResponse   // MissStatus 策略下的确定性响应
    Transport    http.RoundTripper // 仅录制模式使用，默认 http.DefaultTransport
    MaxBodyBytes int            // 单条 body 落盘上限，默认 10 MiB
}
```

### 脱敏

```go
httptape.Config{
    // ...
    Sanitize: httptape.SanitizeConfig{
        RedactHeaders:    []string{"Authorization", "X-Secret"},
        RedactBodyFields: []string{"password", "user.token"}, // JSON 点分路径
        Placeholder:      "***REDACTED***",                  // 可选
    },
}
```

- 脱敏在写入前完成，请求头、响应头、请求体、响应体均会处理。
- 头名大小写不敏感；JSON 字段按点分路径匹配，命中的字段值替换为占位符。
- 回放时对入站请求做同样脱敏后再计算匹配键，因此带真实凭据的请求仍能
  命中脱敏录制，且回放过程不依赖真实凭据。

### 统计

```go
st := replayer.Stats()
// st.Hits    命中数
// st.Misses  未命中数
// st.Records 录制总条数
```

## 并发模型

- **会话隔离**：每个 `Recorder` 绑定一个会话文件；不同用例使用不同
  `Session`，各自文件独立，不串扰。
- **录制**：互斥锁保护临时文件写入，慢用例不阻塞其他用例（各自独立
  锁）。
- **回放**：`sync.RWMutex` 保护索引，读多写少；统计用原子操作。
- **取消**：回放尊重 `req.Context()`，取消时返回 `context.Canceled`，
  不产生半成品状态。

## 注意事项

- 录制会把请求体与响应体完整读入内存后落盘；`MaxBodyBytes` 限制落盘
  前缀大小，超出的记录标记 `body_truncated`，回放命中此类记录时返回
  `ErrBodyTruncated`。
- 录制模式默认使用 `http.DefaultTransport`，可通过 `Transport` 自定义
  （如测试用的 transport）。
- 录制/回放均以 `http.RoundTripper` 接入，`http.Client` 的重定向会逐跳
  经过录制器，因此重定向链路也会被完整录制/回放。
