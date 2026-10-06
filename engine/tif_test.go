package engine

import (
	"testing"

	"github.com/lechandonga/backtest-matching-engine-go/event"
)

func init() {
	register("有效期", "FOK可全成时一次全成", func(t *testing.T) {
		e := newTestEngine(1_000_000)
		e.ProcessOne(quoteEvent("q1", 1, "AAA", 9, 10, 100, 100, "D1"))
		r := e.ProcessOne(submitEvent("f1", 2, "AAA", event.Buy, event.Limit, 100, 10, event.FOK))
		mustAccept(t, r)
		if len(r.Fills) != 1 || r.Fills[0].Qty != 100 {
			t.Fatalf("fills=%+v", r.Fills)
		}
		if statusOf(e, "ord-f1") != StatusFilled {
			t.Fatal("FOK should fill fully")
		}
	})

	register("有效期", "FOK不能全成时整单一点不成无中间成交", func(t *testing.T) {
		e := newTestEngine(1_000_000)
		e.ProcessOne(quoteEvent("q1", 1, "AAA", 9, 10, 60, 60, "D1"))
		r := e.ProcessOne(submitEvent("f1", 2, "AAA", event.Buy, event.Limit, 100, 10, event.FOK))
		mustAccept(t, r)
		if len(r.Fills) != 0 {
			t.Fatalf("FOK must not partially fill: %+v", r.Fills)
		}
		o := e.orders["ord-f1"]
		if o.Status != StatusCanceled || o.RejectReason != RejectFOKNotFilled || o.FilledQty != 0 {
			t.Fatalf("status=%s reason=%s filled=%d", o.Status, o.RejectReason, o.FilledQty)
		}
		snap, _, err := e.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if snap.FrozenCash != 0 {
			t.Fatalf("frozen cash leaked: %.2f", snap.FrozenCash)
		}
	})

	register("有效期", "IOC能成多少成多少剩余撤销并释放冻结", func(t *testing.T) {
		e := newTestEngine(1_000_000)
		e.ProcessOne(quoteEvent("q1", 1, "AAA", 9, 10, 60, 60, "D1"))
		r := e.ProcessOne(submitEvent("i1", 2, "AAA", event.Buy, event.Limit, 100, 10, event.IOC))
		mustAccept(t, r)
		if len(r.Fills) != 1 || r.Fills[0].Qty != 60 {
			t.Fatalf("fills=%+v", r.Fills)
		}
		o := e.orders["ord-i1"]
		if o.Status != StatusCanceled || o.FilledQty != 60 || o.RemainingQty() != 0 {
			t.Fatalf("status=%s filled=%d rem=%d", o.Status, o.FilledQty, o.RemainingQty())
		}
		snap, _, err := e.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if snap.FrozenCash != 0 {
			t.Fatalf("frozen cash leaked: %.2f", snap.FrozenCash)
		}
	})

	register("有效期", "DAY当日单日终过期", func(t *testing.T) {
		e := newTestEngine(1_000_000)
		mustAccept(t, e.ProcessOne(submitEvent("d1", 1, "AAA", event.Buy, event.Limit, 100, 10, event.Day)))
		e.ProcessOne(closeEvent("c1", 2, "AAA", 10, "D1"))
		o := e.orders["ord-d1"]
		if o.Status != StatusExpired {
			t.Fatalf("status=%s", o.Status)
		}
		snap, _, err := e.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if snap.FrozenCash != 0 {
			t.Fatalf("frozen not released: %.2f", snap.FrozenCash)
		}
	})

	register("有效期", "过期后再来成交或撤单都被拒", func(t *testing.T) {
		e := newTestEngine(1_000_000)
		e.ProcessOne(submitEvent("d1", 1, "AAA", event.Buy, event.Limit, 100, 10, event.Day))
		e.ProcessOne(closeEvent("c1", 2, "AAA", 10, "D1"))
		cx := cancelEvent("cx", 3, "ord-d1", 0, "D1")
		mustReject(t, e.ProcessOne(cx), RejectExpiredOrder)
		// 收盘后再来行情也被拒
		mustReject(t, e.ProcessOne(tradeEvent("t2", 4, "AAA", 9, 100, "D1")), RejectSessionClosed)
	})

	register("有效期", "GTC跨日保留且可继续成交", func(t *testing.T) {
		e := newTestEngine(1_000_000)
		e.ProcessOne(submitEvent("g1", 1, "AAA", event.Buy, event.Limit, 100, 10, event.GTC))
		e.ProcessOne(closeEvent("c1", 2, "AAA", 10, "D1"))
		if statusOf(e, "ord-g1") != StatusNew {
			t.Fatalf("GTC should survive close, got %s", statusOf(e, "ord-g1"))
		}
		r := e.ProcessOne(tradeEvent("t1", 3, "AAA", 9, 100, "D2"))
		if len(r.Fills) != 1 || statusOf(e, "ord-g1") != StatusFilled {
			t.Fatalf("fills=%+v status=%s", r.Fills, statusOf(e, "ord-g1"))
		}
	})

	register("有效期", "条件单DAY当日未触发日终失效", func(t *testing.T) {
		e := newTestEngine(1_000_000)
		e.ProcessOne(quoteEvent("q0", 0, "AAA", 100, 100, 50, 50, "D1"))
		ev := stopSubmit("s1", 1, "AAA", event.Buy, event.StopMarket, 10, 110, 0, event.Day)
		mustAccept(t, e.ProcessOne(ev))
		e.ProcessOne(closeEvent("c1", 2, "AAA", 100, "D1"))
		if statusOf(e, "ord-s1") != StatusExpired {
			t.Fatalf("status=%s", statusOf(e, "ord-s1"))
		}
		// 次日即使价格穿越也不再触发
		r := e.ProcessOne(tradeEvent("hit", 3, "AAA", 111, 100, "D2"))
		if len(r.Fills) != 0 {
			t.Fatalf("expired stop order triggered next day: %+v", r.Fills)
		}
	})

	register("有效期", "TIF非法被拒", func(t *testing.T) {
		ev := submitEvent("x1", 1, "AAA", event.Buy, event.Limit, 10, 10, event.TimeInForce(99))
		e := newTestEngine(1_000_000)
		mustReject(t, e.ProcessOne(ev), RejectInvalidTIF)
	})
}
