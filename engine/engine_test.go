package engine

import (
	"math"
	"testing"

	"github.com/lechandonga/backtest-matching-engine-go/account"
	"github.com/lechandonga/backtest-matching-engine-go/event"
)

func approx(a, b float64) bool {
	return math.Abs(a-b) <= 1e-9*math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
}

func newTestEngine(t *testing.T, cash float64, fee account.FeeModel) *Engine {
	t.Helper()
	acct := account.New(cash)
	if fee == nil {
		fee = account.ZeroFee
	}
	return New(acct, WithFeeModel(fee))
}

func mkMarket(seq int64, id, sym string, last float64, liq ...event.Liquidity) *event.Event {
	return &event.Event{Seq: seq, EventID: id, Kind: event.KindMarket,
		Symbol: sym, LastPrice: last, Liquidity: liq}
}

func mkSubmit(seq int64, id string, o event.Order) *event.Event {
	return &event.Event{Seq: seq, EventID: "evt-" + id, Kind: event.KindSubmit, Order: o}
}

func mkCancel(seq int64, id, sym, oid string, qty float64) *event.Event {
	return &event.Event{Seq: seq, EventID: id, Kind: event.KindCancel,
		CancelSymbol: sym, CancelOrderID: oid, CancelQty: qty}
}

func mustProcess(t *testing.T, e *Engine, ev *event.Event) *Result {
	t.Helper()
	r, err := e.Process(ev)
	if err != nil {
		t.Fatalf("处理事件失败: %v", err)
	}
	return r
}

func expectReject(t *testing.T, r *Result, want RejectReason) {
	t.Helper()
	if r == nil || r.Reject == nil {
		t.Fatalf("期望拒绝原因 %s, 实际未拒绝", want)
	}
	if r.Reject.Reason != want {
		t.Fatalf("拒绝原因 = %s, 期望 %s (%s)", r.Reject.Reason, want, r.Reject.Message)
	}
}

// TestMarketOrderLifeCycle 市价单：部分成交后 IOC 剩余取消，无流动性时拒绝。
func TestMarketOrderLifeCycle(t *testing.T) {
	t.Log("业务[订单生命周期] 用例1: 市价单部分成交后取消；零成交拒绝；终态重复操作拒绝")
	e := newTestEngine(t, 100000, nil)

	mustProcess(t, e, mkMarket(1, "m1", "AAA", 10, event.Liquidity{Price: 10, Qty: 30}))
	mustProcess(t, e, mkSubmit(2, "mo1", event.Order{
		ID: "mo1", Symbol: "AAA", Side: event.Buy, Type: event.Market, Qty: 100}))
	o, _ := e.Order("mo1")
	if o.Status != StatusPartiallyCanceled || !approx(o.FilledQty, 30) {
		t.Fatalf("市价单状态 = %s filled=%v", o.Status, o.FilledQty)
	}
	t.Logf("  市价买100, 仅30流动性 -> 成交30, 状态=%s", o.Status)

	r := mustProcess(t, e, mkCancel(3, "c1", "AAA", "mo1", 0))
	expectReject(t, r, RejectOrderTerminal)
	t.Log("  对部分取消的终态单再次撤单 -> order_in_terminal_state")

	r = mustProcess(t, e, mkSubmit(4, "mo2", event.Order{
		ID: "mo2", Symbol: "BBB", Side: event.Buy, Type: event.Market, Qty: 10}))
	expectReject(t, r, RejectNoMarketPrice)
	t.Log("  无行情标的的市价单 -> no_market_price")

	mustProcess(t, e, mkMarket(5, "m2", "CCC", 20))
	r = mustProcess(t, e, mkSubmit(6, "mo3", event.Order{
		ID: "mo3", Symbol: "CCC", Side: event.Buy, Type: event.Market, Qty: 10}))
	expectReject(t, r, RejectNoLiquidity)
	t.Log("  有价无成交流动性的市价单 -> no_liquidity, 状态=rejected")
}

