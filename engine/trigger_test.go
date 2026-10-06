package engine

import (
	"testing"

	"github.com/lechandonga/backtest-matching-engine-go/event"
	"github.com/lechandonga/backtest-matching-engine-go/portfolio"
)

func newTriggerEngine(t *testing.T, cash float64) (*Engine, func(bool)) {
	t.Helper()
	eng := New(portfolio.New(cash, 0))
	return eng, group(t, grpTrigger)
}

func submitEv(id string, seq int64, sym string, side event.Side, typ event.OrderType,
	price, stopPx float64, qty int64, tif event.TIF) event.Event {
	return event.Event{EventID: "ev-" + id, Seq: seq, Kind: event.KindOrderSubmit,
		Symbol: sym, OrderID: id, Side: side, Type: typ, Price: price,
		StopPrice: stopPx, Qty: qty, TIF: tif}
}

func cancelEv(id string, seq int64, sym string) event.Event {
	return event.Event{EventID: "cx-" + id + "-" + itoa(seq), Seq: seq, Kind: event.KindOrderCancel,
		Symbol: sym, OrderID: id}
}

func TestStopWaitsBeforeTrigger(t *testing.T) {
	eng, ok := newTriggerEngine(t, 10_000)
	eng.Process(q("q1", 1, "AAA", 99, 101))
	r := eng.Process(submitEv("stop1", 2, "AAA", event.Buy, event.Stop, 0, 105, 10, event.TGTC))[0]
	ok(r.Reject == nil && len(r.Fills) == 0)

	snap, err := eng.Snapshot()
	ok(err == nil)
	ok(snap.Portfolio.FrozenCash == 0 && snap.Portfolio.Cash == 10_000)
	ok(len(snap.WaitingTrigger) == 1 && len(snap.OpenOrders) == 0)

	eng.Process(q("q2", 3, "AAA", 100, 104), q("q3", 4, "AAA", 90, 92))
	v, _ := eng.Order("stop1")
	ok(v.Status == StatusWaitingTrigger)
}

func TestStopBuyTriggersOnce(t *testing.T) {
	eng, ok := newTriggerEngine(t, 10_000_000)
	eng.Process(q("q1", 1, "AAA", 99, 101))
	buildSellWall(t, eng, 2, "AAA", 106, 500, 200)
	eng.Process(submitEv("stop1", 4, "AAA", event.Buy, event.Stop, 0, 105, 20, event.TGTC))

	r := eng.Process(
		q("q2", 5, "AAA", 103, 105),
		q("q3", 6, "AAA", 90, 91),
		q("q4", 7, "AAA", 105, 105.5),
	)
	fillEvents, totalFills := 0, 0
	for _, rr := range r {
		if len(rr.Fills) > 0 {
			fillEvents++
		}
		totalFills += len(rr.Fills)
	}
	v, _ := eng.Order("stop1")
	ok(v.Status == StatusFilled && v.FilledQty == 20)
	ok(v.TriggerPrice == 105 && v.TriggeredSeq == 5)
	ok(fillEvents == 1 && totalFills == 2)
	snap, err := eng.Snapshot()
	ok(err == nil && snap.Portfolio.FrozenCash == 0)
}

func TestStopSellTriggers(t *testing.T) {
	eng, ok := newTriggerEngine(t, 10_000_000)
	eng.Process(q("q1", 1, "AAA", 99, 101))
	eng.Process(submitEv("hold", 2, "AAA", event.Buy, event.Limit, 101, 0, 100, event.TGTC))
	buildBuyWall(t, eng, 3, "AAA", 95, 50, "bidwall")
	eng.Process(submitEv("sstop", 4, "AAA", event.Sell, event.Stop, 0, 95, 50, event.TGTC))
	r := eng.Process(q("q2", 5, "AAA", 95, 96))[0]
	ok(len(r.Fills) == 2)
	v, _ := eng.Order("sstop")
	ok(v.Status == StatusFilled && v.TriggerPrice == 95)
}

func TestStopLimitBehavior(t *testing.T) {
	eng, ok := newTriggerEngine(t, 10_000_000)
	eng.Process(q("q1", 1, "AAA", 99, 101))
	eng.Process(submitEv("hold", 2, "AAA", event.Buy, event.Limit, 101, 0, 100, event.TGTC))
	eng.Process(submitEv("sl", 3, "AAA", event.Sell, event.StopLimit, 97, 95, 30, event.TGTC))

	eng.Process(q("q2", 4, "AAA", 95, 96)) // 买价 95 触及触发价
	v, _ := eng.Order("sl")
	ok(v.Status == StatusNew && v.FilledQty == 0) // 卖限价 97 > 买价 95，挂簿

	r := eng.Process(q("q3", 5, "AAA", 93, 94))[0] // 买价跌到 93
	ok(len(r.Fills) == 0)                          // 93 仍低于 97
	r = eng.Process(q("q4", 6, "AAA", 97, 98))[0]
	ok(len(r.Fills) == 1)
	v2, _ := eng.Order("sl")
	ok(v2.Status == StatusFilled && v2.TriggerPrice == 95)
}

