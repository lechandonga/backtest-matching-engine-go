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
	StatusTriggered      // 触发型委托已触发（瞬时状态，保留用于可观测记录）
	StatusExpired        // 终态：日终过期或有效期到期（FOK 未成 / IOC 剩余 / Day 日终）
	StatusWaitingTrigger // 触发型委托：等待触发（不参与撮合、无成交，但账户可观测其待触发状态）
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
	case StatusTriggered:
		return "TRIGGERED"
	case StatusExpired:
		return "EXPIRED"
	case StatusWaitingTrigger:
		return "WAITING_TRIGGER"
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
	RejectInvalidEvent      RejectReason = "INVALID_EVENT"         // 事件字段非法（缺 ID、序号非法等）
	RejectUnknownOrder      RejectReason = "UNKNOWN_ORDER"         // 撤单目标订单不存在
	RejectDuplicateOrderID  RejectReason = "DUPLICATE_ORDER_ID"    // 委托 ID 重复
	RejectTerminalOrder     RejectReason = "TERMINAL_ORDER"        // 对终态订单重复操作
	RejectInvalidPrice      RejectReason = "INVALID_PRICE"         // 价格 <= 0
	RejectInvalidQty        RejectReason = "INVALID_QTY"           // 数量 <= 0
	RejectInvalidSide       RejectReason = "INVALID_SIDE"          // 买卖方向非法
	RejectInvalidType       RejectReason = "INVALID_TYPE"          // 委托类型非法
	RejectInvalidLimitPrice RejectReason = "INVALID_LIMIT_PRICE"   // 限价单未给出有效价格
	RejectCancelExceedQty   RejectReason = "CANCEL_EXCEED_QTY"     // 撤单数量超过剩余数量
	RejectInsufficientCash  RejectReason = "INSUFFICIENT_CASH"     // 买入可用资金不足
	RejectInsufficientPos   RejectReason = "INSUFFICIENT_POSITION" // 卖出持仓不足
	RejectNoMarketData      RejectReason = "NO_MARKET_DATA"        // 市价单无任何行情可成交
	RejectNoLiquidity       RejectReason = "NO_LIQUIDITY"          // 市价单无法足额成交（立即撤销剩余）

	// 以下为本轮新增拒单原因，与既有原因并列、互不重叠。
	RejectInvalidTIF        RejectReason = "INVALID_TIF"         // 有效期非法
	RejectInvalidStopPrice  RejectReason = "INVALID_STOP_PRICE"  // 触发价非法
	RejectInvalidTrail      RejectReason = "INVALID_TRAIL"       // 跟踪止损参数非法
	RejectTriggerNoCash     RejectReason = "TRIGGER_NO_CASH"     // 触发瞬间买入资金不足，委托直接拒掉
	RejectTriggerNoPosition RejectReason = "TRIGGER_NO_POSITION" // 触发瞬间卖出持仓不足，委托直接拒掉
	RejectSessionClosed     RejectReason = "SESSION_CLOSED"      // 交易日已收盘后再来成交/撤单等请求
	RejectExpiredOrder      RejectReason = "EXPIRED_ORDER"       // 对已过期委托再操作
	RejectFOKNotFilled      RejectReason = "FOK_NOT_FILLED"      // FOK 本次撮合无法全部成交，整单一点不成
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

	// 有效期。
	TIF event.TimeInForce

	// 触发型委托字段。
	StopPrice     float64 // 止损/止损限价触发价
	TrailDistance float64 // 跟踪止损：绝对回撤价差
	TrailPct      float64 // 跟踪止损：比例回撤
	Triggered     bool    // 是否已触发（触发只发生一次）
	TriggerSeq    int64   // 触发时的事件 Seq
	// TrailAnchor 跟踪止损的最有利基准价：卖出方向跟踪见到的最高价，
	// 买入方向跟踪最低价。只朝有利方向移动，触发后冻结。
	TrailAnchor float64

	// ReservePrice 下单冻结时使用的参考价（限价=限价；市价=对手价；触发型=触发价/限价）。
	ReservePrice float64

	// TradeDate 委托所在交易日（Day/IOC/FOK 仅当日有效；GTC 跨日保留）。
	TradeDate string
}

// IsTriggerOrder 是否为触发型委托。
func (o *Order) IsTriggerOrder() bool {
	return o.Type == event.StopMarket || o.Type == event.StopLimit || o.Type == event.TrailingStop
}

// IsWaitingTrigger 是否处于待触发状态。
func (o *Order) IsWaitingTrigger() bool {
	return o.IsTriggerOrder() && !o.Triggered && o.Status == StatusWaitingTrigger
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

// canCancelStatus 撤单只允许发生在活动委托上（含待触发委托）；
// 已触发但未成交完的委托与普通委托一样可以撤。终态/过期委托一律拒绝。
func canCancelStatus(s OrderStatus) bool {
	return s == StatusNew || s == StatusPartiallyFilled || s == StatusWaitingTrigger
}

// trigger 条件判定与跟踪止损基准更新逻辑放在引擎层，模型层只保留状态字段。
