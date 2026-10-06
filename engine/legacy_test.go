package engine

import (
	"testing"

	"github.com/lechandonga/backtest-matching-engine-go/event"
	"github.com/lechandonga/backtest-matching-engine-go/portfolio"
)

func newTestEngine(t *testing.T, cash float64) (*Engine, func(bool)) {
	t.Helper()
	pf := portfolio.New(cash, 0)
	eng := New(pf)
	return eng, group(t, grpLegacy)
}

func q(id string, seq int64, sym string, bid, ask float64) event.Event {
	return event.Event{EventID: id, Seq: seq, Kind: event.KindQuote, Symbol: sym,
		BidPrice: bid, AskPrice: ask, BidSize: 10000, AskSize: 10000}
}

func tr(id string, seq int64, sym string, px float64, size int64) event.Event {
	return event.Event{EventID: id, Seq: seq, Kind: event.KindTrade, Symbol: sym,
		TradePrice: px, TradeSize: size}
}

// 旧拒单原因体系与状态流转保持不变。
func TestLegacyRejectReasonsAndStates(t *testing.T) {
	eng, ok := newTestEngine(t, 100_000)

	r := eng.Process(event.Event{EventID: "e1", Seq: 1, Kind: event.KindOrderSubmit,
		OrderID: "o1", Side: event.Buy, Type: event.Limit, Qty: 10})[0]
	ok(r.Reject != nil && r.Reject.Reason == RejectInvalidEvent)

	r = eng.Process(event.Event{EventID: "e2", Seq: 2, Kind: event.KindOrderSubmit,
		Symbol: "AAA", OrderID: "o2", Side: event.Buy, Type: event.Limit, Price: 10, Qty: 0})[0]
	ok(r.Reject != nil && r.Reject.Reason == RejectInvalidQty)

	r = eng.Process(event.Event{EventID: "e3", Seq: 3, Kind: event.KindOrderSubmit,
		Symbol: "AAA", OrderID: "o3", Side: event.Buy, Type: event.Limit, Price: 0, Qty: 10})[0]
	ok(r.Reject != nil && r.Reject.Reason == RejectInvalidLimitPrice)

	r = eng.Process(event.Event{EventID: "e4", Seq: 4, Kind: event.KindOrderSubmit,
		Symbol: "AAA", OrderID: "o4", Side: 99, Type: event.Market, Qty: 10})[0]
	ok(r.Reject != nil && r.Reject.Reason == RejectInvalidSide)

	// 未指定 TIF 的旧调用方按 GTC 处理，限价单可正常挂单。
	eng.Process(q("q1", 5, "AAA", 9, 11))
	r = eng.Process(event.Event{EventID: "e5", Seq: 6, Kind: event.KindOrderSubmit,
		Symbol: "AAA", OrderID: "o5", Side: event.Buy, Type: event.Limit, Price: 10, Qty: 10})[0]
	ok(r.Reject == nil)
	view, exists := eng.Order("o5")
	ok(exists && view.Status == StatusNew && view.TIF == event.TGTC)

	// 重复 OrderID 拒单。
	r = eng.Process(event.Event{EventID: "e6", Seq: 7, Kind: event.KindOrderSubmit,
		Symbol: "AAA", OrderID: "o5", Side: event.Buy, Type: event.Limit, Price: 10, Qty: 10})[0]
	ok(r.Reject != nil && r.Reject.Reason == RejectDuplicateOrderID)

	// 未知订单撤单。
	r = eng.Process(event.Event{EventID: "e7", Seq: 8, Kind: event.KindOrderCancel,
		Symbol: "AAA", OrderID: "nope"})[0]
	ok(r.Reject != nil && r.Reject.Reason == RejectUnknownOrder)
}

// 买入资金不足、卖出持仓不足沿用旧拒单原因，且不留下冻结。
func TestLegacyInsufficientFundsAndPosition(t *testing.T) {
	eng, ok := newTestEngine(t, 500)
	eng.Process(q("q1", 1, "AAA", 9, 11))

	r := eng.Process(event.Event{EventID: "e1", Seq: 2, Kind: event.KindOrderSubmit,
		Symbol: "AAA", OrderID: "poor", Side: event.Buy, Type: event.Limit, Price: 100, Qty: 10})[0]
	ok(r.Reject != nil && r.Reject.Reason == RejectInsufficientCash)
	snap, err := eng.Snapshot()
	ok(err == nil && snap.Portfolio.FrozenCash == 0 && snap.Portfolio.Cash == 500)

	r = eng.Process(event.Event{EventID: "e2", Seq: 3, Kind: event.KindOrderSubmit,
		Symbol: "AAA", OrderID: "noshare", Side: event.Sell, Type: event.Limit, Price: 100, Qty: 1})[0]
	ok(r.Reject != nil && r.Reject.Reason == RejectInsufficientPos)
	pos := snap.Portfolio.Positions["AAA"]
	ok(pos.Qty == 0 && pos.Frozen == 0)
}

