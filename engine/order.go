package engine

import (
	"fmt"

	"github.com/lechandonga/backtest-matching-engine-go/event"
)

// OrderStatus 订单状态。
type OrderStatus uint8

const (
	StatusPendingNew OrderStatus = iota // 已接收、尚未进入撮合
	StatusNew                           // 有效委托（可部分成交）
	StatusPartiallyFilled
	StatusFilled         // 终态：全部成交
	StatusCanceled       // 终态：撤销（允许部分成交后撤单）
	StatusRejected       // 终态：被拒单
	StatusWaitingTrigger // 触发型委托已接收、等待触发（不参与撮合、不占资金/持仓）
	StatusExpired        // 终态：日终过期
)

func (s OrderStatus) String() string {
	switch s {
	case StatusPendingNew:
		return "PENDING_NEW"
	case StatusNew:
		return "NEW"
	case StatusPartiallyFilled:
		return "PARTIALLY_FILLED"
	case StatusFilled:
		return "FILLED"
	case StatusCanceled:
		return "CANCELED"
	case StatusRejected:
		return "REJECTED"
	case StatusWaitingTrigger:
		return "WAITING_TRIGGER"
	case StatusExpired:
		return "EXPIRED"
	default:
		return fmt.Sprintf("STATUS(%d)", int(s))
	}
}

// IsTerminal 终态订单不再接受成交或撤单。
func (s OrderStatus) IsTerminal() bool {
	return s == StatusFilled || s == StatusCanceled || s == StatusRejected || s == StatusExpired
}

// RejectReason 可区分的拒单原因。
type RejectReason string

const (
	RejectInvalidEvent      RejectReason = "INVALID_EVENT"                 // 事件字段非法（缺 ID、序号非法等）
	RejectUnknownOrder      RejectReason = "UNKNOWN_ORDER"                 // 撤单目标订单不存在
	RejectDuplicateOrderID  RejectReason = "DUPLICATE_ORDER_ID"            // 委托 ID 重复
	RejectTerminalOrder     RejectReason = "TERMINAL_ORDER"                // 对终态订单重复操作
	RejectInvalidPrice      RejectReason = "INVALID_PRICE"                 // 价格 <= 0
	RejectInvalidQty        RejectReason = "INVALID_QTY"                   // 数量 <= 0
	RejectInvalidSide       RejectReason = "INVALID_SIDE"                  // 买卖方向非法
	RejectInvalidType       RejectReason = "INVALID_TYPE"                  // 委托类型非法
	RejectInvalidLimitPrice RejectReason = "INVALID_LIMIT_PRICE"           // 限价单未给出有效价格
	RejectCancelExceedQty   RejectReason = "CANCEL_EXCEED_QTY"             // 撤单数量超过剩余数量
	RejectInsufficientCash  RejectReason = "INSUFFICIENT_CASH"             // 买入可用资金不足
	RejectInsufficientPos   RejectReason = "INSUFFICIENT_POSITION"         // 卖出持仓不足
	RejectNoMarketData      RejectReason = "NO_MARKET_DATA"                // 市价单无任何行情可成交
	RejectNoLiquidity       RejectReason = "NO_LIQUIDITY"                  // 市价单无法足额成交（立即撤销剩余）
	RejectInvalidTIF        RejectReason = "INVALID_TIF"                   // 有效期取值非法
	RejectInvalidStopPrice  RejectReason = "INVALID_STOP_PRICE"            // 触发价/回撤参数非法
	RejectTriggerNoCash     RejectReason = "TRIGGER_INSUFFICIENT_CASH"     // 触发瞬间买入资金不足
	RejectTriggerNoPos      RejectReason = "TRIGGER_INSUFFICIENT_POSITION" // 触发瞬间卖出持仓不足
	RejectStaleTradingDay   RejectReason = "STALE_TRADING_DAY"             // 收盘事件交易日序号倒退
	RejectOrderExpired      RejectReason = "ORDER_EXPIRED"                 // 对已过期订单重复操作
)

// RejectError 携带结构化拒单原因，方便调用方区分处理。
type RejectError struct {
	Reason  RejectReason
	OrderID string
	Detail  string
}

func (e *RejectError) Error() string {
	if e.OrderID == "" {
		return fmt.Sprintf("order rejected: %s (%s)", e.Reason, e.Detail)
	}
	return fmt.Sprintf("order %s rejected: %s (%s)", e.OrderID, e.Reason, e.Detail)
}

func reject(orderID string, reason RejectReason, detail string) *RejectError {
	return &RejectError{Reason: reason, OrderID: orderID, Detail: detail}
}

// Order 是订单的运行时状态。
type Order struct {
	ID           string
	Symbol       string
	Side         event.Side
	Type         event.OrderType
	LimitPrice   float64
	Qty          int64   // 原始委托数量
	FilledQty    int64   // 累计成交数量
	AvgFillPrice float64 // 成交量加权平均成交价
	Status       OrderStatus
	RejectReason RejectReason
	Seq          int64 // 提交事件的 Seq
	TIF          event.TIF
	SubmitDay    int64 // 提交时所在交易日（DAY 单在该日收盘失效）

	// 触发型委托运行时状态。仅 Type ∈ {Stop, StopLimit, TrailingStop} 使用。
	StopPrice    float64 // Stop/StopLimit 的固定触发价
	TrailPct     float64
	TrailAmount  float64
	AnchorPrice  float64        // TrailingStop 有利方向极值基准，只会朝有利方向更新
	TriggerPrice float64        // 实际触发价（触发后冻结，便于对账与复现）
	TriggeredSeq int64          // 触发事件 Seq；0 表示尚未触发（-1 表示提交即判定触发的保留值）
	RefPrice     float64        // 冻结资金时使用的参考价（限价=限价，市价/触发=对手价）
	reserves     []reserveSlice // 买入冻结分段（仅买单）；卖出冻结按数量直接核销
}

// reserveSlice 是一段按 refPrice 预留的买入冻结（部分成交逐段核销）。
type reserveSlice struct {
	qty      int64
	refPrice float64
}

// RemainingQty 剩余可成交数量。
func (o *Order) RemainingQty() int64 {
	return o.Qty - o.FilledQty
}

// applyFill 记录一次成交并推进状态机。
func (o *Order) applyFill(fillQty int64, price float64) {
	if fillQty <= 0 || fillQty > o.RemainingQty() {
		panic("engine: illegal fill quantity")
	}
	totalNotional := o.AvgFillPrice*float64(o.FilledQty) + price*float64(fillQty)
	o.FilledQty += fillQty
	o.AvgFillPrice = totalNotional / float64(o.FilledQty)
	if o.FilledQty >= o.Qty {
		o.Status = StatusFilled
	} else {
		o.Status = StatusPartiallyFilled
	}
}

// legalSubmitTransitions 提交时只允许进入 PENDING_NEW；状态迁移在撮合过程中受控进行。
var legalCancelFrom = map[OrderStatus]bool{
	StatusNew:             true,
	StatusPartiallyFilled: true,
	StatusWaitingTrigger:  true, // 触发前的条件单允许撤单
}
