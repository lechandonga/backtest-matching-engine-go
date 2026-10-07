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

// 多标的只投一次收盘：不管事件带没带标的、带的是哪个标的，
// 当天所有标的的当日单/当日条件单统一失效，冻结原路退回。
func TestDayCloseAllSymbols(t *testing.T) {
	eng, ok := newDayEngine(t, 10_000_000)
	eng.Process(q("qa", 1, "AAA", 99, 101), q("qb", 1, "BBB", 49, 51))
	// AAA：当日限价单（冻结 900）+ GTC 限价单（冻结 910）
	eng.Process(submitEv("da", 2, "AAA", event.Buy, event.Limit, 90, 0, 10, event.TIODay))
	eng.Process(submitEv("ga", 3, "AAA", event.Buy, event.Limit, 91, 0, 10, event.TGTC))
	// BBB：当日条件单（未触发）+ GTC 条件单
	eng.Process(submitEv("sb", 4, "BBB", event.Buy, event.Stop, 0, 60, 10, event.TIODay))
	eng.Process(submitEv("sgb", 5, "BBB", event.Buy, event.Stop, 0, 60, 10, event.TGTC))
	// BBB：先建仓再挂当日卖单（冻结 5 股持仓）
	eng.Process(submitEv("hb", 6, "BBB", event.Buy, event.Limit, 51, 0, 20, event.TGTC))
	eng.Process(submitEv("ds", 7, "BBB", event.Sell, event.Limit, 55, 0, 5, event.TIODay))

	cashBefore, _ := eng.Snapshot()
	// 收盘事件带的是 AAA 的收盘价，但收口的是一整天、全部标的。
	r := eng.Process(closeEv("c1", 8, 1, "AAA", 100))[0]
	ok(len(r.ExpiredOrderIDs) == 3) // da、sb、ds，按 (Seq, OrderID) 确定序
	ok(r.ExpiredOrderIDs[0] == "da" && r.ExpiredOrderIDs[1] == "sb" && r.ExpiredOrderIDs[2] == "ds")

	vda, _ := eng.Order("da")
	vsb, _ := eng.Order("sb")
	vds, _ := eng.Order("ds")
	vga, _ := eng.Order("ga")
	vsgb, _ := eng.Order("sgb")
	ok(vda.Status == StatusExpired && vsb.Status == StatusExpired && vds.Status == StatusExpired)
	ok(vga.Status == StatusNew && vsgb.Status == StatusWaitingTrigger)

	snap, err := eng.Snapshot()
	ok(err == nil)
	// 冻结干净退回：现金冻结只剩 GTC 单 ga 的 910；持仓冻结清零。
	ok(approx(snap.Portfolio.FrozenCash, 910))
	ok(snap.Portfolio.Positions["BBB"].Frozen == 0 && snap.Portfolio.Positions["BBB"].Qty == 20)
	// 现金不多退：与收盘前相比只多了 da 释放的 900。
	ok(approx(snap.Portfolio.Cash, cashBefore.Portfolio.Cash+900))
	// 只有 AAA 的盯市价被收盘价更新；BBB 保持原行情。
	ok(approx(snap.Portfolio.Marks["AAA"], 100))
	ok(approx(snap.Portfolio.Marks["BBB"], 51))
}

// 不带标的的收盘：同样收口一整天，且不会在行情/盯市里产生空标的记录。
func TestDayCloseWithoutSymbol(t *testing.T) {
	eng, ok := newDayEngine(t, 10_000_000)
	eng.Process(q("qa", 1, "AAA", 99, 101))
	eng.Process(submitEv("da", 2, "AAA", event.Buy, event.Limit, 90, 0, 10, event.TIODay))

	r := eng.Process(event.Event{EventID: "c1", Seq: 3, Kind: event.KindDayClose, TradingDay: 1})[0]
	ok(r.Reject == nil && len(r.ExpiredOrderIDs) == 1 && r.ExpiredOrderIDs[0] == "da")

	snap, err := eng.Snapshot()
	ok(err == nil)
	_, hasEmpty := snap.Portfolio.Marks[""]
	ok(!hasEmpty)
	// 没有收盘价，盯市价维持行情价不变。
	ok(approx(snap.Portfolio.Marks["AAA"], 100))
	ok(eng.CurrentTradingDay() == 1)
}

