package account

import (
	"errors"
	"math"
	"testing"
)

func approx(a, b float64) bool {
	return math.Abs(a-b) <= 1e-9*math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
}

// TestPnLLifecycle 覆盖：买入建仓 -> 未实现盈亏 -> 部分卖出实现盈亏 -> 全平 -> 费用核对。
func TestPnLLifecycle(t *testing.T) {
	t.Log("业务[盈亏对账] 用例1: 建仓/部分实现/全平/费用，权益恒等式逐笔成立")
	fee := BpsFee(10) // 0.1%
	acct := New(100000)

	must(t, acct.PlaceBuy("o1", "AAA", 100, 10, fee))
	must(t, acct.BuyFilled(Fill{FillID: "f1", Symbol: "AAA", Side: "buy", Qty: 100, Price: 10, Fee: fee.Fee(1000, 100)}))
	must(t, acct.AdjustBuyReservation("o1", "AAA", 0, 10, fee))

	s, err := acct.Reconcile(map[string]float64{"AAA": 12})
	must(t, err)
	if !approx(s.UnrealizedPnL, 200) {
		t.Fatalf("未实现盈亏 = %v, 期望 200", s.UnrealizedPnL)
	}
	if !approx(s.Equity, 100000+200-1) {
		t.Fatalf("浮盈后权益 = %v, 期望 100199", s.Equity)
	}
	t.Logf("  100股@10 建仓, 标记12: 未实现盈亏=%.2f 费用=%.2f 权益=%.2f", s.UnrealizedPnL, s.Fees, s.Equity)

	must(t, acct.PlaceSell("o2", "AAA", 40))
	must(t, acct.SellFilled(Fill{FillID: "f2", Symbol: "AAA", Side: "sell", Qty: 40, Price: 12, Fee: fee.Fee(480, 40)}))
	must(t, acct.AdjustSellReservation("o2", "AAA", 0))

	s, err = acct.Reconcile(map[string]float64{"AAA": 12})
	must(t, err)
	if !approx(s.RealizedPnL, 80) || !approx(s.UnrealizedPnL, 120) {
		t.Fatalf("部分卖出后 已实现=%v 未实现=%v", s.RealizedPnL, s.UnrealizedPnL)
	}
	if !approx(s.Equity, 100000+80+120-1-0.48) {
		t.Fatalf("部分卖出后权益 = %v", s.Equity)
	}
	t.Logf("  卖出40股@12: 已实现=%.2f 未实现=%.2f 费用累计=%.2f 权益=%.2f",
		s.RealizedPnL, s.UnrealizedPnL, s.Fees, s.Equity)

	must(t, acct.PlaceSell("o3", "AAA", 60))
	must(t, acct.SellFilled(Fill{FillID: "f3", Symbol: "AAA", Side: "sell", Qty: 60, Price: 11, Fee: fee.Fee(660, 60)}))
	must(t, acct.AdjustSellReservation("o3", "AAA", 0))

	s, err = acct.Reconcile(map[string]float64{"AAA": 11})
	must(t, err)
	if !approx(s.RealizedPnL, 140) || !approx(s.UnrealizedPnL, 0) {
		t.Fatalf("全平后 已实现=%v 未实现=%v", s.RealizedPnL, s.UnrealizedPnL)
	}
	if !approx(s.Equity, 100000+140-1-0.48-0.66) {
		t.Fatalf("全平后权益 = %v", s.Equity)
	}
	t.Logf("  清仓60股@11: 已实现=%.2f 费用累计=%.2f 权益=%.2f, 对账通过", s.RealizedPnL, s.Fees, s.Equity)
}

// TestIdempotentFills 覆盖：同一 FillID 重复投递不重复扣款/计盈亏；冲突内容报错。
func TestIdempotentFills(t *testing.T) {
	t.Log("业务[重复投递-核算] 用例2: 重复成交不重复扣款，内容冲突显式拒绝")
	acct := New(10000)
	must(t, acct.PlaceBuy("o1", "AAA", 10, 100, ZeroFee))
	f := Fill{FillID: "dup", Symbol: "AAA", Side: "buy", Qty: 10, Price: 100}
	must(t, acct.BuyFilled(f))
	cashAfter := acct.Snapshot(nil).Cash
	must(t, acct.BuyFilled(f))
	if got := acct.Snapshot(nil).Cash; !approx(got, cashAfter) {
		t.Fatalf("重复成交导致重复扣款: %v -> %v", cashAfter, got)
	}
	t.Logf("  同一 FillID 投递两次, 现金保持 %.2f", cashAfter)

	bad := f
	bad.Price = 101
	if err := acct.BuyFilled(bad); !errors.Is(err, ErrDuplicateFill) {
		t.Fatalf("冲突成交应返回 ErrDuplicateFill, got %v", err)
	}
	t.Log("  同 FillID 不同价格 -> ErrDuplicateFill, 测试通过")
}

