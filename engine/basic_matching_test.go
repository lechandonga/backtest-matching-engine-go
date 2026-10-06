package engine

import (
	"testing"

	"github.com/lechandonga/backtest-matching-engine-go/event"
)

func init() {
	register("基础撮合", "限价买单按成交价全成并对账", func(t *testing.T) {
		e := newTestEngine(100_000)
		mustAccept(t, e.ProcessOne(submitEvent("b1", 1, "AAA", event.Buy, event.Limit, 100, 10, event.Day)))
		r := e.ProcessOne(tradeEvent("t1", 2, "AAA", 9.5, 200, "D1"))
		mustAccept(t, r)
		if len(r.Fills) != 1 || r.Fills[0].Qty != 100 || r.Fills[0].Price != 9.5 {
			t.Fatalf("unexpected fills: %+v", r.Fills)
		}
		if got := statusOf(e, "ord-b1"); got != StatusFilled {
			t.Fatalf("status=%s", got)
		}
		snap, _, err := e.Snapshot()
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if snap.Cash != 100_000-950 {
			t.Fatalf("cash=%.2f", snap.Cash)
		}
	})

	register("基础撮合", "限价未穿越不成交", func(t *testing.T) {
		e := newTestEngine(100_000)
		mustAccept(t, e.ProcessOne(submitEvent("b1", 1, "AAA", event.Buy, event.Limit, 100, 9, event.Day)))
		r := e.ProcessOne(tradeEvent("t1", 2, "AAA", 9.5, 200, "D1"))
		mustAccept(t, r)
		if len(r.Fills) != 0 {
			t.Fatalf("should not fill: %+v", r.Fills)
		}
		if got := statusOf(e, "ord-b1"); got != StatusNew {
			t.Fatalf("status=%s", got)
		}
	})

	register("基础撮合", "买入资金不足拒单", func(t *testing.T) {
		e := newTestEngine(500)
		r := e.ProcessOne(submitEvent("b1", 1, "AAA", event.Buy, event.Limit, 100, 10, event.Day))
		mustReject(t, r, RejectInsufficientCash)
		snap, _, err := e.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if snap.Cash != 500 || snap.FrozenCash != 0 {
			t.Fatalf("cash/frozen=%.2f/%.2f", snap.Cash, snap.FrozenCash)
		}
	})

	register("基础撮合", "重复EventID返回首次结果且不二次生效", func(t *testing.T) {
		e := newTestEngine(100_000)
		ev := submitEvent("b1", 1, "AAA", event.Buy, event.Limit, 100, 10, event.Day)
		r1 := e.ProcessOne(ev)
		r2 := e.ProcessOne(ev)
		mustAccept(t, r1)
		if !r2.Duplicate || !r2.Accepted {
			t.Fatalf("duplicate result wrong: %+v", r2)
		}
		tr := tradeEvent("t1", 2, "AAA", 9.5, 100, "D1")
		e.ProcessOne(tr)
		r3 := e.ProcessOne(tr)
		if !r3.Duplicate || len(r3.Fills) != 1 {
			t.Fatalf("dup trade: %+v", r3)
		}
		if got := statusOf(e, "ord-b1"); got != StatusFilled {
			t.Fatalf("status=%s", got)
		}
	})

	register("基础撮合", "乱序批量投递与有序投递最终状态一致", func(t *testing.T) {
		run := func(shuffle bool) float64 {
			e := newTestEngine(100_000)
			evs := []event.Event{
				submitEvent("b1", 1, "AAA", event.Buy, event.Limit, 100, 10, event.Day),
				submitEvent("b2", 2, "AAA", event.Buy, event.Limit, 50, 10, event.Day),
				tradeEvent("t1", 3, "AAA", 9.5, 120, "D1"),
			}
			if shuffle {
				evs[0], evs[2] = evs[2], evs[0]
				evs[1], evs[2] = evs[2], evs[1]
			}
			e.Process(evs)
			snap, _, err := e.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			return snap.Equity
		}
		if eq1, eq2 := run(false), run(true); eq1 != eq2 {
			t.Fatalf("equity differs: %.4f vs %.4f", eq1, eq2)
		}
	})

	register("基础撮合", "并发重复投递结果确定", func(t *testing.T) {
		e := newTestEngine(1_000_000)
		var evs []event.Event
		for i := 0; i < 50; i++ {
			evs = append(evs, submitEvent(
				string(rune('a'+i%26))+itoa(i), int64(i+1), "AAA",
				event.Buy, event.Limit, 10, 10, event.Day))
		}
		tr := tradeEvent("t1", 100, "AAA", 9, 500, "D1")
		done := make(chan struct{}, 8)
		for w := 0; w < 8; w++ {
			go func() {
				defer func() { done <- struct{}{} }()
				e.Process(evs)
				e.ProcessOne(tr)
				e.ProcessOne(tr)
			}()
		}
		for w := 0; w < 8; w++ {
			<-done
		}
		snap, _, err := e.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		var frozen float64
		_ = frozen
		if snap.Equity != 1_000_000 {
			t.Fatalf("equity changed without loss: %.2f", snap.Equity)
		}
	})

	register("基础撮合", "终态委托再撤单被拒", func(t *testing.T) {
		e := newTestEngine(100_000)
		e.ProcessOne(submitEvent("b1", 1, "AAA", event.Buy, event.Limit, 100, 10, event.Day))
		e.ProcessOne(tradeEvent("t1", 2, "AAA", 9.5, 100, "D1"))
		cx := cancelEvent("c1", 3, "ord-b1", 0, "D1")
		mustReject(t, e.ProcessOne(cx), RejectTerminalOrder)
	})

	register("基础撮合", "市价单吃不完立即撤销", func(t *testing.T) {
		e := newTestEngine(1_000_000)
		e.ProcessOne(quoteEvent("q1", 1, "AAA", 9, 10, 50, 50, "D1"))
		r := e.ProcessOne(submitEvent("m1", 2, "AAA", event.Buy, event.Market, 100, 0, event.Day))
		mustAccept(t, r)
		if len(r.Fills) != 1 || r.Fills[0].Qty != 50 {
			t.Fatalf("fills=%+v", r.Fills)
		}
		if got := statusOf(e, "ord-m1"); got != StatusCanceled {
			t.Fatalf("status=%s", got)
		}
		snap, _, err := e.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if snap.FrozenCash != 0 {
			t.Fatalf("frozen not released: %.2f", snap.FrozenCash)
		}
	})

	register("基础撮合", "报价深度多委托按序共享不超卖", func(t *testing.T) {
		e := newTestEngine(1_000_000)
		// 两张各 50 股的买单排队，卖盘顶档只有 60 股。
		mustAccept(t, e.ProcessOne(submitEvent("b1", 1, "AAA", event.Buy, event.Limit, 50, 10, event.Day)))
		mustAccept(t, e.ProcessOne(submitEvent("b2", 2, "AAA", event.Buy, event.Limit, 50, 10, event.Day)))
		r := e.ProcessOne(quoteEvent("q1", 3, "AAA", 9, 10, 0, 60, "D1"))
		mustAccept(t, r)
		if e.orders["ord-b1"].FilledQty != 50 || e.orders["ord-b2"].FilledQty != 10 {
			t.Fatalf("b1=%d b2=%d", e.orders["ord-b1"].FilledQty, e.orders["ord-b2"].FilledQty)
		}
		if e.orders["ord-b2"].Status != StatusPartiallyFilled {
			t.Fatalf("b2 status=%s", e.orders["ord-b2"].Status)
		}
	})
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
