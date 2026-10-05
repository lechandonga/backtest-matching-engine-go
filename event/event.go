// Package event 定义本地回测的事件协议。
//
// 一次回测就是按 Seq 顺序（或在 shard 并发处理后按 Seq 提交）回放一串 Event。
// 每一种 Event 都携带业务幂等键 EventID：同一 EventID 重复投递只会生效一次。
package event

import (
	"errors"
)

// Kind 是事件类型。
type Kind string

const (
	KindMarket   Kind = "market"   // 行情更新：最新价 + 可成交流动性
	KindSubmit   Kind = "submit"   // 新委托（市价 / 限价）
	KindCancel   Kind = "cancel"   // 撤销委托剩余数量（可部分撤）
	KindSplit    Kind = "split"    // 公司行为：拆股（含合股，ratio < 1）
	KindDividend Kind = "dividend" // 公司行为：每股现金分红
)

// Side 是买卖方向。
type Side string

const (
	Buy  Side = "buy"
	Sell Side = "sell"
)

// OrderType 是委托类型。
type OrderType string

const (
	Market OrderType = "market" // 市价单：IOC，按行情立即成交，剩余立即取消
	Limit  OrderType = "limit"  // 限价单：GTC，挂入订单簿等待撮合
)

// Event 是回测流中的一条事件。
//
// Seq 为全局单调序号（从 0 或 1 开始均可，只要求严格递增），
// 并发处理时同一标的内严格按 Seq，不同标的之间的账户记账提交也按 Seq 排序，
// 因此并发乱序投递不改变最终结果。
type Event struct {
	Seq     int64
	EventID string
	Kind    Kind

	// Market 字段，Kind == KindMarket 时有效。
	Symbol    string
	LastPrice float64
	Liquidity []Liquidity

	// Order 字段，Kind == KindSubmit 时有效。
	Order Order

	// Cancel 字段，Kind == KindCancel 时有效；Qty 为 0 表示全撤。
	CancelSymbol  string
	CancelOrderID string
	CancelQty     float64

	// 公司行为字段，Kind == KindSplit / KindDividend 时有效。
	CorporateSymbol string
	Ratio           float64 // Split: 每股变 ratio 股（2 表示一拆二，0.5 表示二合一）
	PerShareCash    float64 // Dividend: 每股派现金额
}

// Order 是委托内容。
type Order struct {
	ID     string
	Symbol string
	Side   Side
	Type   OrderType
	Qty    float64
	Price  float64 // 限价单必填且必须 > 0；市价单忽略
}

// Liquidity 是行情中可被吃掉的一档外部流动性。
type Liquidity struct {
	Price float64
	Qty   float64
}

// Validate 校验事件协议层字段是否合法。撮合层的业务拒绝（资金不足等）不在此列。
func (e *Event) Validate() error {
	if e == nil {
		return errors.New("nil event")
	}
	if e.EventID == "" {
		return errors.New("empty event id")
	}
	if e.Seq < 0 {
		return errors.New("negative seq")
	}
	switch e.Kind {
	case KindMarket:
		if e.Symbol == "" {
			return errors.New("market event: empty symbol")
		}
		if e.LastPrice < 0 {
			return errors.New("market event: negative last price")
		}
		for _, l := range e.Liquidity {
			if l.Price <= 0 || l.Qty < 0 {
				return errors.New("market event: invalid liquidity level")
			}
		}
	case KindSubmit:
		return e.Order.validate()
	case KindCancel:
		if e.CancelSymbol == "" || e.CancelOrderID == "" {
			return errors.New("cancel event: missing symbol or order id")
		}
		if e.CancelQty < 0 {
			return errors.New("cancel event: negative qty")
		}
	case KindSplit:
		if e.CorporateSymbol == "" {
			return errors.New("split event: empty symbol")
		}
		if e.Ratio <= 0 {
			return errors.New("split event: ratio must be > 0")
		}
	case KindDividend:
		if e.CorporateSymbol == "" {
			return errors.New("dividend event: empty symbol")
		}
		if e.PerShareCash < 0 {
			return errors.New("dividend event: negative cash per share")
		}
	default:
		return errors.New("unknown event kind: " + string(e.Kind))
	}
	return nil
}

func (o *Order) validate() error {
	if o.ID == "" {
		return errors.New("submit event: empty order id")
	}
	if o.Symbol == "" {
		return errors.New("submit event: empty symbol")
	}
	if o.Qty <= 0 {
		return errors.New("submit event: qty must be > 0")
	}
	switch o.Side {
	case Buy, Sell:
	default:
		return errors.New("submit event: unknown side")
	}
	switch o.Type {
	case Market:
	case Limit:
		if o.Price <= 0 {
			return errInvalidPrice
		}
	default:
		return errors.New("submit event: unknown order type")
	}
	return nil
}

var errInvalidPrice = errors.New("submit event: limit price must be > 0")

// IsInvalidPriceErr 报告协议校验错误是否由非法价格引起。
func IsInvalidPriceErr(err error) bool { return errors.Is(err, errInvalidPrice) }
