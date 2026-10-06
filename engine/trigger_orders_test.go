package engine

import (
	"testing"

	"github.com/lechandonga/backtest-matching-engine-go/event"
	"github.com/lechandonga/backtest-matching-engine-go/portfolio"
)

func stopSubmit(id string, seq int64, symbol string, side event.Side, typ event.OrderType, qty int64, stop, limit float64, tif event.TimeInForce) event.Event {
	ev := submitEvent(id, seq, symbol, side, typ, qty, limit, tif)
	ev.StopPrice = stop
	return ev
}

func trailSubmit(id string, seq int64, side event.Side, qty int64, distance, pct float64) event.Event {
	ev := submitEvent(id, seq, "AAA", side, event.TrailingStop, qty, 0, event.GTC)
	ev.TrailDistance = distance
	ev.TrailPct = pct
	return ev
}

func init() {
	register("触发型委托", "触发前不撮合不冻结且快照可见WAITING_TRIGGER", func(t *testing.T) {
		e := newTestEngine(100_000)
		mustAccept(t, e.ProcessOne(stopSubmit("s1", 1, "AAA", event.Sell, event.StopMarket, 10, 9, 0, event.GTC)))
		e.ProcessOne(tradeEvent("t1", 2, "AAA", 100, 50, "D1")) // 价格远在上方，不应触发
		if got := statusOf(e, "ord-s1"); got != StatusWaitingTrigger {
			t.Fatalf("status=%s", got)
		}
		snap, orders, err := e.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if snap.FrozenCash != 0 {
			t.Fatalf("trigger order must not freeze cash before trigger: %.2f", snap.FrozenCash)
		}
		var seen bool
		for _, o := range orders {
			if o.ID == "ord-s1" && o.Status == StatusWaitingTrigger {
				seen = true
			}
		}
		if !seen {
			t.Fatal("waiting-trigger order missing from snapshot")
		}
	})

	register("触发型委托", "止损单只触发一次且重复乱序行情结果一致", func(t *testing.T) {
		run := func(order []event.Event) []Fill {
			e := newTestEngine(100_000)
			e.ProcessOne(quoteEvent("q0", 1, "AAA", 99, 101, 200, 200, "D1"))
			e.ProcessOne(submitEvent("build", 0, "AAA", event.Buy, event.Market, 10, 0, event.IOC))
			mustAccept(t, e.ProcessOne(stopSubmit("s1", 2, "AAA", event.Sell, event.StopMarket, 10, 90, 0, event.GTC)))
			hit1 := tradeEvent("hit1", 3, "AAA", 89, 20, "D1")
			hit2 := tradeEvent("hit2", 4, "AAA", 88, 20, "D1")
			rs := e.Process([]event.Event{hit2, hit1, hit1, hit2})
			var fills []Fill
			for _, r := range rs {
				fills = append(fills, r.Fills...)
			}
			if got := statusOf(e, "ord-s1"); got != StatusFilled {
				t.Fatalf("status=%s", got)
			}
			snap, _, err := e.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			if snap.FrozenCash != 0 {
				t.Fatalf("leftover frozen cash %.4f", snap.FrozenCash)
			}
			// 重复投递的触发事件各自回放首次结果，但底层委托只被触发、成交一次。
			o := e.orders["ord-s1"]
			if o.FilledQty != 10 {
				t.Fatalf("filled qty=%d, want 10 (trigger exactly once)", o.FilledQty)
			}
			return fills
		}
		f1 := run(nil)
		f2 := run([]event.Event{})
		if len(f1) != 2 || len(f2) != 2 || f1[0].Price != f2[0].Price || f1[0].Qty != 10 {
			t.Fatalf("trigger fill not deterministic: %+v vs %+v", f1, f2)
		}
	})

	register("触发型委托", "止损限价触发后按限价路径挂单成交", func(t *testing.T) {
		e := newTestEngine(100_000)
		e.ProcessOne(quoteEvent("q0", 1, "AAA", 88, 101, 200, 200, "D1"))
		e.ProcessOne(submitEvent("build", 0, "AAA", event.Buy, event.Market, 10, 0, event.IOC))
		mustAccept(t, e.ProcessOne(stopSubmit("s1", 2, "AAA", event.Sell, event.StopLimit, 10, 90, 92, event.GTC)))
		e.ProcessOne(tradeEvent("hit", 3, "AAA", 89, 20, "D1"))          // 触发，但 89 < 限价92，不成交
		e.ProcessOne(quoteEvent("q1", 4, "AAA", 88, 91, 200, 200, "D1")) // 买一仍低于限价 92
		if got := statusOf(e, "ord-s1"); got != StatusNew {
			t.Fatalf("after trigger status=%s want NEW", got)
		}
		r := e.ProcessOne(tradeEvent("fill", 5, "AAA", 92, 20, "D1"))
		if len(r.Fills) != 1 || r.Fills[0].Price != 92 {
			t.Fatalf("fills=%+v", r.Fills)
		}
	})

	register("触发型委托", "跟踪止损基准只朝有利方向走且回撤触发", func(t *testing.T) {
		e := newTestEngine(100_000)
		// 先建仓：100 股 @100
		e.ProcessOne(quoteEvent("q0", 1, "AAA", 99, 101, 100, 100, "D1"))
		buy := submitEvent("b1", 2, "AAA", event.Buy, event.Market, 100, 0, event.IOC)
		r := e.ProcessOne(buy)
		if len(r.Fills) != 1 || r.Fills[0].Price != 101 {
			t.Fatalf("setup fill=%+v", r.Fills)
		}
		// 跟踪止损 5%，提交时锚定 101
		mustAccept(t, e.ProcessOne(trailSubmit("tr1", 3, event.Sell, 100, 0, 0.05)))
		e.ProcessOne(tradeEvent("u1", 4, "AAA", 110, 10, "D1")) // 有利：锚点上移到 110
		e.ProcessOne(tradeEvent("u2", 5, "AAA", 112, 10, "D1")) // 锚点 112
		e.ProcessOne(tradeEvent("d1", 6, "AAA", 108, 10, "D1")) // 回撤未到 5%(106.4)
		if got := statusOf(e, "ord-tr1"); got != StatusWaitingTrigger {
			t.Fatalf("should not trigger yet, status=%s", got)
		}
		e.ProcessOne(tradeEvent("u3", 7, "AAA", 109, 10, "D1"))        // 反弹不应抬高锚点
		hit := e.ProcessOne(tradeEvent("d2", 8, "AAA", 106, 10, "D1")) // <=112*0.95=106.4 触发
		if len(hit.Fills) != 1 || hit.Fills[0].Qty != 100 {
			t.Fatalf("fills=%+v", hit.Fills)
		}
		if got := statusOf(e, "ord-tr1"); got != StatusFilled {
			t.Fatalf("status=%s", got)
		}
		o := e.orders["ord-tr1"]
		if o.TrailAnchor != 112 {
			t.Fatalf("anchor=%.2f want 112 (frozen after trigger)", o.TrailAnchor)
		}
	})

	register("触发型委托", "跟踪止损绝对价差触发", func(t *testing.T) {
		e := newTestEngine(100_000)
		e.ProcessOne(quoteEvent("q0", 1, "AAA", 100, 100, 50, 50, "D1"))
		e.ProcessOne(buyLike("b1", 2))
		mustAccept(t, e.ProcessOne(trailSubmit("tr1", 3, event.Sell, 50, 3, 0)))
		e.ProcessOne(tradeEvent("u1", 4, "AAA", 105, 10, "D1"))
		if got := statusOf(e, "ord-tr1"); got != StatusWaitingTrigger {
			t.Fatalf("status=%s", got)
		}
		r := e.ProcessOne(tradeEvent("d1", 5, "AAA", 102, 10, "D1")) // 105-3=102 触发
		if len(r.Fills) != 1 {
			t.Fatalf("fills=%+v", r.Fills)
		}
	})

	register("触发型委托", "触发时持仓不足直接拒单且不留下冻结", func(t *testing.T) {
		e := newTestEngine(100_000)
		e.ProcessOne(quoteEvent("q0", 1, "AAA", 99, 101, 100, 100, "D1"))
		e.ProcessOne(submitEvent("b1", 2, "AAA", event.Buy, event.Market, 50, 0, event.IOC))
		// 止损卖 100 股，但只有 50 股
		mustAccept(t, e.ProcessOne(stopSubmit("s1", 3, "AAA", event.Sell, event.StopMarket, 100, 90, 0, event.GTC)))
		e.ProcessOne(tradeEvent("hit", 4, "AAA", 89, 100, "D1"))
		o := e.orders["ord-s1"]
		if o.Status != StatusRejected || o.RejectReason != RejectTriggerNoPosition {
			t.Fatalf("status=%s reason=%s", o.Status, o.RejectReason)
		}
		snap, _, err := e.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		pos := snap.Positions["AAA"]
		if pos.Frozen != 0 || pos.Qty != 50 {
			t.Fatalf("position not clean: %+v", pos)
		}
	})

	register("触发型委托", "触发时资金不足直接拒单原因可区分", func(t *testing.T) {
		e := New(portfolio.New(0, 0))
		e.ProcessOne(quoteEvent("q0", 1, "AAA", 100, 100, 50, 50, "D1"))
		// 买入止损：现金为 0，触发时冻结失败
		mustAccept(t, e.ProcessOne(stopSubmit("s1", 2, "AAA", event.Buy, event.StopMarket, 10, 110, 0, event.GTC)))
		e.ProcessOne(tradeEvent("hit", 3, "AAA", 111, 100, "D1"))
		o := e.orders["ord-s1"]
		if o.Status != StatusRejected || o.RejectReason != RejectTriggerNoCash {
			t.Fatalf("status=%s reason=%s", o.Status, o.RejectReason)
		}
		snap, _, err := e.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if snap.FrozenCash != 0 || snap.Cash != 0 {
			t.Fatalf("cash not clean: %+v", snap)
		}
	})

	register("触发型委托", "待触发委托可以撤销并释放状态", func(t *testing.T) {
		e := newTestEngine(100_000)
		mustAccept(t, e.ProcessOne(stopSubmit("s1", 1, "AAA", event.Sell, event.StopMarket, 10, 90, 0, event.GTC)))
		cx := cancelEvent("c1", 2, "ord-s1", 0, "D1")
		mustAccept(t, e.ProcessOne(cx))
		if got := statusOf(e, "ord-s1"); got != StatusCanceled {
			t.Fatalf("status=%s", got)
		}
		// 撤销后再来行情不触发
		r := e.ProcessOne(tradeEvent("hit", 3, "AAA", 80, 100, "D1"))
		if len(r.Fills) != 0 {
			t.Fatalf("canceled order must not fill: %+v", r.Fills)
		}
	})

	register("触发型委托", "不同标的触发互不影响", func(t *testing.T) {
		e := newTestEngine(100_000)
		e.ProcessOne(quoteEvent("qa", 0, "AAA", 99, 101, 200, 200, "D1"))
		e.ProcessOne(quoteEvent("qb", 0, "BBB", 99, 101, 200, 200, "D1"))
		ba := submitEvent("ba", 0, "AAA", event.Buy, event.Market, 10, 0, event.IOC)
		bb := submitEvent("bb", 0, "BBB", event.Buy, event.Market, 10, 0, event.IOC)
		e.ProcessOne(ba)
		e.ProcessOne(bb)
		mustAccept(t, e.ProcessOne(withSymbol(stopSubmit("a", 1, "AAA", event.Sell, event.StopMarket, 10, 90, 0, event.GTC), "AAA")))
		mustAccept(t, e.ProcessOne(withSymbol(stopSubmit("b", 2, "BBB", event.Sell, event.StopMarket, 10, 90, 0, event.GTC), "BBB")))
		e.ProcessOne(tradeEvent("hita", 3, "AAA", 89, 100, "D1"))
		if statusOf(e, "ord-a") != StatusFilled || statusOf(e, "ord-b") != StatusWaitingTrigger {
			t.Fatalf("a=%s b=%s", statusOf(e, "ord-a"), statusOf(e, "ord-b"))
		}
	})

	register("触发型委托", "已结束委托再撤单按老规矩拒掉", func(t *testing.T) {
		e := newTestEngine(100_000)
		e.ProcessOne(quoteEvent("q0", 1, "AAA", 100, 100, 50, 50, "D1"))
		e.ProcessOne(submitEvent("build", 0, "AAA", event.Buy, event.Market, 10, 0, event.IOC))
		e.ProcessOne(stopSubmit("s1", 2, "AAA", event.Sell, event.StopMarket, 10, 100, 0, event.GTC))
		e.ProcessOne(tradeEvent("hit", 3, "AAA", 99, 100, "D1"))
		cx := cancelEvent("c1", 4, "ord-s1", 0, "D1")
		mustReject(t, e.ProcessOne(cx), RejectTerminalOrder)
	})
}

func withSymbol(ev event.Event, symbol string) event.Event {
	ev.Symbol = symbol
	return ev
}

func buyLike(id string, seq int64) event.Event {
	return submitEvent(id, seq, "AAA", event.Buy, event.Market, 50, 0, event.IOC)
}
