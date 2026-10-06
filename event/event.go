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
	KindQuote       Kind = 1 // 行情快照（最新买卖价）
	KindTrade       Kind = 2 // 行情成交流（用于推动限价单撮合）
	KindOrderSubmit Kind = 3 // 提交委托
	KindOrderCancel Kind = 4 // 撤销委托
	KindSplit       Kind = 5 // 拆股/合股公司行为
	KindDividend    Kind = 6 // 现金分红公司行为
	KindDayClose    Kind = 7 // 交易日收盘：日终失效 + 收盘价盯市
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
	case KindDayClose:
		return "DAY_CLOSE"
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
	Stop         OrderType = 3 // 止损单：触发后转为市价单
	StopLimit    OrderType = 4 // 止损限价单：触发后转为限价单
	TrailingStop OrderType = 5 // 跟踪止损单：触发后转为市价单
)

func (t OrderType) String() string {
	switch t {
	case Market:
		return "MARKET"
	case Limit:
		return "LIMIT"
	case Stop:
		return "STOP"
	case StopLimit:
		return "STOP_LIMIT"
	case TrailingStop:
		return "TRAILING_STOP"
	default:
		return fmt.Sprintf("TYPE(%d)", int(t))
	}
}

// TIF 有效期（time in force）。零值 TIFUnset 仅用于兼容未填写 TIF 的
// 历史调用方，提交时按 GTC（长期有效）解释，旧行为不变。
type TIF uint8

const (
	TIFUnset TIF = 0 // 未指定：按 GTC 处理
	TIODay   TIF = 1 // 当日有效：当日收盘未成交/未触发即失效
	TIFO     TIF = 2 // Immediate-Or-Cancel：立即成交能成多少成多少，剩余撤销
	TFOK     TIF = 3 // Fill-Or-Kill：本次撮合要么全成，要么整单一点不成交
	TGTC     TIF = 4 // 长期有效（Good-Till-Canceled）
)

func (t TIF) String() string {
	switch t {
	case TIFUnset:
		return "UNSET"
	case TIODay:
		return "DAY"
	case TIFO:
		return "IOC"
	case TFOK:
		return "FOK"
	case TGTC:
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
	OrderID string
	Side    Side
	Type    OrderType
	Price   float64 // 限价单价格；市价单忽略
	Qty     int64   // 委托数量（正数）

	TIF TIF // 有效期；零值按 GTC 处理

	// 触发型委托参数（Type = Stop / StopLimit / TrailingStop 时使用）。
	// 触发前不冻结资金/持仓、不参与撮合。
	StopPrice   float64 // Stop/StopLimit 触发价：买 Stop>=价 触发，卖 Stop<=价 触发
	TrailPct    float64 // TrailingStop 跟踪回撤比例，>0，与 TrailAmount 二选一
	TrailAmount float64 // TrailingStop 跟踪回撤绝对价差，>0，与 TrailPct 二选一

	// 撤单字段（KindOrderCancel）：复用 OrderID 与 Qty。
	// Qty == 0 表示全部撤销；Qty > 0 表示只撤销指定剩余数量。

	// 公司行为字段。
	SplitRatio  float64 // KindSplit：每股变为 SplitRatio 股（2=一拆二）
	PostSplit   float64 // KindSplit：除权后参考价，用于未实现盈亏盯市
	DivPerShare float64 // KindDividend：每股派现金额（税前）

	// 收盘事件字段（KindDayClose）。
	TradingDay int64   // 交易日序号（从 1 起单调递增）；0 表示由引擎按顺序推断
	ClosePrice float64 // 收盘价：>0 时用于该标的日终盯市
}

func (e Event) String() string {
	return fmt.Sprintf("Event(id=%s seq=%d kind=%s symbol=%s)", e.EventID, e.Seq, e.Kind, e.Symbol)
}