// TestLimitOrderPartialFillAndCancel 限价单挂簿、行情触发部分成交、部分撤、超量撤拒绝。
func TestLimitOrderPartialFillAndCancel(t *testing.T) {
	t.Log("业务[订单生命周期] 用例2: 限价单挂簿->部分成交->部分撤->超量撤被拒->全撤终态")
	e := newTestEngine(t, 100000, nil)

	mustProcess(t, e, mkSubmit(1, "lo1", event.Order{
		ID: "lo1", Symbol: "AAA", Side: event.Buy, Type: event.Limit, Qty: 100, Price: 10}))
	mustProcess(t, e, mkMarket(2, "m1", "AAA", 11, event.Liquidity{Price: 9, Qty: 40}))
	o, _ := e.Order("lo1")
	if o.Status != StatusPartiallyFilled || !approx(o.FilledQty, 40) {
		t.Fatalf("期望部分成交40, got status=%s filled=%v", o.Status, o.FilledQty)
	}
	t.Logf("  限价买100@10, 卖报价9量40 -> 部分成交40, 状态=%s", o.Status)

	mustProcess(t, e, mkCancel(3, "c1", "AAA", "lo1", 30))
	o, _ = e.Order("lo1")
	if !approx(o.Remaining(), 30) || o.Status != StatusPartiallyFilled {
		t.Fatalf("部分撤后 remaining=%v status=%s", o.Remaining(), o.Status)
	}
	t.Logf("  部分撤30 -> 剩余30, 仍为 partially_filled")

	r := mustProcess(t, e, mkCancel(4, "c2", "AAA", "lo1", 31))
	expectReject(t, r, RejectCancelExceedsQty)
	t.Log("  撤31 > 剩余30 -> cancel_qty_exceeds_remaining")

	mustProcess(t, e, mkCancel(5, "c3", "AAA", "lo1", 0))
	o, _ = e.Order("lo1")
	if o.Status != StatusPartiallyCanceled {
		t.Fatalf("状态 = %s, 期望 partially_canceled", o.Status)
	}
	t.Log("  全撤剩余 -> partially_canceled(终态)")

	r = mustProcess(t, e, mkCancel(6, "c4", "AAA", "ghost", 0))
	expectReject(t, r, RejectUnknownOrder)
	t.Log("  撤销不存在的订单 -> unknown_order")
}

// TestLimitCrossing 限价挂单交叉，按价格/时间优先撮合。
func TestLimitCrossing(t *testing.T) {
	t.Log("业务[订单生命周期] 用例3: 买卖限价挂单交叉成交，状态正确且对账通过")
	e := newTestEngine(t, 100000, nil)

	mustProcess(t, e, mkMarket(1, "m1", "AAA", 10, event.Liquidity{Price: 10, Qty: 100}))
	mustProcess(t, e, mkSubmit(2, "stock", event.Order{
		ID: "stock", Symbol: "AAA", Side: event.Buy, Type: event.Market, Qty: 100}))

	mustProcess(t, e, mkSubmit(3, "ask1", event.Order{
		ID: "ask1", Symbol: "AAA", Side: event.Sell, Type: event.Limit, Qty: 60, Price: 11}))
	mustProcess(t, e, mkSubmit(4, "bid1", event.Order{
		ID: "bid1", Symbol: "AAA", Side: event.Buy, Type: event.Limit, Qty: 100, Price: 11}))

	ask, _ := e.Order("ask1")
	bid, _ := e.Order("bid1")
	if ask.Status != StatusFilled || !approx(ask.FilledQty, 60) {
		t.Fatalf("ask 应全部成交, got %s %v", ask.Status, ask.FilledQty)
	}
	if bid.Status != StatusPartiallyFilled || !approx(bid.FilledQty, 60) || !approx(bid.Remaining(), 40) {
		t.Fatalf("bid 应部分成交60, got %s %v", bid.Status, bid.FilledQty)
	}
	t.Log("  卖60@11 与 买100@11 交叉: 卖全成, 买成交60剩余40挂簿")

	if _, err := e.Account().Reconcile(map[string]float64{"AAA": 11}); err != nil {
		t.Fatalf("对账失败: %v", err)
	}
}
