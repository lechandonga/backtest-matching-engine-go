# 本地事件驱动回测撮合与组合核算（Go）

用本地行情与订单事件模拟完整交易过程：订单状态机、撮合、资金/持仓/盈亏核算、
公司行为、并发乱序回放，全部在进程内完成，无外部依赖。

## 包结构

| 包 | 职责 |
| --- | --- |
| `event` | 事件协议与字段校验（行情/委托/撤单/拆股/分红） |
| `engine` | 订单状态机、每标的订单簿、市价/限价撮合、部分成交、撤单、公司行为联动 |
| `account` | 现金/持仓预留、成交记账、已实现与未实现盈亏、费用、公司行为、强制对账 |
| `backtest` | 并发回测驱动器：多标的 shard 并发 + 全局 Seq 屏障，保证乱序到达结果确定 |

## 事件协议

每条事件含全局严格递增的 `Seq` 与业务幂等键 `EventID`。

| Kind | 关键字段 | 语义 |
| --- | --- | --- |
| `market` | `Symbol` `LastPrice` `Liquidity[]{Price,Qty}` | 更新最新价；提供可被吃掉的外部流动性档位 |
| `submit` | `Order{ID,Symbol,Side,Type,Qty,Price}` | 提交市价单（IOC）或限价单（GTC） |
| `cancel` | `CancelSymbol` `CancelOrderID` `CancelQty` | `Qty=0` 全撤；>0 部分撤，不能超过剩余数量 |
| `split` | `CorporateSymbol` `Ratio` | 每股变 `Ratio` 股（2 一拆二，0.5 二合一） |
| `dividend` | `CorporateSymbol` `PerShareCash` | 按每股现金派现 |

同一 `EventID` 重复投递：`Result.Duplicate=true`，不产生任何二次副作用
（不重复扣款、不重复成交、不重复计盈亏）。同一 `FillID` 的成交在账户层同样幂等，
且同 ID 内容不一致会返回 `account.ErrDuplicateFill`。

## 订单状态机（合法迁移白名单）

```
new ──部分成交──▶ partially_filled ──部分成交──▶ partially_filled
  │                   │                          │
  │                   ├──全部成交──▶ filled ◀─────┘
  ├──全部撤销──▶ canceled
  └──(部分成交后撤剩余)──▶ partially_canceled ◀── partially_filled
  new / partially_filled ──校验失败──▶ rejected
```

- 市价单为 IOC：立即按外部流动性与对手挂簿成交，未成交部分立即取消；
  零成交时拒绝（`no_liquidity`），有成交时进入 `partially_canceled`。
- 限价单为 GTC：价格交叉立即成交，剩余进入订单簿，价格优先、同价时间优先。
- 终态订单（`filled/canceled/partially_canceled/rejected`）的重复撤单返回
  `order_in_terminal_state`；任何非法状态迁移会直接报错。

### 可区分的拒绝原因（`engine.RejectReason`）

`invalid_event`、`invalid_price`、`invalid_qty`、`duplicate_event`、
`duplicate_order_id`、`unknown_order`、`order_in_terminal_state`、
`nothing_to_cancel`、`cancel_qty_exceeds_remaining`、`insufficient_cash`、
`insufficient_position`、`no_market_price`、`no_liquidity`。

## 撮合规则

- 行情事件携带的 `Liquidity` 是外部可成交盘口：买单按卖价从低到高吃，
  卖单按买价从高到低吃；未吃完的档位保留，供后续行情与市价单使用。
- 行情到达时，挂簿限价单与交叉的外部档位成交（买限 ≥ 卖报价、卖限 ≤ 买报价）。
- 新提交的主动单先吃外部流动性，再吃对手挂簿。
- 成交价格取被动方价格（外部档位价 / 挂单价）。

## 核算规则与权益恒等式

任意时刻（`Account.Reconcile`）强制成立：

```
Equity = InitialCash + RealizedPnL + UnrealizedPnL - Fees + CorporateAdjustments
```

- 买入：现金减 `Qty*Price+Fee`，持仓加权平均成本上升。
- 卖出：现金加 `Qty*Price-Fee`，`RealizedPnL += (Price-AvgCost)*Qty`。
- 未实现盈亏：按最近行情 mark-to-market，`(Mark-AvgCost)*Qty`。
- 下单即做资金/持仓预留：买单预留 `金额+预估费`（市价买按可成交最差价保守预留），
  卖单预留持仓数量（默认禁止卖空）；成交后缩减预留，撤单释放预留。
- 拆股：`数量*=Ratio，成本价/=Ratio`，挂单数量与限价、卖单预留同步调整，
  不改变权益；合股用 `Ratio<1`。
- 分红：现金增加并计入 `CorporateAdjustments`（不进已实现盈亏）。
- 恒等式对不上时返回 `*account.ReconcileError`（含差额），绝不静默吸收。
- 全部变动写入不可变 `LedgerEntry` 流水，便于逐笔解释。

## 并发与确定性

`backtest.Runner` 将事件按标的哈希到多个 shard goroutine（模拟并发到达），
唯一提交协程严格按 `Seq` 放行真正的撮合与记账。因此：

- 并发到达顺序不改变最终订单状态、持仓与盈亏；
- `-race` 下无数据竞争（引擎订单簿按标的加锁，账户自带互斥）；
- 同一序列反复回放、或整批事件中夹带重复 `EventID`，结果指纹一致。

## 本地验证

```bash
# 全量单测，日志中按“业务[xxx] 用例N”展示覆盖的业务场景与结果
go test ./... -v

# 竞态检测（需要 cgo；本机 gcc 可用时）
CGO_ENABLED=1 go test -race -count=1 ./...

# 静态检查
go vet ./...
```

测试覆盖：

- 订单生命周期：市价 IOC、限价挂簿/交叉、部分成交、部分撤/全撤、终态操作、
  非法价格数量、资金/持仓不足、重复订单 ID、超量撤单。
- 盈亏对账：建仓/部分实现/全平、费用科目、拆股、分红、账实不符显式报错。
- 幂等与并发：重复成交/重复事件、并发乱序指纹一致、重复回放一致。

## 快速上手

参见 `backtest/example_test.go`。核心用法：

```go
acct := account.New(100000)
eng := engine.New(acct, engine.WithFeeModel(account.BpsFee(5)))
runner := backtest.New(eng)
outcomes := runner.Run(events, 4)
snap, err := runner.Reconcile(marks) // 任意时刻强制对账
```
