package engine

import (
	"testing"

	"github.com/lechandonga/backtest-matching-engine-go/account"
	"github.com/lechandonga/backtest-matching-engine-go/event"
)

// TestSubmitValidation 非法价格/数量/方向以及重复订单 ID 的拒绝原因。
func TestSubmitValidation(t *testing.T) {
	t.Log("业务[委托校验] 用例4: 非法数量、非法限价、未知方向、重复订单ID各自可区分")
	e := newTestEngine(t, 100000, nil)

	r, _ := e.Process(&event.Event{Seq: 1, EventID: "bad1", Kind: event.KindSubmit, Order: event.Order{
		ID: "x", Symbol: "AAA", Side: event.Buy, Type: event.Limit, Qty: 0, Price: 10}})
	expectReject(t, r, RejectInvalidEvent)

	r, _ = e.Process(&event.Event{Seq: 2, EventID: "bad2", Kind: event.KindSubmit, Order: event.Order{
		ID: "x", Symbol: "AAA", Side: event.Buy, Type: event.Limit, Qty: 10, Price: -1}})
	expectReject(t, r, RejectInvalidPrice)

	r, _ = e.Process(&event.Event{Seq: 3, EventID: "bad3", Kind: event.KindSubmit, Order: event.Order{
		ID: "x", Symbol: "AAA", Side: "sideways", Type: event.Limit, Qty: 10, Price: 10}})
	expectReject(t, r, RejectInvalidEvent)
	t.Log("  非法数量/价格/方向 -> invalid_* 拒绝")

	mustProcess(t, e, mkSubmit(4, "dup", event.Order{
		ID: "dup", Symbol: "AAA", Side: event.Buy, Type: event.Limit, Qty: 10, Price: 10}))
	r = mustProcess(t, e, mkSubmit(5, "dup2", event.Order{
		ID: "dup", Symbol: "AAA", Side: event.Buy, Type: event.Limit, Qty: 10, Price: 10}))
	expectReject(t, r, RejectDuplicateOrderID)
	t.Log("  相同订单ID二次提交 -> duplicate_order_id")
}

// TestInsufficientFundsAndPosition 资金/持仓不足的委托必须拒单且不占用资源。
func TestInsufficientFundsAndPosition(t *testing.T) {
	t.Log("业务[资金持仓校验] 用例5: 超资金买、超持仓卖分别拒绝，拒绝后不占用资源")
	e := newTestEngine(t, 1000, nil)

	r := mustProcess(t, e, mkSubmit(1, "big", event.Order{
		ID: "big", Symbol: "AAA", Side: event.Buy, Type: event.Limit, Qty: 100, Price: 20}))
	expectReject(t, r, RejectInsufficientCash)
	o, _ := e.Order("big")
	if o.Status != StatusRejected {
		t.Fatalf("资金不足订单应进入 rejected, got %s", o.Status)
	}
	t.Log("  买100@20 但现金1000 -> insufficient_cash, 状态 rejected")

	r = mustProcess(t, e, mkSubmit(2, "short", event.Order{
		ID: "short", Symbol: "AAA", Side: event.Sell, Type: event.Limit, Qty: 1, Price: 20}))
	expectReject(t, r, RejectInsufficientPos)
	t.Log("  零持仓卖出 -> insufficient_position")

	s, err := e.Account().Reconcile(map[string]float64{"AAA": 20})
	if err != nil {
		t.Fatalf("拒单后对账失败: %v", err)
	}
	if !approx(s.Cash, 1000) || !approx(s.Equity, 1000) {
		t.Fatalf("拒单不应占用资金: cash=%v equity=%v", s.Cash, s.Equity)
	}
	t.Log("  拒单后资金与权益保持 1000，对账通过")
}

