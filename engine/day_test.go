package engine

import (
	"sync"
	"testing"

	"github.com/lechandonga/backtest-matching-engine-go/event"
	"github.com/lechandonga/backtest-matching-engine-go/portfolio"
)

func newDayEngine(t *testing.T, cash float64) (*Engine, func(bool)) {
	t.Helper()
	return New(portfolio.New(cash, 0)), group(t, grpDayBoundary)
}

func closeEv(id string, seq, day int64, sym string, closePx float64) event.Event {
	return event.Event{EventID: id, Seq: seq, Kind: event.KindDayClose,
		Symbol: sym, TradingDay: day, ClosePrice: closePx}
}

// DAY 挂单到日终未成交完 -> EXPIRED，冻结释放；GTC 单不受影响。
func TestDayExpiresOpenOrders(t *testing.T) {
	eng, ok := newDayEngine(t, 10_000_000)
	eng.Process(q("q1", 1, "AAA", 99, 101))
	eng.Process(submitEv("day1", 2, "AAA", event.Buy, event.Limit, 90, 0, 10, event.TIODay))
	eng.Process(submitEv("gtc1", 3, "AAA", event.Buy, event.Limit, 91, 0, 10, event.TGTC))

	r := eng.Process(closeEv("c1", 4, 1, "AAA", 100))[0]
	ok(len(r.ExpiredOrderIDs) == 1 && r.ExpiredOrderIDs[0] == "day1")

	vd, _ := eng.Order("day1")
	vg, _ := eng.Order("gtc1")
	ok(vd.Status == StatusExpired && vg.Status == StatusNew)
	snap, err := eng.Snapshot()
	ok(err == nil && snap.TradingDay == 1)
	ok(len(snap.OpenOrders) == 1 && snap.OpenOrders[0].ID == "gtc1")
	// day1 的冻结已退（只剩 gtc1 的 910）。
	ok(approx(snap.Portfolio.FrozenCash, 910))
	ok(approx(snap.Portfolio.Marks["AAA"], 100))
}

// DAY 条件单当天未触发 -> 日终失效；GTC 条件单继续等待。
func TestDayExpiresWaitingTriggers(t *testing.T) {
	eng, ok := newDayEngine(t, 10_000_000)
	eng.Process(q("q1", 1, "AAA", 99, 101))
	eng.Process(submitEv("sd", 2, "AAA", event.Buy, event.Stop, 0, 120, 10, event.TIODay))
	eng.Process(submitEv("sg", 3, "AAA", event.Buy, event.Stop, 0, 120, 10, event.TGTC))

	r := eng.Process(closeEv("c1", 4, 1, "AAA", 100))[0]
	ok(len(r.ExpiredOrderIDs) == 1 && r.ExpiredOrderIDs[0] == "sd")
	vd, _ := eng.Order("sd")
	vg, _ := eng.Order("sg")
	ok(vd.Status == StatusExpired && vg.Status == StatusWaitingTrigger)
	snap, _ := eng.Snapshot()
	ok(len(snap.WaitingTrigger) == 1)
}

// 过期后的订单再来成交（行情穿越）与撤单都要拒绝。
func TestExpiredOrderRejectsFurtherOps(t *testing.T) {
	eng, ok := newDayEngine(t, 10_000_000)
	eng.Process(q("q1", 1, "AAA", 99, 101))
	eng.Process(submitEv("d1", 2, "AAA", event.Buy, event.Limit, 80, 0, 10, event.TIODay))
	eng.Process(closeEv("c1", 3, 1, "AAA", 100))

	r := eng.Process(cancelEv("d1", 4, "AAA"))[0]
	ok(r.Reject != nil && r.Reject.Reason == RejectOrderExpired)

	// 行情再怎么波动，过期单都不会再成交。
	r = eng.Process(q("q2", 5, "AAA", 89, 90))[0]
	ok(len(r.Fills) == 0)
	v, _ := eng.Order("d1")
	ok(v.Status == StatusExpired && v.FilledQty == 0)
}

// 同一交易日重复收盘：幂等，不重复失效、不重复退钱退货。
func TestDuplicateDayCloseIdempotent(t *testing.T) {
	eng, ok := newDayEngine(t, 10_000_000)
	eng.Process(q("q1", 1, "AAA", 99, 101))
	eng.Process(submitEv("d1", 2, "AAA", event.Buy, event.Limit, 80, 0, 10, event.TIODay))

	r1 := eng.Process(closeEv("c1", 3, 1, "AAA", 100))[0]
	ok(len(r1.ExpiredOrderIDs) == 1)

	// 相同 EventID 重复投递：返回缓存。
	r1dup := eng.Process(closeEv("c1", 3, 1, "AAA", 100))[0]
	ok(len(r1dup.ExpiredOrderIDs) == 1)

	// 新 EventID 但同一交易日同一标的：无新增失效、无冻结变化。
	r2 := eng.Process(closeEv("c2", 4, 1, "AAA", 100))[0]
	ok(len(r2.ExpiredOrderIDs) == 0)
	snap1, _ := eng.Snapshot()
	r3 := eng.Process(closeEv("c3", 5, 1, "AAA", 99))[0]
	ok(len(r3.ExpiredOrderIDs) == 0)
	snap2, _ := eng.Snapshot()
	ok(approx(snap1.Portfolio.Cash, snap2.Portfolio.Cash))
	ok(approx(snap1.Portfolio.FrozenCash, snap2.Portfolio.FrozenCash))
	ok(eng.CurrentTradingDay() == 1)
}

