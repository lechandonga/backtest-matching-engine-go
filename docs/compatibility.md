# 兼容边界（相对上一轮）

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
