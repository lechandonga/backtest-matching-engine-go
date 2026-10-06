// Package event 定义回测撮合系统中流转的全部事件。
//
// 所有事件都带有全局唯一的 EventID 和单调递增的 Seq。引擎以
// EventID 做幂等去重，以 Seq（相同 Seq 再按 EventID）做确定性排序，
// 保证同一批事件无论以何种顺序、多少次并发投递，最终状态一致。
package event

import "fmt"

// Kind 事件类型。
type Kind uint8

const (
	KindQuote        Kind = 1 // 行情快照（最新买卖价）
	KindTrade        Kind = 2 // 行情成交流（用于推动限价单撮合）
	KindOrderSubmit  Kind = 3 // 提交委托
	KindOrderCancel  Kind = 4 // 撤销委托
	KindSplit        Kind = 5 // 拆股/合股公司行为
	KindDividend     Kind = 6 // 现金分红公司行为
	KindSessionClose Kind = 7 // 交易日收盘（日终结算：失效当日单、收盘价盯市、产出日终快照）
)

func (k Kind) String() string {
	switch k {
	case KindQuote:
		return "QUOTE"
	case KindTrade:
		return "TRADE"
	case KindOrderSubmit:
		return "ORDER_SUBMIT"
	case KindOrderCancel:
		return "ORDER_CANCEL"
	case KindSplit:
		return "CORPORATE_SPLIT"
	case KindDividend:
		return "CORPORATE_DIVIDEND"
	case KindSessionClose:
		return "SESSION_CLOSE"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", int(k))
	}
}

// Side 买卖方向。
type Side uint8

const (
	Buy  Side = 1
	Sell Side = 2
)

func (s Side) String() string {
	switch s {
	case Buy:
		return "BUY"
	case Sell:
		return "SELL"
	default:
		return fmt.Sprintf("SIDE(%d)", int(s))
	}
}

// OrderType 委托类型。
type OrderType uint8

const (
	Market       OrderType = 1 // 市价单
	Limit        OrderType = 2 // 限价单
	StopMarket   OrderType = 3 // 止损市价单：触发前不参与撮合，触发后按市价单处理
	StopLimit    OrderType = 4 // 止损限价单：触发前不参与撮合，触发后按限价单处理
	TrailingStop OrderType = 5 // 跟踪止损：触发前不参与撮合，基准只朝有利方向走，回撤触发后按市价单处理
)

func (t OrderType) String() string {
	switch t {
	case Market:
		return "MARKET"
	case Limit:
		return "LIMIT"
	case StopMarket:
		return "STOP_MARKET"
	case StopLimit:
		return "STOP_LIMIT"
	case TrailingStop:
		return "TRAILING_STOP"
	default:
		return fmt.Sprintf("TYPE(%d)", int(t))
	}
}

// TimeInForce 委托有效期。零值兼容旧事件，按 Day 处理。
type TimeInForce uint8

const (
	Day TimeInForce = 1 // 当日有效（默认）
	IOC TimeInForce = 2 // 立即成交否则取消：能成多少成多少，剩余立即撤销
	FOK TimeInForce = 3 // 全部成交否则取消：本次撮合要么全成，要么一点不成
	GTC TimeInForce = 4 // 长期有效：跨交易日保留
)

// TIF 是 TimeInForce 的简写别名。
type TIF = TimeInForce

func (t TimeInForce) String() string {
	switch t {
	case Day:
		return "DAY"
	case IOC:
		return "IOC"
	case FOK:
		return "FOK"
	case GTC:
		return "GTC"
	default:
		return fmt.Sprintf("TIF(%d)", int(t))
	}
}

// Event 是引擎接受的唯一输入结构。不同 Kind 使用对应字段，其余字段留空。
type Event struct {
	// EventID 全局唯一事件 ID，用于幂等去重，必填。
	EventID string
	// Seq 单调递增的事件序号（行情时间戳语义），用于确定性排序，必须 >= 0。
	Seq int64
	// Kind 事件类型。
	Kind Kind

	// Symbol 行情/委托/公司行为所对应的标的。
	Symbol string

	// 行情字段（KindQuote）：买一/卖一价量。
	BidPrice float64
	BidSize  int64
	AskPrice float64
	AskSize  int64

	// 成交流字段（KindTrade）：成交价与成交量。
	TradePrice float64
	TradeSize  int64

	// 委托字段（KindOrderSubmit）。
	OrderID       string
	Side          Side
	Type          OrderType
	Price         float64     // 限价单价格；市价单忽略
	Qty           int64       // 委托数量（正数）
	TIF           TimeInForce // 有效期；零值按 Day 处理
	StopPrice     float64     // 触发型委托的触发价（StopMarket/StopLimit）
	TrailDistance float64     // 跟踪止损：绝对回撤价差（与 TrailPct 二选一，>0 生效）
	TrailPct      float64     // 跟踪止损：比例回撤，如 0.05 表示从最有利基准回撤 5% 触发

	// 撤单字段（KindOrderCancel）：复用 OrderID 与 Qty。
	// Qty == 0 表示全部撤销；Qty > 0 表示只撤销指定剩余数量。

	// 公司行为字段。
	SplitRatio  float64 // KindSplit：每股变为 SplitRatio 股（2=一拆二）
	PostSplit   float64 // KindSplit：除权后参考价，用于未实现盈亏盯市
	DivPerShare float64 // KindDividend：每股派现金额（税前）

	// 交易日字段。
	// TradeDate 用于给事件打上交易日标签（如 "2026-10-06"）；
	// 同一交易日的事件应使用相同标签。跨到新标签的首个事件会先自动收掉上一交易日。
	TradeDate string
	// ClosePrice 用于 KindSessionClose：该标的的收盘价（>0 时用于盯市）。
	// 收盘事件可按标的投递（Symbol 非空），也可投递一条无 Symbol 的全局收盘事件
	// 统一触发当日单失效；两类事件在同一交易日都只有第一次生效。
	ClosePrice float64
}

func (e Event) String() string {
	return fmt.Sprintf("Event(id=%s seq=%d kind=%s symbol=%s)", e.EventID, e.Seq, e.Kind, e.Symbol)
}