// TestDuplicateEventNoDoubleEffect 同一事件重复投递只生效一次。
func TestDuplicateEventNoDoubleEffect(t *testing.T) {
	t.Log("业务[重复投递-撮合] 用例6: 重复 submit/market 事件不重复成交/扣款")
	e := newTestEngine(t, 100000, nil)
	mustProcess(t, e, mkMarket(1, "m1", "AAA", 10, event.Liquidity{Price: 10, Qty: 50}))

	sub := mkSubmit(2, "o1", event.Order{
		ID: "o1", Symbol: "AAA", Side: event.Buy, Type: event.Market, Qty: 50})
	mustProcess(t, e, sub)
	cash1 := e.Account().Snapshot(nil).Cash

	r := mustProcess(t, e, sub)
	if !r.Duplicate {
		t.Fatal("重复事件应标记 Duplicate")
	}
	cash2 := e.Account().Snapshot(nil).Cash
	if !approx(cash1, cash2) {
		t.Fatalf("重复 submit 导致重复扣款: %v -> %v", cash1, cash2)
	}

	mkt := mkMarket(1, "m1", "AAA", 10, event.Liquidity{Price: 10, Qty: 50})
	r = mustProcess(t, e, mkt)
	if !r.Duplicate {
		t.Fatal("重复行情事件应标记 Duplicate")
	}
	t.Logf("  重复 submit/market 事件均 Duplicate, 现金保持 %.2f", cash2)

	if _, err := e.Account().Reconcile(nil); err != nil {
		t.Fatalf("对账失败: %v", err)
	}
}

// TestFeesFlowThrough 费用入账 Fees 科目并保持权益恒等式。
func TestFeesFlowThrough(t *testing.T) {
	t.Log("业务[费用核算] 用例7: 买卖双方费用累计, 权益=初始+盈亏-费用")
	e := newTestEngine(t, 100000, account.BpsFee(10))

	mustProcess(t, e, mkMarket(1, "m1", "AAA", 10,
		event.Liquidity{Price: 10, Qty: 100}, event.Liquidity{Price: 10, Qty: 100}))
	mustProcess(t, e, mkSubmit(2, "buy1", event.Order{
		ID: "buy1", Symbol: "AAA", Side: event.Buy, Type: event.Market, Qty: 60}))
	mustProcess(t, e, mkSubmit(3, "sell1", event.Order{
		ID: "sell1", Symbol: "AAA", Side: event.Sell, Type: event.Market, Qty: 40}))

	s, err := e.Account().Reconcile(map[string]float64{"AAA": 10})
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	wantFee := 600*0.001 + 400*0.001
	if !approx(s.Fees, wantFee) {
		t.Fatalf("费用 = %v, 期望 %v", s.Fees, wantFee)
	}
	if !approx(s.Equity, 100000-wantFee) {
		t.Fatalf("费用后权益 = %v, 期望 %v", s.Equity, 100000-wantFee)
	}
	t.Logf("  买入1000+卖出400, 费用合计%.2f, 权益%.2f, 对账通过", s.Fees, s.Equity)
}

// TestCorporateActionThroughEngine 拆股后挂单价格/数量联动，分红进权益。
func TestCorporateActionThroughEngine(t *testing.T) {
	t.Log("业务[公司行为-引擎] 用例8: 拆股调整持仓与挂单口径；分红现金入账且可对账")
	e := newTestEngine(t, 100000, nil)

	mustProcess(t, e, mkMarket(1, "m1", "AAA", 10, event.Liquidity{Price: 10, Qty: 100}))
	mustProcess(t, e, mkSubmit(2, "buy1", event.Order{
		ID: "buy1", Symbol: "AAA", Side: event.Buy, Type: event.Market, Qty: 100}))
	mustProcess(t, e, mkSubmit(3, "bid2", event.Order{
		ID: "bid2", Symbol: "AAA", Side: event.Buy, Type: event.Limit, Qty: 100, Price: 8}))

	mustProcess(t, e, &event.Event{Seq: 4, EventID: "split1", Kind: event.KindSplit,
		CorporateSymbol: "AAA", Ratio: 2})

	o, _ := e.Order("bid2")
	if !approx(o.Qty, 200) || !approx(o.Price, 4) {
		t.Fatalf("拆股后挂单 qty=%v price=%v, 期望 200/4", o.Qty, o.Price)
	}
	last, _ := e.LastPrice("AAA")
	if !approx(last, 5) {
		t.Fatalf("拆股后最新价 = %v, 期望 5", last)
	}
	t.Log("  一拆二: 挂单 100@8 -> 200@4, 最新价 10 -> 5")

	mustProcess(t, e, &event.Event{Seq: 5, EventID: "div1", Kind: event.KindDividend,
		CorporateSymbol: "AAA", PerShareCash: 0.5})
	s, err := e.Account().Reconcile(map[string]float64{"AAA": 5})
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if !approx(s.CorporateAdjustments, 100) || !approx(s.Equity, 100100) {
		t.Fatalf("分红后 corporateAdj=%v equity=%v", s.CorporateAdjustments, s.Equity)
	}
	t.Logf("  拆股后200股每股派0.5 -> 派现100, 权益%.2f, 对账通过", s.Equity)
}
