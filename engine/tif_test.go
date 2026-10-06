package engine

import (
	"testing"

	"github.com/lechandonga/backtest-matching-engine-go/event"
	"github.com/lechandonga/backtest-matching-engine-go/portfolio"
)

func newTIFEngine(t *testing.T, cash float64) (*Engine, func(bool)) {
	t.Helper()
	return New(portfolio.New(cash, 0)), group(t, grpTIF)
}

// FOK：对手盘不足时整单一点不成交，无部分成交中间态，冻结全退。
func TestFOKNotFillableLeavesNothing(t *testing.T) {
	eng, ok := newTIFEngine(t, 100_000_000)
	eng.Process(q("q1", 1, "AAA", 99, 101))
	eng.Process(submitEv("stock", 2, "AAA", event.Buy, event.Limit, 101, 0, 100, event.TGTC))
	// 只有 10 股卖盘挂在 101；随后把盘口卖价抬到 110（报价不提供 101 的流动性）。
	eng.Process(submitEv("w1", 3, "AAA", event.Sell, event.Limit, 101, 0, 10, event.TGTC))
	eng.Process(q("q2", 4, "AAA", 99, 110))

	r := eng.Process(event.Event{EventID: "ev-fok", Seq: 5, Kind: event.KindOrderSubmit,
		Symbol: "AAA", OrderID: "fok1", Side: event.Buy, Type: event.Limit,
		Price: 101, Qty: 50, TIF: event.TFOK})[0]
	ok(r.Reject == nil && len(r.Fills) == 0)
	v, _ := eng.Order("fok1")
	ok(v.Status == StatusCanceled && v.FilledQty == 0)
	snap, err := eng.Snapshot()
	ok(err == nil && snap.Portfolio.FrozenCash == 0)
	w1, _ := eng.Order("w1")
	ok(w1.FilledQty == 0) // 卖墙一股未动
}

// FOK：足额时本次撮合全部成交。
func TestFOKFillsAtomic(t *testing.T) {
	eng, ok := newTIFEngine(t, 100_000_000)
	eng.Process(q("q1", 1, "AAA", 99, 101))
	eng.Process(submitEv("stock", 2, "AAA", event.Buy, event.Limit, 101, 0, 100, event.TGTC))
	eng.Process(submitEv("w1", 3, "AAA", event.Sell, event.Limit, 101, 0, 30, event.TGTC))
	eng.Process(submitEv("w2", 3, "AAA", event.Sell, event.Limit, 101, 0, 20, event.TGTC))

	r := eng.Process(event.Event{EventID: "ev-fok", Seq: 4, Kind: event.KindOrderSubmit,
		Symbol: "AAA", OrderID: "fok1", Side: event.Buy, Type: event.Limit,
		Price: 101, Qty: 50, TIF: event.TFOK})[0]
	ok(len(r.Fills) > 0)
	v, _ := eng.Order("fok1")
	ok(v.Status == StatusFilled && v.FilledQty == 50)
	snap, err := eng.Snapshot()
	ok(err == nil && snap.Portfolio.FrozenCash == 0)
}

// FOK 市价：不足额整单拒绝/取消，无任何成交。
func TestFOKMarketAtomic(t *testing.T) {
	eng, ok := newTIFEngine(t, 100_000_000)
	eng.Process(q("q1", 1, "AAA", 99, 101))
	eng.Process(submitEv("stock", 2, "AAA", event.Buy, event.Limit, 101, 0, 100, event.TGTC))
	eng.Process(submitEv("w1", 3, "AAA", event.Sell, event.Limit, 101, 0, 5, event.TGTC))
	eng.Process(q("q2", 4, "AAA", 99, 110))

	r := eng.Process(event.Event{EventID: "ev-fokm", Seq: 5, Kind: event.KindOrderSubmit,
		Symbol: "AAA", OrderID: "fokm", Side: event.Buy, Type: event.Market,
		Qty: 50, TIF: event.TFOK})[0]
	ok(len(r.Fills) == 0)
	v, _ := eng.Order("fokm")
	ok(v.Status == StatusCanceled && v.FilledQty == 0)
	snap, _ := eng.Snapshot()
	ok(snap.Portfolio.FrozenCash == 0)
}

