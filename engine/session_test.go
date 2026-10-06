package engine

import (
	"math"
	"sync"
	"testing"

	"github.com/lechandonga/backtest-matching-engine-go/event"
)

func init() {
	register("交易日边界", "收盘重复投递不重复结算不退钱退货", func(t *testing.T) {
		e := newTestEngine(1_000_000)
		e.ProcessOne(submitEvent("d1", 1, "AAA", event.Buy, event.Limit, 100, 10, event.Day))
		c := closeEvent("c1", 2, "AAA", 10, "D1")
		e.ProcessOne(c)
		snap1, _, _ := e.Snapshot()
		e.ProcessOne(c)
		e.ProcessOne(closeEvent("c2", 3, "AAA", 11, "D1"))
		snap2, orders, err := e.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if snap1.Cash != snap2.Cash || snap1.FrozenCash != snap2.FrozenCash {
			t.Fatalf("double settlement changed cash: %.2f/%.2f vs %.2f/%.2f",
				snap1.Cash, snap1.FrozenCash, snap2.Cash, snap2.FrozenCash)
		}
		expired := 0
		for _, o := range orders {
			if o.ID == "ord-d1" && o.Status == StatusExpired {
				expired++
			}
		}
		if expired != 1 {
			t.Fatalf("order expired %d times", expired)
		}
	})

	register("交易日边界", "收盘价用于盯市且日终快照权益对得上", func(t *testing.T) {
		e := newTestEngine(1_000_000)
		// D1 买入 100 股 @10
		e.ProcessOne(tradeEvent("t1", 1, "AAA", 10, 100, "D1"))
		e.ProcessOne(submitEvent("b1", 2, "AAA", event.Buy, event.Market, 100, 0, event.IOC))
		e.ProcessOne(closeEvent("c1", 3, "AAA", 12, "D1"))
		ds := e.GetDaySnapshot("D1")
		if ds == nil {
			t.Fatal("day snapshot missing")
		}
		// 现金 = 1e6-1000；持仓盯市 = 100*12=1200；权益 = 1e6+200（未实现盈利）
		if math.Abs(ds.Portfolio.Equity-(1_000_000+200)) > 1e-6 {
			t.Fatalf("equity=%.4f", ds.Portfolio.Equity)
		}
		if ds.Portfolio.Marks["AAA"] != 12 {
			t.Fatalf("mark=%.2f", ds.Portfolio.Marks["AAA"])
		}
	})

	register("交易日边界", "跨日隔夜仓位继续盯市权益拆解不破", func(t *testing.T) {
		e := newTestEngine(1_000_000)
		e.ProcessOne(tradeEvent("t1", 1, "AAA", 10, 100, "D1"))
		e.ProcessOne(submitEvent("b1", 2, "AAA", event.Buy, event.Market, 100, 0, event.IOC))
		e.ProcessOne(closeEvent("c1", 3, "AAA", 12, "D1"))
		// D2 价格继续变化，浮动盈亏接着盯市
		e.ProcessOne(tradeEvent("t2", 4, "AAA", 15, 10, "D2"))
		snap, _, err := e.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if math.Abs(snap.Equity-(1_000_000+500)) > 1e-6 {
			t.Fatalf("D2 equity=%.4f", snap.Equity)
		}
		e.ProcessOne(closeEvent("c2", 5, "AAA", 14, "D2"))
		ds := e.GetDaySnapshot("D2")
		if math.Abs(ds.Portfolio.Equity-(1_000_000+400)) > 1e-6 {
			t.Fatalf("D2 close equity=%.4f", ds.Portfolio.Equity)
		}
	})

	register("交易日边界", "日终失效与行情并发到达结果不随顺序变", func(t *testing.T) {
		run := func(reorder bool) float64 {
			e := newTestEngine(1_000_000)
			submit := submitEvent("d1", 1, "AAA", event.Buy, event.Limit, 100, 10, event.Day)
			tr := tradeEvent("t1", 2, "AAA", 9, 100, "D1") // Seq2：收盘前
			cl := closeEvent("c1", 3, "AAA", 9, "D1")
			evs := []event.Event{submit, tr, cl}
			if reorder {
				evs[0], evs[1], evs[2] = cl, tr, submit
			}
			e.Process(evs)
			snap, _, err := e.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			return snap.Equity
		}
		if run(false) != run(true) {
			t.Fatal("equity depends on delivery order")
		}
	})

	register("交易日边界", "并发收盘与成交只有一次结算", func(t *testing.T) {
		e := newTestEngine(1_000_000)
		e.ProcessOne(submitEvent("d1", 1, "AAA", event.Buy, event.Limit, 100, 10, event.Day))
		cl := closeEvent("c1", 5, "AAA", 10, "D1")
		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); e.ProcessOne(cl) }()
		}
		wg.Wait()
		snap, _, err := e.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if snap.FrozenCash != 0 || len(e.ClosedDates()) != 1 {
			t.Fatalf("frozen=%.2f closed=%v", snap.FrozenCash, e.ClosedDates())
		}
	})

	register("交易日边界", "跨日未显式收盘自动收掉上一交易日", func(t *testing.T) {
		e := newTestEngine(1_000_000)
		e.ProcessOne(submitEvent("d1", 1, "AAA", event.Buy, event.Limit, 100, 10, event.Day))
		// 直接投 D2 事件，D1 应被自动收盘
		e.ProcessOne(tradeEvent("t1", 2, "AAA", 11, 10, "D2"))
		if statusOf(e, "ord-d1") != StatusExpired {
			t.Fatalf("status=%s", statusOf(e, "ord-d1"))
		}
		if ds := e.GetDaySnapshot("D1"); ds == nil {
			t.Fatal("auto close snapshot missing")
		}
	})

	register("交易日边界", "收盘后提交委托被拒", func(t *testing.T) {
		e := newTestEngine(1_000_000)
		e.ProcessOne(closeEvent("c1", 1, "", 0, "D1"))
		mustReject(t, e.ProcessOne(submitEvent("x1", 2, "AAA", event.Buy, event.Limit, 10, 10, event.Day)),
			RejectSessionClosed)
	})
}