// TestRejections 覆盖：资金不足/持仓不足/重复预留的可区分拒绝。
func TestRejections(t *testing.T) {
	t.Log("业务[核算拒绝] 用例3: 资金不足、持仓不足、重复预留分别可区分")
	acct := New(1000)
	if err := acct.PlaceBuy("b1", "AAA", 100, 20, ZeroFee); !errors.Is(err, ErrInsufficientCash) {
		t.Fatalf("期望 ErrInsufficientCash, got %v", err)
	}
	t.Logf("  资金不足买单被拒: %v", errOf(acct.PlaceBuy("b2", "AAA", 100, 20, ZeroFee)))

	if err := acct.PlaceSell("s1", "AAA", 1); !errors.Is(err, ErrInsufficientPosition) {
		t.Fatalf("期望 ErrInsufficientPosition, got %v", err)
	}
	t.Log("  零持仓卖单被拒(insufficient position)")

	must(t, acct.PlaceBuy("b3", "AAA", 1, 100, ZeroFee))
	if err := acct.PlaceBuy("b3", "AAA", 1, 100, ZeroFee); !errors.Is(err, ErrDuplicateReservation) {
		t.Fatalf("期望 ErrDuplicateReservation, got %v", err)
	}
	t.Log("  重复预留同一订单被拒(duplicate reservation)")
}

// TestCorporateActions 覆盖拆股（数量/成本口径）与分红（计入公司行为调整）。
func TestCorporateActions(t *testing.T) {
	t.Log("业务[公司行为] 用例4: 一拆二不改变权益；现金分红计入 CorporateAdjustments")
	acct := New(10000)
	must(t, acct.PlaceBuy("o1", "AAA", 100, 10, ZeroFee))
	must(t, acct.BuyFilled(Fill{FillID: "f1", Symbol: "AAA", Side: "buy", Qty: 100, Price: 10}))
	must(t, acct.AdjustBuyReservation("o1", "AAA", 0, 10, ZeroFee))

	acct.Split("evt-split", "AAA", 2)
	s := acct.Snapshot(map[string]float64{"AAA": 5})
	if !approx(s.Positions[0].Qty, 200) || !approx(s.Positions[0].AvgCost, 5) {
		t.Fatalf("拆股后数量/成本异常: %+v", s.Positions[0])
	}
	if !approx(s.Equity, 10000) || !approx(s.UnrealizedPnL, 0) {
		t.Fatalf("拆股不应改变权益: equity=%v upl=%v", s.Equity, s.UnrealizedPnL)
	}
	t.Log("  100股@10 一拆二 -> 200股@5, 标记5, 权益不变")

	got := acct.Dividend("evt-div", "AAA", 0.5)
	if !approx(got, 100) {
		t.Fatalf("分红总额 = %v, 期望 100", got)
	}
	s, err := acct.Reconcile(map[string]float64{"AAA": 5})
	must(t, err)
	if !approx(s.CorporateAdjustments, 100) || !approx(s.Equity, 10100) {
		t.Fatalf("分红后 corporateAdj=%v equity=%v", s.CorporateAdjustments, s.Equity)
	}
	t.Logf("  每股派0.5 -> 现金+%.2f, 计入公司行为调整, 权益=%.2f, 对账通过", got, s.Equity)
}

// TestReconcileRejectsMismatch 覆盖对账差额显式报错而不是静默吸收。
func TestReconcileRejectsMismatch(t *testing.T) {
	t.Log("业务[对账失败] 用例5: 人为制造账实差额时 Reconcile 必须返回 ReconcileError")
	acct := New(1000)
	s, err := acct.Reconcile(nil)
	must(t, err)
	if !approx(s.Equity, 1000) {
		t.Fatalf("初始权益异常: %v", s.Equity)
	}

	// 直接破坏内部不变量（模拟账本错误）：现金凭空增加但不经过任何业务科目。
	acct.cash += 500
	if _, err := acct.Reconcile(nil); err == nil {
		t.Fatal("账实不一致时必须返回错误")
	} else {
		t.Logf("  检测到差额并显式报错: %v", err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func errOf(err error) error { return err }
