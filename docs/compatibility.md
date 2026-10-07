# 兼容边界（相对上一轮）

## 本轮行为变化（收盘语义）

- **收盘从"按标的独立收口"改为"整日收口"**：一个 `KindDayClose` 事件
  对全部标的的当日单/当日条件单统一失效，不再只影响事件里带的标的。
  原来"每个标的各投一次收盘"的用法仍然安全：第一次收盘完成整日结算，
  后续同交易日的收盘自动幂等为空操作。
- `TradingDay` 单调性校验从"按标的"改为"全局"：任何标的的收盘使交易日
  前进后，更早交易日的收盘一律按 `STALE_TRADING_DAY` 拒绝。
- 新增 `Engine.DayEndSnapshot(day)`：收盘时冻结的日终快照（权益及现金/
  冻结/持仓/已实现/未实现盈亏拆解），重复收盘不改写。
- 收盘事件不带标的时不再在盯市价表里留下空标的记录（旧行为会写入
  `Marks[""]`）。

## 保持不动

- 事件幂等：`EventID` 去重；确定性顺序 `(Seq, EventID)`。
- 账户：冻结/成交/释放语义、权益恒等式与 `ReconcileError`、费用口径、
  公司行为（拆股/分红）全部保留。
- 订单状态：`PENDING_NEW / NEW / PARTIALLY_FILLED / FILLED / CANCELED /
  REJECTED` 的流转与含义不变；新增 `WAITING_TRIGGER`（触发前）、
  `EXPIRED`（日终失效），均为附加状态，旧值编号不变。
- 旧拒单原因全部保留且触发条件不变（`INSUFFICIENT_CASH`、
  `INSUFFICIENT_POSITION`、`TERMINAL_ORDER` 等）。
- `TIF` 零值（历史调用方不填）按 `GTC` 解释，旧委托行为完全不变。

## 新增（均为附加，不重写旧路径）

- 委托类型：`Stop` / `StopLimit` / `TrailingStop`。
- 有效期：`DAY` / `IOC` / `FOK` / `GTC`。
- 事件：`KindDayClose`。
- 拒单原因：
  - `INVALID_TIF`
  - `INVALID_STOP_PRICE`
  - `TRIGGER_INSUFFICIENT_CASH` / `TRIGGER_INSUFFICIENT_POSITION`
  - `STALE_TRADING_DAY`
  - `ORDER_EXPIRED`
- 账户方法：`portfolio.FillSelfCross`、`portfolio.FeeRate`（只读/新增场景）。

注意区分：下单时资金/持仓不足仍是 `INSUFFICIENT_CASH` /
`INSUFFICIENT_POSITION`；只有**触发瞬间**不足才使用
`TRIGGER_INSUFFICIENT_*`。`EXPIRED` 订单的后续操作使用 `ORDER_EXPIRED`，
其它终态仍使用 `TERMINAL_ORDER`。
