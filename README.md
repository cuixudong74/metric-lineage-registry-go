# Metric Lineage Registry

统一的**指标定义注册、版本化、血缘追踪与增量重算**组件（Go）。
解决指标口径与派生关系散落各处、定义变更影响面不可知、计算/查询结果不可复现的问题。

- 指标定义带标识、口径、单位、维度，注册即版本化，历史版本不可原地覆盖
- 派生指标通过表达式引用其他指标，注册时做**直接与间接循环引用检测**
- 单位量纲与维度兼容性静态校验，可换算单位按显式规则归一后参与计算
- 基于血缘的**增量重算**，只重算受影响指标，结果与全量重算逐键一致
- 注册 / 查询 / 喂数 / 重算可并发：查询永远读到某个完整已提交版本
- 事件日志持久化，重启后定义、版本、血缘与数据可恢复，结果确定性重建

## 安装与快速开始

```go
import "github.com/cuixudong74/metric-lineage-registry-go/registry"

// StorePath 留空为纯内存模式；生产环境传文件路径以支持重启恢复。
r, err := registry.New(registry.DefaultOptions())
defer r.Close()

// 1. 注册基础指标（外部喂数）
r.Register(registry.RegisterInput{
    ID: "gmv", Unit: registry.Unit{Name: "CNY"},
    Dimensions: []string{"region"},
})
r.Register(registry.RegisterInput{
    ID: "refund", Unit: registry.Unit{Name: "CNY"},
    Dimensions: []string{"region"},
})

// 2. 注册派生指标：口径即表达式，单位/维度/循环在注册时校验
r.Register(registry.RegisterInput{
    ID: "refund_rate",
    Formula:    "refund / gmv",
    Unit:       registry.Unit{Name: "ratio"},
    Dimensions: []string{"region"},
})

// 3. 喂基础数据（原始单位数值）
r.PutBaseData("gmv", map[string]string{"region": "cn"}, 100)
r.PutBaseData("refund", map[string]string{"region": "cn"}, 20)

// 4. 增量重算并读取结果
r.Recompute(false)
v, snapVersion, ok := r.GetResult("refund_rate", map[string]string{"region": "cn"})
// v == 0.2
```

## 指标与表达式

- 标识：`[A-Za-z_][A-Za-z0-9_]*`，全局唯一；**已下线标识不可复活**。
- 两类指标：
  - 基础指标 `KindBase`：不提供 `Formula`，通过 `PutBaseData` 喂数。
  - 派生指标 `KindDerived`：提供 `Formula`，值只由计算产生。
- 表达式支持：数字常量、指标引用、括号、`+ - * /`、一元负号，例如 `(gmv - refund) / gmv * 100`。
- 基础指标可升级为派生指标（发布新版本）；派生指标不可降级为基础指标。

## 版本化语义

- `Register` 同一 `ID`：定义（口径/单位/维度）完全相同 → 返回 `ErrAlreadyExists`，**不产生版本**。
- 任一属性变化 → 生成新版本号（同一 ID 下从 1 递增）。旧版本永不被修改。
- 查询：
  - `CurrentSnapshot()`：当前已提交完整只读视图，版本号全局单调递增。
  - `GetVersion(id, n)` / `History(id)`：读取任意历史版本。
- 下线 `Retire` 也产生一个不可变版本（`Retired=true`）；仍有活跃下游时拒绝下线。

## 血缘语义

- 边的方向为 **输入指标 → 派生指标**（上游 → 下游）。
- `Snapshot.Impacted(id)`：返回 `id` 变更/下线会影响到的**全部直接与间接下游派生指标**，
  按拓扑顺序排列（依赖者排在被依赖者之后）。
- `Snapshot.Lineage(id)`：直接输入。
- 循环引用：注册新定义前在当前已提交血缘上做 DFS 三色检测。
  - 自引用（`x` 的公式引用 `x`）与任意深度间接环（`a→b→c→a`）一律拒绝，返回
    `ErrCycleDetected`（可 `errors.Is`），`CycleError.Path` 给出环路径。
  - 拒绝发生在任何写入之前，注册表不会留下半截血缘。

### 维度规则

- 指标声明的维度名会排序去重；派生指标的维度集合必须与其**每个输入完全一致**
  （集合相等，声明顺序任意）。维度对不上直接返回 `ErrDimensionMismatch`。
- 数据按维度键连接：同一维度键值组合（如 `region=cn`）下，所有输入齐备才计算；
  某个输入在该键无数据，则该键不产出结果（不产生静默零值）。
- 无维度指标用 `nil`/空 map 喂数与查询。

### 单位规则

单位是同一物理量纲下的仿射变换，规则显式、无隐式猜测：

```
canonical = raw * Scale + Offset
raw       = (canonical - Offset) / Scale
```

- 每个单位属于一个 `Dimension`（如 `currency`、`ratio`、`duration`、`count`）。
- 不同 `Dimension` 不可换算；在派生表达式中按下列规则做静态量纲推断，不兼容即拒绝：

| 运算 | 规则 | 结果量纲 |
|---|---|---|
| 一元 `-` | 透传 | 输入量纲 |
| `+` `-` | 所有带量纲输入必须同量纲；常量不得与带量纲值加减 | 该量纲（或无量纲） |
| `*` | 至多一个物理量纲；`ratio`/常量透传 | 物理量纲 / `ratio` / 无量纲 |
| `/` | 两个物理量纲必须**相同**，相消 | `ratio` |
| `/` | 仅一个物理量纲 | 该物理量纲 |

