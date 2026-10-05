// Package engine 实现本地事件驱动的回测撮合引擎：
// 订单状态机、每标的订单簿、市价/限价撮合、部分成交、撤单与公司行为联动。
package engine

import (
	"errors"
)

// OrderStatus 是订单状态。
type OrderStatus string

const (
	StatusNew               OrderStatus = "new"
	StatusPartiallyFilled   OrderStatus = "partially_filled"
	StatusFilled            OrderStatus = "filled"
	StatusCanceled          OrderStatus = "canceled"           // 全部撤销
	StatusPartiallyCanceled OrderStatus = "partially_canceled" // 部分成交后撤销剩余
	StatusRejected          OrderStatus = "rejected"
)

// RejectReason 是可区分的拒绝原因。
type RejectReason string

const (
	RejectInvalidEvent     RejectReason = "invalid_event"
	RejectInvalidPrice     RejectReason = "invalid_price"
	RejectInvalidQty       RejectReason = "invalid_qty"
	RejectDuplicateEvent   RejectReason = "duplicate_event"
	RejectDuplicateOrderID RejectReason = "duplicate_order_id"
	RejectUnknownOrder     RejectReason = "unknown_order"
	RejectOrderTerminal    RejectReason = "order_in_terminal_state"
	RejectNothingToCancel  RejectReason = "nothing_to_cancel"
	RejectCancelExceedsQty RejectReason = "cancel_qty_exceeds_remaining"
	RejectInsufficientCash RejectReason = "insufficient_cash"
	RejectInsufficientPos  RejectReason = "insufficient_position"
	RejectNoMarketPrice    RejectReason = "no_market_price"
	RejectNoLiquidity      RejectReason = "no_liquidity"
)

// Reject 携带可区分的拒绝原因与明细。
type Reject struct {
	Reason  RejectReason
	Message string
}

func (r *Reject) Error() string { return string(r.Reason) + ": " + r.Message }

func reject(reason RejectReason, format string, args ...any) *Reject {
	return &Reject{Reason: reason, Message: sprintf(format, args...)}
}

// Order 是引擎内的订单视图。
type Order struct {
	ID        string
	Symbol    string
	Side      string
	Type      string
	Qty       float64 // 原始委托数量
	Price     float64 // 限价；市价为 0
	FilledQty float64
	AvgPrice  float64
	Status    OrderStatus
	EventSeq  int64 // 提交事件序号
	Reject    *Reject
}

// Remaining 返回未成交数量。
func (o *Order) Remaining() float64 { return o.Qty - o.FilledQty }

// Terminal 报告订单是否处于终态。
func (o *Order) Terminal() bool {
	switch o.Status {
	case StatusFilled, StatusCanceled, StatusPartiallyCanceled, StatusRejected:
		return true
	}
	return false
}

// legalTransitions 定义合法状态迁移白名单。
var legalTransitions = map[OrderStatus]map[OrderStatus]bool{
	StatusNew: {
		StatusPartiallyFilled:   true,
		StatusFilled:            true,
		StatusCanceled:          true,
		StatusRejected:          true,
		StatusPartiallyCanceled: true, // 市价单零成交取消归到 Canceled，这里保留兼容
	},
	StatusPartiallyFilled: {
		StatusPartiallyFilled:   true,
		StatusFilled:            true,
		StatusPartiallyCanceled: true,
	},
}

func (o *Order) transition(to OrderStatus) error {
	if to == o.Status {
		return nil
	}
	allowed, ok := legalTransitions[o.Status]
	if !ok || !allowed[to] {
		return errors.New("illegal order status transition: " + string(o.Status) + " -> " + string(to))
	}
	o.Status = to
	return nil
}

// applyFill 累加成交并刷新均价与状态（to 由调用方按剩余量决定）。
func (o *Order) applyFill(qty, price float64, to OrderStatus) error {
	o.AvgPrice = (o.AvgPrice*o.FilledQty + price*qty) / (o.FilledQty + qty)
	o.FilledQty += qty
	return o.transition(to)
}