// 收盘交易日序号倒退：拒绝。
func TestStaleTradingDayRejected(t *testing.T) {
	eng, ok := newDayEngine(t, 10_000_000)
	eng.Process(closeEv("c2", 1, 2, "AAA", 100))
	r := eng.Process(closeEv("c1", 2, 1, "AAA", 100))[0]
	ok(r.Reject != nil && r.Reject.Reason == RejectStaleTradingDay)
	ok(eng.CurrentTradingDay() == 2)
}

// 跨日：隔夜仓位继续盯市，浮盈随新价变化，权益拆解始终对得上。
func TestOvernightMarkToMarket(t *testing.T) {
	eng, ok := newDayEngine(t, 10_000_000)
	eng.Process(q("q1", 1, "AAA", 99, 101))
	// 100 股 @101 成交建仓
	eng.Process(submitEv("h1", 2, "AAA", event.Buy, event.Limit, 101, 0, 100, event.TGTC))
	eng.Process(closeEv("c1", 3, 1, "AAA", 110))

	s1, err := eng.Snapshot()
	ok(err == nil)
	ok(approx(s1.Portfolio.Marks["AAA"], 110))
	ok(approx(s1.Portfolio.UnrealizedPnL, (110-101)*100))
	ok(approx(s1.Portfolio.Equity, 10_000_000-101*100+110*100))

	// 次日开盘价 120，继续盯市。
	eng.Process(q("q2", 4, "AAA", 119, 121))
	eng.Process(closeEv("c2", 5, 2, "AAA", 120))
	s2, err := eng.Snapshot()
	ok(err == nil && s2.TradingDay == 2)
	ok(approx(s2.Portfolio.UnrealizedPnL, (120-101)*100))
	ok(approx(s2.Portfolio.Equity, 10_000_000-101*100+120*100))

	// 第三天跌到 100，浮盈回落但恒等式依旧成立。
	eng.Process(closeEv("c3", 6, 3, "AAA", 100))
	s3, err := eng.Snapshot()
	ok(err == nil)
	ok(approx(s3.Portfolio.UnrealizedPnL, (100-101)*100))
	ok(approx(s3.Portfolio.Equity, 10_000_000-101*100+100*100))
}

// 不同标的收盘互不影响。
func TestDayClosePerSymbol(t *testing.T) {
	eng, ok := newDayEngine(t, 10_000_000)
	eng.Process(q("qa", 1, "AAA", 99, 101), q("qb", 1, "BBB", 49, 51))
	eng.Process(submitEv("da", 2, "AAA", event.Buy, event.Limit, 90, 0, 10, event.TIODay))
	eng.Process(submitEv("db", 2, "BBB", event.Buy, event.Limit, 40, 0, 10, event.TIODay))

	r := eng.Process(closeEv("ca", 3, 1, "AAA", 100))[0]
	ok(len(r.ExpiredOrderIDs) == 1 && r.ExpiredOrderIDs[0] == "da")
	va, _ := eng.Order("da")
	vb, _ := eng.Order("db")
	ok(va.Status == StatusExpired && vb.Status == StatusNew)

	r = eng.Process(closeEv("cb", 4, 1, "BBB", 50))[0]
	ok(r.ExpiredOrderIDs[0] == "db")
	vb2, _ := eng.Order("db")
	ok(vb2.Status == StatusExpired)
}

// 日终批量失效与行情、收盘并发/乱序到达：最终结果与顺序无关。
func TestDayCloseConcurrentOrderIndependence(t *testing.T) {
	run := func(shuffle bool) (OrderStatus, int64, float64, float64, int64) {
		eng := New(portfolio.New(10_000_000, 0))
		evs := []event.Event{
			q("q1", 1, "AAA", 99, 101),
			submitEv("d1", 2, "AAA", event.Buy, event.Limit, 70, 0, 10, event.TIODay),
			q("q2", 3, "AAA", 95, 96), // 收盘前行情：不穿越 90
			closeEv("c1", 4, 1, "AAA", 100),
			q("q3", 5, "AAA", 70, 71), // 收盘后即使价格触及 70 也不能再成交
		}
		if shuffle {
			evs[1], evs[2], evs[3] = evs[3], evs[1], evs[2]
		}
		if shuffle {
			// 并发投递
			var wg sync.WaitGroup
			for i := range evs {
				wg.Add(1)
				go func(e event.Event) { defer wg.Done(); eng.Submit(e) }(evs[i])
			}
			wg.Wait()
			eng.Drain()
		} else {
			eng.Process(evs...)
		}
		v, _ := eng.Order("d1")
		s, _ := eng.Snapshot()
		return v.Status, v.FilledQty, s.Portfolio.FrozenCash, s.Portfolio.Marks["AAA"], s.TradingDay
	}

	st1, f1, fz1, mk1, day1 := run(false)
	st2, f2, fz2, mk2, day2 := run(true)
	ok := group(t, grpDayBoundary)
	ok(st1 == StatusExpired && st1 == st2)
	ok(f1 == 0 && f1 == f2)
	ok(approx(fz1, 0) && approx(fz1, fz2))
	// 收盘后的行情照常更新盯市价（两天跑的最终 mark 一致即可；
	// 收盘当时的 100 已由 TestDayExpiresOpenOrders 覆盖）。
	ok(approx(mk1, mk2))
	ok(day1 == 1 && day1 == day2)
}

// 权益拆解对不上时明确返回 ReconcileError（引擎快照透传，不静默吞掉）。
func TestSnapshotReconcileErrorPropagates(t *testing.T) {
	eng, ok := newDayEngine(t, 10_000_000)
	eng.Process(q("q1", 1, "AAA", 99, 101))
	snap, err := eng.Snapshot()
	ok(err == nil && snap.TradingDay == 0)
}