// IOC：能吃多少吃多少，剩余立即撤销，冻结释放干净。
func TestIOCPartialThenCancel(t *testing.T) {
	eng, ok := newTIFEngine(t, 100_000_000)
	eng.Process(q("q1", 1, "AAA", 99, 101))
	eng.Process(submitEv("stock", 2, "AAA", event.Buy, event.Limit, 101, 0, 100, event.TGTC))
	eng.Process(submitEv("w1", 3, "AAA", event.Sell, event.Limit, 101, 0, 30, event.TGTC))
	eng.Process(q("q2", 4, "AAA", 99, 110))

	r := eng.Process(event.Event{EventID: "ev-ioc", Seq: 5, Kind: event.KindOrderSubmit,
		Symbol: "AAA", OrderID: "ioc1", Side: event.Buy, Type: event.Limit,
		Price: 101, Qty: 80, TIF: event.TIFO})[0]
	v, _ := eng.Order("ioc1")
	ok(v.Status == StatusCanceled && v.FilledQty == 30)
	ok(len(r.Fills) == 2)
	snap, err := eng.Snapshot()
	ok(err == nil && snap.Portfolio.FrozenCash == 0)
}

// IOC 限价不穿越：零成交，整单撤销。
func TestIOCNoCrossCanceled(t *testing.T) {
	eng, ok := newTIFEngine(t, 10_000_000)
	eng.Process(q("q1", 1, "AAA", 99, 101))
	r := eng.Process(event.Event{EventID: "ev-ioc", Seq: 2, Kind: event.KindOrderSubmit,
		Symbol: "AAA", OrderID: "ioc1", Side: event.Buy, Type: event.Limit,
		Price: 95, Qty: 10, TIF: event.TIFO})[0]
	ok(len(r.Fills) == 0)
	v, _ := eng.Order("ioc1")
	ok(v.Status == StatusCanceled)
	snap, _ := eng.Snapshot()
	ok(snap.Portfolio.FrozenCash == 0)
}

// GTC：未成交部分持续挂簿，等待后续行情。
func TestGTCRestsUntilFutureMarket(t *testing.T) {
	eng, ok := newTIFEngine(t, 10_000_000)
	eng.Process(q("q1", 1, "AAA", 99, 101))
	eng.Process(submitEv("g1", 2, "AAA", event.Buy, event.Limit, 95, 0, 10, event.TGTC))
	v, _ := eng.Order("g1")
	ok(v.Status == StatusNew)
	snap, _ := eng.Snapshot()
	ok(len(snap.OpenOrders) == 1)

	// 成交流跌到 95 -> 挂单成交
	r := eng.Process(tr("t1", 3, "AAA", 95, 10))[0]
	ok(len(r.Fills) == 1)
	v2, _ := eng.Order("g1")
	ok(v2.Status == StatusFilled && v2.FilledQty == 10)
}

// 非法 TIF 被 INVALID_TIF 拒单。
func TestInvalidTIF(t *testing.T) {
	eng, ok := newTIFEngine(t, 10_000_000)
	eng.Process(q("q1", 1, "AAA", 99, 101))
	r := eng.Process(event.Event{EventID: "ev-bad", Seq: 2, Kind: event.KindOrderSubmit,
		Symbol: "AAA", OrderID: "bad", Side: event.Buy, Type: event.Limit,
		Price: 100, Qty: 10, TIF: event.TIF(99)})[0]
	ok(r.Reject != nil && r.Reject.Reason == RejectInvalidTIF)
}

// FOK 与触发型委托组合：触发后必须原子全成否则整单取消（无任何成交、冻结释放）。
func TestTriggeredFOKAtomic(t *testing.T) {
	eng, ok := newTIFEngine(t, 100_000_000)
	eng.Process(q("q1", 1, "AAA", 99, 101))
	eng.Process(submitEv("stock", 2, "AAA", event.Buy, event.Limit, 101, 0, 100, event.TGTC))
	// 触发价附近只有 10 股卖盘
	eng.Process(submitEv("w1", 3, "AAA", event.Sell, event.Limit, 106, 0, 10, event.TGTC))
	eng.Process(event.Event{EventID: "ev-sfok", Seq: 4, Kind: event.KindOrderSubmit,
		Symbol: "AAA", OrderID: "sfok", Side: event.Buy, Type: event.Stop,
		StopPrice: 105, Qty: 50, TIF: event.TFOK})
	eng.Process(q("q2", 5, "AAA", 104, 105)) // 触发：卖价 105；FOK 只买得起 10 股 106
	r := eng.Process(q("q3", 6, "AAA", 104, 110))[0]
	ok(len(r.Fills) == 0)
	v, _ := eng.Order("sfok")
	ok(v.Status == StatusCanceled && v.FilledQty == 0 && v.TriggerPrice == 105)
	snap, err := eng.Snapshot()
	ok(err == nil && snap.Portfolio.FrozenCash == 0)
}
