# Metric Lineage Registry

一套在统一注册入口下维护**指标定义、版本与派生血缘**，并保证计算结果一致可复现的 Go 库。

指标口径和派生关系集中到注册中心后：

* 指标定义带标识、口径（Spec）、单位和业务维度，**版本化**保存，历史版本只读、不可原地覆盖；
* 派生指标以公式引用其他指标，注册时进行**血缘环检测、单位兼容性与维度覆盖校验**；
* 上游定义或数据变更时可做**基于血缘的增量重算**，结果与全量重算严格一致；
* 注册、查询、重算可**并发**进行，读者永远看到某个已提交的完整快照；
* 已提交内容写入 **fsync 的 WAL**，重启后定义、版本、血缘与结果都能恢复，未完成的重算不会被错误应用。

## 包结构

| 包 | 职责 |
|----|------|
| `metric`   | 指标定义、版本、状态、数据点等核心类型与错误哨兵 |
| `expr`     | 派生公式的小型算术语言（数字、`a/b/...` 槽位引用、`+ - * /`、括号） |
| `unit`     | 量纲向量、线性/仿射单位换算、复合单位（`m/s`、`m^2`）与结果量纲推断 |
| `lineage`  | 不可变依赖图：环检测、影响分析、拓扑排序 |
| `compute`  | 校验派生定义；全量与增量计算（维度投影、inner join、单位归一） |
| `store`    | fsync 的 JSON-line WAL 与原子快照文件 |
| `registry` | 门面：MVCC 提交、并发控制、重算事务、WAL 恢复 |

## 快速开始

```go
r, err := registry.New(registry.Config{Dir: "/var/lib/metrics"})
if err != nil { log.Fatal(err) }
defer r.Close()

// 1) 注册基础指标
r.Register(&metric.Definition{
    ID: "gmv_cny", Kind: metric.KindBase,
    Spec:  "成交金额（人民币口径）",
    Unit:  "CNY",
    Dims:  []string{"region", "day"},
})

// 2) 注册派生指标：公式槽位 -> 输入指标 ID
r.Register(&metric.Definition{
    ID:   "gmv_usd",
    Kind: metric.KindDerived,
    Spec: "成交金额，统一换算为美元",
    Unit: "USD",                       // 声明单位，参与量纲校验
    Dims: []string{"region", "day"},
    Formula: "a",                      // 仅做单位归一
    Inputs:  map[string]string{"a": "gmv_cny"},
})

// 3) 写入基础数据
r.UpsertBaseData("gmv_cny", []metric.Point{
    {DimValues: []string{"CN", "2026-01-01"}, Value: 700},
})

// 4) 增量重算并读取结果
r.Recompute()
vals, _ := r.Result("gmv_usd") // 700 CNY 按配置汇率归一为 USD
```

开启 `Config{AutoRecompute: true}` 后，注册/数据提交会在同一事务内同步完成增量重算，
提交成功后读到的派生结果即为最新。

## 配置项 `registry.Config`

| 字段 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `Dir` | `string` | `""`（纯内存） | WAL/快照持久化目录；为空时不写盘，重启不保留 |
| `AutoRecompute` | `bool` | `false` | 为 `true` 时，每次定义/数据/下线提交在事务内同步执行增量重算，提交后无 dirty 指标 |

自定义单位可在启动时通过 `(*Registry).RegisterUnit` 注册（线性单位：`baseValue = value*Factor`）。

## 版本化语义

* 首次注册得到 `Version=1`；
* 相同 ID + **完全相同口径内容**重复注册是幂等的，返回既有版本，不产生新记录；
* 内容变化（Spec / Unit / Dims / Formula / Inputs 任一不同）产生 `Version+1`；
* 历史版本通过 `GetVersion(id, v)` / `Versions(id)` 读取，永不被原地覆盖；
* 不允许改变指标的 `Kind`（base ↔ derived）。

## 并发与一致性

* 所有可变状态保存在不可变 `*compute.Snapshot` 中，读接口（`Get`、`Snapshot`、
  `Impact`、`Result`、`Versions`）拿到的永远是**某个完整已提交版本**；
* 单把提交锁串行化写事务：先基于最新快照校验，再追加 WAL（fsync），最后原子发布新快照；
* 重算同样走 WAL + 发布路径；若发布时发现快照已被更新，则放弃本次纯计算结果并重试，
  因此重算期间到达的定义/数据变更不会丢失，也不会产生陈旧结果。

## 持久化与重启恢复

* `Dir/wal.log` 为 JSON-line 预写日志，每条记录带单调递增 `Seq`，追加后 `fsync`；
* 重启时按序重放 `register / retire / base_update` 重建定义、状态、基础数据与血缘；
  `results` 记录作为结果检查点，恢复其结果与 dirty 集合；
* 崩溃导致的尾部半条记录会被忽略，只恢复完整已提交前缀；
* “进行中但未提交”的重算从不写日志，恢复后既不会重复应用，也不会遮盖已提交变更；
  尚未被检查点覆盖的变更，其下游指标会保留在 dirty 集合中。

## 血缘语义与单位规则

详见 [`docs/lineage.md`](docs/lineage.md)。

## 测试

```bash
go test ./...            # 单元 + 场景测试
go test ./... -race      # 含数据竞争检测
```

覆盖场景：循环血缘（直接/间接）、单位不兼容与维度不匹配、并发注册与查询、
增量与全量重算一致性、重启恢复（定义/版本/血缘/结果/dirty）、WAL 半条记录。