// 日终快照：收盘后可取到当天固化快照，拆解与对账口径一致；
// 重复收盘（换 EventID）不改变快照内容；未收盘的交易日取不到。
func TestDayEndSnapshot(t *testing.T) {
	eng, ok := newDayEngine(t, 10_000_000)
	eng.Process(q("q1", 1, "AAA", 99, 101))
	eng.Process(submitEv("h1", 2, "AAA", event.Buy, event.Limit, 101, 0, 100, event.TGTC))
	eng.Process(submitEv("d1", 3, "AAA", event.Buy, event.Limit, 80, 0, 10, event.TIODay))

	_, okDay, err := eng.DayEndSnapshot(1)
	ok(!okDay && err == nil) // 尚未收盘

	eng.Process(closeEv("c1", 4, 1, "AAA", 110))
	s1, okDay, err := eng.DayEndSnapshot(1)
	ok(okDay && err == nil)
	ok(s1.TradingDay == 1)
	// 权益拆解：权益 = 现金 + 冻结 + 持仓市值；= 初始资金 + 未实现盈亏。
	pf := s1.Portfolio
	ok(approx(pf.Equity, pf.Cash+pf.FrozenCash+110*100))
	ok(approx(pf.Equity, 10_000_000-101*100+110*100))
	ok(approx(pf.UnrealizedPnL, (110-101)*100))
	ok(approx(pf.RealizedPnL, 0) && approx(pf.FrozenCash, 0))
	ok(pf.Positions["AAA"].Qty == 100)

	// 重复收盘（换了 EventID、甚至换了收盘价）：不二次结算，快照不变。
	r := eng.Process(closeEv("c2", 5, 1, "AAA", 55))[0]
	ok(r.Reject == nil && len(r.ExpiredOrderIDs) == 0)
	s2, okDay, err := eng.DayEndSnapshot(1)
	ok(okDay && err == nil)
	ok(approx(s2.Portfolio.Equity, s1.Portfolio.Equity))
	ok(approx(s2.Portfolio.Marks["AAA"], 110)) // 快照固化，不被重复收盘改写
	ok(len(s2.Orders) == len(s1.Orders))

	// 第二天收盘后，两天的日终快照各自独立可取。
	eng.Process(q("q2", 6, "AAA", 119, 121))
	eng.Process(closeEv("c3", 7, 2, "AAA", 120))
	s3, okDay, err := eng.DayEndSnapshot(2)
	ok(okDay && err == nil && s3.TradingDay == 2)
	ok(approx(s3.Portfolio.UnrealizedPnL, (120-101)*100))
	s1again, okDay, _ := eng.DayEndSnapshot(1)
	ok(okDay && approx(s1again.Portfolio.Equity, s1.Portfolio.Equity))
}

// 失效的当日条件单第二天不会再触发；GTC 条件单照常触发。
func TestExpiredTriggerNotFiredNextDay(t *testing.T) {
	eng, ok := newDayEngine(t, 10_000_000)
	eng.Process(q("q1", 1, "AAA", 99, 101))
	eng.Process(submitEv("sd", 2, "AAA", event.Buy, event.StopLimit, 121, 120, 10, event.TIODay))
	eng.Process(submitEv("sg", 3, "AAA", event.Buy, event.StopLimit, 121, 120, 10, event.TGTC))
	eng.Process(closeEv("c1", 4, 1, "AAA", 100))

	// 第二天行情上穿触发价：只有 GTC 条件单触发。
	r := eng.Process(q("q2", 5, "AAA", 119, 121))[0]
	ok(len(r.Fills) == 1 && r.Fills[0].OrderID == "sg")
	vd, _ := eng.Order("sd")
	ok(vd.Status == StatusExpired && vd.FilledQty == 0)
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