// 市价单无行情 -> NO_MARKET_DATA；部分成交后剩余 -> 撤销且冻结释放。
func TestLegacyMarketOrderRules(t *testing.T) {
	eng, ok := newTestEngine(t, 100_000)

	r := eng.Process(event.Event{EventID: "e1", Seq: 1, Kind: event.KindOrderSubmit,
		Symbol: "AAA", OrderID: "m0", Side: event.Buy, Type: event.Market, Qty: 10})[0]
	ok(r.Reject != nil && r.Reject.Reason == RejectNoMarketData)

	// 准备卖盘：持仓来自一笔限价卖单需要先有仓位；直接用两个账户的对手单：
	// 先建仓（买），再挂卖，再市价吃。
	eng.Process(q("q1", 2, "AAA", 99, 101))
	eng.Process(event.Event{EventID: "e2", Seq: 3, Kind: event.KindOrderSubmit,
		Symbol: "AAA", OrderID: "hold", Side: event.Buy, Type: event.Limit, Price: 101, Qty: 100})
	// hold 在 ask=101 全部成交
	hv, _ := eng.Order("hold")
	ok(hv.Status == StatusFilled && hv.FilledQty == 100)

	eng.Process(event.Event{EventID: "e3", Seq: 4, Kind: event.KindOrderSubmit,
		Symbol: "AAA", OrderID: "ask1", Side: event.Sell, Type: event.Limit, Price: 102, Qty: 30})
	eng.Process(event.Event{EventID: "e4", Seq: 5, Kind: event.KindOrderSubmit,
		Symbol: "AAA", OrderID: "ask2", Side: event.Sell, Type: event.Limit, Price: 103, Qty: 30})

	r = eng.Process(event.Event{EventID: "e5", Seq: 6, Kind: event.KindOrderSubmit,
		Symbol: "AAA", OrderID: "m1", Side: event.Buy, Type: event.Market, Qty: 50})[0]
	// 只能吃到 30+30=60，这里应全成 50。
	ok(r.Reject == nil && len(r.Fills) >= 2)

	// 再来一张只能部分成交的：卖盘剩余 10。
	r = eng.Process(event.Event{EventID: "e6", Seq: 7, Kind: event.KindOrderSubmit,
		Symbol: "AAA", OrderID: "m2", Side: event.Buy, Type: event.Market, Qty: 40})[0]
	v, _ := eng.Order("m2")
	ok(v.Status == StatusCanceled && v.FilledQty == 10)
	snap, err := eng.Snapshot()
	ok(err == nil && snap.Portfolio.FrozenCash == 0)
}

// 终态订单再撤按老规矩 TERMINAL_ORDER；撤单数量超限拒绝。
func TestLegacyCancelRules(t *testing.T) {
	eng, ok := newTestEngine(t, 100_000)
	eng.Process(q("q1", 1, "AAA", 99, 101))
	eng.Process(event.Event{EventID: "e1", Seq: 2, Kind: event.KindOrderSubmit,
		Symbol: "AAA", OrderID: "b1", Side: event.Buy, Type: event.Limit, Price: 95, Qty: 100})

	r := eng.Process(event.Event{EventID: "e2", Seq: 3, Kind: event.KindOrderCancel,
		Symbol: "AAA", OrderID: "b1", Qty: 200})[0]
	ok(r.Reject != nil && r.Reject.Reason == RejectCancelExceedQty)

	eng.Process(event.Event{EventID: "e3", Seq: 4, Kind: event.KindOrderCancel,
		Symbol: "AAA", OrderID: "b1"})
	v, _ := eng.Order("b1")
	ok(v.Status == StatusCanceled)

	r = eng.Process(event.Event{EventID: "e4", Seq: 5, Kind: event.KindOrderCancel,
		Symbol: "AAA", OrderID: "b1"})[0]
	ok(r.Reject != nil && r.Reject.Reason == RejectTerminalOrder)
}

// 乱序、重复投递：相同 EventID 幂等，按 (Seq, EventID) 确定序，结果一致。
func TestLegacyIdempotencyAndDeterminism(t *testing.T) {
	build := func() (*Engine, []Result) {
		pf := portfolio.New(100_000, 0)
		e := New(pf)
		evs := []event.Event{
			q("q1", 1, "AAA", 99, 101),
			{EventID: "b1", Seq: 3, Kind: event.KindOrderSubmit, Symbol: "AAA",
				OrderID: "b1", Side: event.Buy, Type: event.Limit, Price: 100, Qty: 10},
			tr("t1", 2, "AAA", 100, 50),
			q("q1", 1, "AAA", 99, 101), // 重复
			{EventID: "b1", Seq: 3, Kind: event.KindOrderSubmit, Symbol: "AAA",
				OrderID: "b1", Side: event.Buy, Type: event.Limit, Price: 100, Qty: 10},
			{EventID: "", Seq: 4, Kind: event.KindQuote, Symbol: "AAA",
				BidPrice: 1, AskPrice: 2}, // 非法 ID
		}
		return e, e.Process(evs...)
	}

	e1, r1 := build()
	e2, r2 := build()
	ok := group(t, grpLegacy)
	ok(len(r1) == len(r2))
	v1, _ := e1.Order("b1")
	v2, _ := e2.Order("b1")
	ok(v1.Status == v2.Status && v1.FilledQty == v2.FilledQty)
	s1, _ := e1.Snapshot()
	s2, _ := e2.Snapshot()
	ok(approx(s1.Portfolio.Equity, s2.Portfolio.Equity) && s1.Portfolio.FrozenCash == s2.Portfolio.FrozenCash)

	// 同一批结果数 = 事件数（含重复，重复返回缓存结果）。
	ok(len(r1) == 6)
	// b1 在 seq3 时行情为 100，限价 100 未穿越（bid=99/ask=101），保持挂单。
	ok(v1.Status == StatusNew)
}

func approx(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-6
}
