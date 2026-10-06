package portfolio

import (
	"fmt"
	"math"
	"os"
	"sort"
	"testing"
)

// 与 engine 包一致的业务分组风格：打印各业务用例数与结果。

type pCase struct {
	group string
	name  string
	fn    func(t *testing.T)
}

var pRegistered []pCase

func pRegister(group, name string, fn func(t *testing.T)) {
	pRegistered = append(pRegistered, pCase{group: group, name: name, fn: fn})
}

func TestPortfolioGroups(t *testing.T) {
	groups := map[string][]pCase{}
	var order []string
	for _, c := range pRegistered {
		if _, ok := groups[c.group]; !ok {
			order = append(order, c.group)
		}
		groups[c.group] = append(groups[c.group], c)
	}
	sort.Strings(order)

	fmt.Println()
	fmt.Println("================ 业务分组测试结果（portfolio）================")
	var totalPass, totalFail int
	for _, g := range order {
		var pass, fail int
		for _, c := range groups[g] {
			c := c
			ok := t.Run(g+"/"+c.name, func(t *testing.T) { c.fn(t) })
			if ok {
				pass++
			} else {
				fail++
			}
		}
		totalPass += pass
		totalFail += fail
		result := "PASS"
		if fail > 0 {
			result = "FAIL"
		}
		fmt.Printf("[%-10s] 分组=%-20s 用例数=%-3d 通过=%-3d 失败=%-3d\n", result, g, pass+fail, pass, fail)
	}
	fmt.Printf("[%-10s] 合计用例=%d 通过=%d 失败=%d\n",
		map[bool]string{true: "PASS", false: "FAIL"}[totalFail == 0], totalPass+totalFail, totalPass, totalFail)
	fmt.Println("==============================================================")
	fmt.Println()
	if totalFail > 0 {
		os.Exit(1)
	}
}

func init() {
	pRegister("账户核算", "买入成交后现金持仓与权益拆解一致", func(t *testing.T) {
		p := New(100_000, 0)
		if err := p.ReserveBuy("AAA", 100, 10); err != nil {
			t.Fatal(err)
		}
		if err := p.FillBuy("AAA", 100, 9.5, 10); err != nil {
			t.Fatal(err)
		}
		snap, err := p.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if math.Abs(snap.Cash-(100_000-950)) > 1e-6 {
			t.Fatalf("cash=%.4f", snap.Cash)
		}
		if snap.Positions["AAA"].Qty != 100 || snap.Positions["AAA"].AvgCost != 9.5 {
			t.Fatalf("position=%+v", snap.Positions["AAA"])
		}
	})

	pRegister("账户核算", "卖出成交结转已实现盈亏", func(t *testing.T) {
		p := New(100_000, 0)
		_ = p.ReserveBuy("AAA", 100, 10)
		_ = p.FillBuy("AAA", 100, 10, 10)
		if err := p.ReserveSell("AAA", 100); err != nil {
			t.Fatal(err)
		}
		if err := p.FillSell("AAA", 100, 11); err != nil {
			t.Fatal(err)
		}
		snap, err := p.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if math.Abs(snap.RealizedPnL-100) > 1e-6 {
			t.Fatalf("realized=%.4f", snap.RealizedPnL)
		}
	})

	pRegister("账户核算", "撤单释放买入冻结后恒等式成立", func(t *testing.T) {
		p := New(100_000, 0)
		if err := p.ReserveBuy("AAA", 100, 10); err != nil {
			t.Fatal(err)
		}
		p.ReleaseBuy(10, 100)
		snap, err := p.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if snap.Cash != 100_000 || snap.FrozenCash != 0 {
			t.Fatalf("cash=%.2f frozen=%.2f", snap.Cash, snap.FrozenCash)
		}
	})

	pRegister("账户核算", "盯市价格变化只动未实现盈亏", func(t *testing.T) {
		p := New(100_000, 0)
		_ = p.ReserveBuy("AAA", 100, 10)
		_ = p.FillBuy("AAA", 100, 10, 10)
		p.SetMark("AAA", 12)
		snap, err := p.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if math.Abs(snap.UnrealizedPnL-200) > 1e-6 || math.Abs(snap.Equity-(100_000+200)) > 1e-6 {
			t.Fatalf("unrealized=%.2f equity=%.2f", snap.UnrealizedPnL, snap.Equity)
		}
	})

	pRegister("账户核算", "拆股调整持仓成本且对账通过", func(t *testing.T) {
		p := New(100_000, 0)
		_ = p.ReserveBuy("AAA", 100, 10)
		_ = p.FillBuy("AAA", 100, 10, 10)
		if err := p.ApplySplit("AAA", 2, 5); err != nil {
			t.Fatal(err)
		}
		snap, err := p.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		pos := snap.Positions["AAA"]
		if pos.Qty != 200 || math.Abs(pos.AvgCost-5) > 1e-9 {
			t.Fatalf("qty=%d cost=%.4f", pos.Qty, pos.AvgCost)
		}
	})

	pRegister("账户核算", "分红派现计入公司行为项", func(t *testing.T) {
		p := New(100_000, 0)
		_ = p.ReserveBuy("AAA", 100, 10)
		_ = p.FillBuy("AAA", 100, 10, 10)
		if err := p.ApplyDividend("AAA", 0.5); err != nil {
			t.Fatal(err)
		}
		snap, err := p.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if math.Abs(snap.CorporateActions-50) > 1e-6 || math.Abs(snap.Cash-(100_000-1000+50)) > 1e-6 {
			t.Fatalf("corp=%.2f cash=%.2f", snap.CorporateActions, snap.Cash)
		}
	})

	pRegister("账户核算", "卖出持仓不足拒绝冻结", func(t *testing.T) {
		p := New(100_000, 0)
		if err := p.ReserveSell("AAA", 1); err == nil {
			t.Fatal("expected insufficient position error")
		}
	})
}