func TestTrailingStopSell(t *testing.T) {
	eng, ok := newTriggerEngine(t, 10_000_000)
	eng.Process(q("q1", 1, "AAA", 99, 101))
	eng.Process(submitEv("hold", 2, "AAA", event.Buy, event.Limit, 101, 0, 200, event.TGTC))

	eng.Process(event.Event{EventID: "ev-trail", Seq: 3, Kind: event.KindOrderSubmit,
		Symbol: "AAA", OrderID: "trail", Side: event.Sell, Type: event.TrailingStop,
		TrailAmount: 5, Qty: 40, TIF: event.TGTC})
	v, _ := eng.Order("trail")
	ok(v.AnchorPrice == 100)

	// 卖出跟踪看买价：买价 109 -> 锚点升到 109；买价回到 105 回撤 4 不触发。
	eng.Process(q("up", 4, "AAA", 109, 111))
	eng.Process(q("dip", 5, "AAA", 105, 106))
	v2, _ := eng.Order("trail")
	ok(v2.Status == StatusWaitingTrigger && v2.AnchorPrice == 109)

	// 买价 104 回撤 5，触发；用 104 买墙承接触发后的卖市价。
	buildBuyWall(t, eng, 6, "AAA", 104, 40, "wall")
	r := eng.Process(q("hit", 7, "AAA", 104, 105))[0]
	ok(len(r.Fills) == 2)
	v3, _ := eng.Order("trail")
	ok(v3.Status == StatusFilled && v3.AnchorPrice == 109 && v3.TriggerPrice == 104)

	eng.Process(q("moon", 8, "AAA", 200, 201))
	v4, _ := eng.Order("trail")
	ok(v4.AnchorPrice == 109)
}

func TestTrailingStopBuyPct(t *testing.T) {
	eng, ok := newTriggerEngine(t, 100_000_000)
	eng.Process(q("q1", 1, "AAA", 99, 101))
	eng.Process(submitEv("stock", 2, "AAA", event.Buy, event.Limit, 101, 0, 500, event.TGTC))

	eng.Process(event.Event{EventID: "ev-tb", Seq: 4, Kind: event.KindOrderSubmit,
		Symbol: "AAA", OrderID: "tb", Side: event.Buy, Type: event.TrailingStop,
		TrailPct: 0.05, Qty: 20, TIF: event.TGTC})

	eng.Process(q("dn", 5, "AAA", 89, 90))
	// 价格跌下来后再挂 96 的卖墙（此时买价 89 不会穿越）。
	eng.Process(submitEv("wall", 5, "AAA", event.Sell, event.Limit, 96, 0, 100, event.TGTC))
	eng.Process(q("up1", 6, "AAA", 93, 94))
	v, _ := eng.Order("tb")
	ok(v.Status == StatusWaitingTrigger && v.AnchorPrice == 90)
	r := eng.Process(q("up2", 7, "AAA", 94, 95))[0]
	ok(len(r.Fills) == 2)
	v2, _ := eng.Order("tb")
	ok(v2.Status == StatusFilled && v2.TriggerPrice == 95 && v2.AnchorPrice == 90)
}

func TestTriggerRejectedForCash(t *testing.T) {
	eng, ok := newTriggerEngine(t, 1000)
	eng.Process(q("q1", 1, "AAA", 9, 10))
	eng.Process(submitEv("stop1", 2, "AAA", event.Buy, event.Stop, 0, 10, 1000, event.TGTC))

	r := eng.Process(q("q2", 3, "AAA", 10, 10.5))[0]
	ok(r.Reject != nil && r.Reject.Reason == RejectTriggerNoCash)
	v, _ := eng.Order("stop1")
	ok(v.Status == StatusRejected && v.TriggerPrice == 10.5)
	snap, err := eng.Snapshot()
	ok(err == nil && snap.Portfolio.FrozenCash == 0 && snap.Portfolio.Cash == 1000)
	ok(len(snap.WaitingTrigger) == 0)
}

func TestTriggerRejectedForPosition(t *testing.T) {
	eng, ok := newTriggerEngine(t, 1_000_000)
	eng.Process(q("q1", 1, "AAA", 99, 101))
	eng.Process(submitEv("sstop", 2, "AAA", event.Sell, event.Stop, 0, 95, 10, event.TGTC))

	r := eng.Process(q("q2", 3, "AAA", 94, 95))[0]
	ok(r.Reject != nil && r.Reject.Reason == RejectTriggerNoPos)
	v, _ := eng.Order("sstop")
	ok(v.Status == StatusRejected && v.TriggerPrice == 94)
	snap, err := eng.Snapshot()
	ok(err == nil && snap.Portfolio.Positions["AAA"].Frozen == 0)
	ok(len(snap.WaitingTrigger) == 0)
}