- 刻意**不支持复合量纲**（如 `currency * count`、`currency / count`），避免口径歧义。
- 推断出的量纲还必须与派生指标声明单位的量纲一致，否则拒绝。
- 计算时所有输入先归一到量纲基准单位（canonical），表达式结果再换算回声明单位，
  因此 `USD` 与 `CNY`、`percent` 与 `ratio` 可在兼容口径中混用。

#### 内置单位

| 量纲 | 单位（Scale, Offset） |
|---|---|
| `currency` | `CNY`(1)、`USD`(7.1)、`kCNY`(1000) |
| `ratio` | `ratio`(1)、`percent`(0.01) |
| `count` | `count`(1)、`thousand`(1000) |
| `duration` | `s`(1)、`ms`(0.001)、`min`(60)、`hour`(3600) |

可通过 `RegisterUnit` 注册自定义单位；同名但定义不同会被拒绝（换算规则不可悄悄变更）。
内置换算系数仅为开箱默认值，生产口径请按组织标准显式注册覆盖（需调整默认系数时建议新起单位名）。

## 增量重算与一致性

```go
res, _ := r.Recompute(false) // 增量
res, _ = r.Recompute(true)   // 强制全量（正确性基准）
```

- 每个基础指标维护**数据水位**（最后一条数据事件序号）与"已重算基线"；
  每个派生指标维护**上游结构化签名**（各输入的定义版本 + 基础数据水位 / 派生签名的确定性拼接，
  非哈希值、无碰撞）。
- 增量规划：
  1. 找出数据水位高于基线的基础指标、定义版本或指纹过期的派生指标（脏源）；
  2. 沿下游边拓扑传播，得到受影响集合；
  3. 仅重写集合内指标的结果。集合外的结果字节级保持不变。
- 定义变更（`Register` 新版本）后，旧派生结果在重算前对查询不可见，
  保证"读到的结果一定匹配当前已提交定义"。
- 增量结果与 `Recompute(true)` 全量结果逐维度键一致（测试强制校验）。

### 并发模型

- 内部用 `sync.RWMutex` 保护可变状态；所有变更在锁内完成校验、事件追加与指针切换。
- `CurrentSnapshot()` 返回的快照一经发布不再被修改；注册/下线通过**整体替换快照指针**
  发布，因此并发查询只能读到完整提交版本，读不到中间状态。
- `Recompute` 分三阶段：
  1. 锁内取快照 + 状态工作副本并确定待算集合；
  2. **锁外**在副本上计算（不阻塞注册/喂数/查询）；
  3. CAS 提交：若基线快照或事件水位已变，丢弃副本并基于最新状态重试。
  - 因此重算期间到达的定义/数据变更不会丢失：要么已在当前轮纳入，要么触发重试/下一轮纳入。

## 持久化与重启恢复

- 已提交状态以 **JSON Lines 事件日志** 追加存储，每行一个完整事件，字段：
  `seq`（全局连续递增）、`kind`（`register` / `retire` / `data`）及对应载荷。
- `StoreSync=true`（默认）时每个事件 `fsync` 成功后才返回。
- 恢复语义：
  - 顺序重放事件重建定义、版本、血缘与基础数据；序号必须连续。
  - 崩溃残留的**半行/坏行视为未提交**，从该处截断，后续内容忽略。
  - 派生结果**不持久化**：恢复后基于最终快照做一次确定性全量重算。
    因此"未完成的重算"天然只存在于内存工作副本，与已提交状态严格分离，
    重启后不会重复应用变更，也不会漏掉已提交事件。
  - 恢复后继续提交时事件序号接着日志最大值递增，不冲突、不重复。

## 配置项（`registry.Options`）

| 字段 | 默认 | 说明 |
|---|---|---|
| `StorePath` | `""`（纯内存） | 事件日志文件路径；非空则启动时恢复 |
| `StoreSync` | `true` | 每个已提交事件是否 `fsync` |
| `MaxRecomputeRetries` | `64` | 并发冲突时重算 CAS 重试上限 |
| `Clock` | `time.Now` | 版本时间戳来源（测试可注入） |

## 错误速查

| 哨兵错误 | 触发场景 |
|---|---|
| `ErrAlreadyExists` | 重复注册相同定义；自定义单位重名冲突 |
| `ErrCycleDetected` | 自引用 / 间接循环血缘（`CycleError.Path`） |
| `ErrIncompatibleUnit` | 量纲不兼容、表达式量纲与声明单位不符、单位未注册 |
| `ErrDimensionMismatch` | 派生与输入维度集合不一致；喂数缺维度 |
| `ErrUnknownInput` | 表达式引用不存在的指标；计算时输入无数据 |
| `ErrDependentStillActive` | 仍有活跃下游依赖时下线 |
| `ErrNotFound` | 指标 / 历史版本不存在 |
| `ErrInvalidDefinition` | 标识非法、表达式语法错、除零、派生降级为基础等 |
| `ErrStore` | 日志打开 / 写入 / fsync / 序号断裂 |

## 测试

```bash
go test ./... -race -count=1
```

覆盖场景：直接与间接循环血缘、环路径、单位不兼容、维度对不上、未知输入、
单位换算归一（USD/CNY、percent/ratio）、版本化与历史不可变、影响面与下线、
增量重算只动受影响分支且与全量一致、定义变更触发重算、并发注册与查询读到一致快照、
并发喂数与重算不丢更新、重启恢复与幂等重放、崩溃半行日志截断、多级血缘传播。