func TestTriggerCancelRules(t *testing.T) {
	eng, ok := newTriggerEngine(t, 10_000_000)
	eng.Process(q("q1", 1, "AAA", 99, 101))
	eng.Process(submitEv("stop1", 2, "AAA", event.Buy, event.Stop, 0, 105, 10, event.TGTC))

	r := eng.Process(cancelEv("stop1", 3, "AAA"))[0]
	ok(r.Reject == nil)
	v, _ := eng.Order("stop1")
	ok(v.Status == StatusCanceled)
	snap, _ := eng.Snapshot()
	ok(len(snap.WaitingTrigger) == 0)

	r = eng.Process(cancelEv("stop1", 4, "AAA"))[0]
	ok(r.Reject != nil && r.Reject.Reason == RejectTerminalOrder)

	eng.Process(submitEv("hold", 5, "AAA", event.Buy, event.Limit, 101, 0, 50, event.TGTC))
	buildBuyWall(t, eng, 6, "AAA", 94, 10, "bwall")
	eng.Process(submitEv("sstop", 7, "AAA", event.Sell, event.Stop, 0, 95, 10, event.TGTC))
	eng.Process(q("q2", 8, "AAA", 94, 95))
	v2, _ := eng.Order("sstop")
	ok(v2.Status == StatusFilled)
	r = eng.Process(cancelEv("sstop", 9, "AAA"))[0]
	ok(r.Reject != nil && r.Reject.Reason == RejectTerminalOrder)
}

func TestTriggerDeterminismUnderReorder(t *testing.T) {
	run := func() (OrderView, float64, float64, int) {
		eng := New(portfolio.New(10_000_000, 0))
		evs := []event.Event{
			q("q1", 1, "AAA", 99, 101),
			submitEv("stop1", 4, "AAA", event.Buy, event.Stop, 0, 105, 20, event.TGTC),
			q("q4", 7, "AAA", 107, 109), // 乱序
			q("q2", 5, "AAA", 103, 105), // 触发
			q("q2", 5, "AAA", 103, 105), // 重复 EventID
			q("q4", 7, "AAA", 107, 109),
			q("q3", 6, "AAA", 90, 91),
		}
		// 卖墙必须先于触发挂好：放在 seq2
		eng.Submit(buildSellWallEvents(2, "AAA", 106, 200, 100)...)
		rs := eng.Process(evs...)
		n := 0
		for _, r := range rs {
			n += len(r.Fills)
		}
		v, _ := eng.Order("stop1")
		s, _ := eng.Snapshot()
		return v, s.Portfolio.Equity, s.Portfolio.FrozenCash, n
	}
	v1, eq1, fz1, n1 := run()
	v2, eq2, fz2, n2 := run()
	ok := group(t, grpTrigger)
	ok(v1.Status == StatusFilled && v1.TriggeredSeq == 5 && v1.TriggerPrice == 105)
	ok(v1.FilledQty == v2.FilledQty && n1 == n2)
	ok(approx(eq1, eq2) && approx(fz1, fz2) && fz1 == 0)
}

func TestTriggerIsolationAcrossSymbols(t *testing.T) {
	eng, ok := newTriggerEngine(t, 10_000_000)
	eng.Process(
		q("qa", 1, "AAA", 99, 101),
		q("qb", 1, "BBB", 49, 51),
		submitEv("ha", 2, "AAA", event.Buy, event.Limit, 101, 0, 100, event.TGTC),
		submitEv("hb", 2, "BBB", event.Buy, event.Limit, 51, 0, 100, event.TGTC),
	)
	buildBuyWall(t, eng, 3, "BBB", 44, 20, "bbbwall")
	eng.Process(
		submitEv("sa", 4, "AAA", event.Sell, event.Stop, 0, 95, 20, event.TGTC),
		submitEv("sb", 4, "BBB", event.Sell, event.Stop, 0, 45, 20, event.TGTC),
	)
	eng.Process(q("bb", 5, "BBB", 44, 46))
	va, _ := eng.Order("sa")
	vb, _ := eng.Order("sb")
	ok(va.Status == StatusWaitingTrigger && vb.Status == StatusFilled)

	buildBuyWall(t, eng, 6, "AAA", 94, 20, "aaawall")
	eng.Process(q("aa", 7, "AAA", 94, 96))
	va2, _ := eng.Order("sa")
	ok(va2.Status == StatusFilled)
}
