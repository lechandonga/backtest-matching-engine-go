package backtest

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/lechandonga/backtest-matching-engine-go/account"
	"github.com/lechandonga/backtest-matching-engine-go/engine"
	"github.com/lechandonga/backtest-matching-engine-go/event"
)

func buildScenario() []*event.Event {
	var evs []*event.Event
	seq := int64(1)
	syms := []string{"AAA", "BBB", "CCC"}
	add := func(ev *event.Event) {
		ev.Seq = seq
		seq++
		evs = append(evs, ev)
	}

	// 交错多标的行情与订单，制造真实的乱序到达可能。
	for round := 0; round < 6; round++ {
		for i, sym := range syms {
			px := 10.0 + float64(round)
			q := 50.0 + float64(round*10)
			add(&event.Event{EventID: fmt.Sprintf("m-%s-%d", sym, round), Kind: event.KindMarket,
				Symbol: sym, LastPrice: px,
				Liquidity: []event.Liquidity{{Price: px, Qty: q}}})
			if round%2 == 0 {
				side := event.Buy
				if (round+i)%3 == 0 {
					side = event.Sell
				}
				add(&event.Event{EventID: fmt.Sprintf("s-%s-%d", sym, round), Kind: event.KindSubmit,
					Order: event.Order{
						ID:     fmt.Sprintf("o-%s-%d", sym, round),
						Symbol: sym, Side: side, Type: event.Market, Qty: 20,
					}})
			}
		}
	}
	// 公司行为
	add(&event.Event{EventID: "split-aaa", Kind: event.KindSplit,
		CorporateSymbol: "AAA", Ratio: 2})
	add(&event.Event{EventID: "div-bbb", Kind: event.KindDividend,
		CorporateSymbol: "BBB", PerShareCash: 1})
	return evs
}

type fingerprint struct {
	cash, realized, fees, corp, equity float64
	filled, canceled, rejected         int
}

func runOnce(t *testing.T, evs []*event.Event, shuffle bool, workers int) fingerprint {
	t.Helper()
	acct := account.New(1_000_000)
	eng := engine.New(acct, engine.WithFeeModel(account.BpsFee(5)))
	r := New(eng)

	input := make([]*event.Event, len(evs))
	copy(input, evs)
	if shuffle {
		rand.New(rand.NewSource(99)).Shuffle(len(input), func(i, j int) {
			input[i], input[j] = input[j], input[i]
		})
	}

	outs := r.Run(input, workers)
	for _, o := range outs {
		if o.Err != nil {
			t.Fatalf("事件处理错误: %v", o.Err)
		}
	}

	var fp fingerprint
	s, err := acct.Reconcile(map[string]float64{"AAA": 7.5, "BBB": 15, "CCC": 15})
	if err != nil {
		t.Fatalf("最终对账失败: %v", err)
	}
	fp.cash, fp.realized, fp.fees, fp.corp, fp.equity =
		s.Cash, s.RealizedPnL, s.Fees, s.CorporateAdjustments, s.Equity

	seenID := map[string]bool{}
	for _, ev := range evs {
		if ev.Kind != event.KindSubmit {
			continue
		}
		if seenID[ev.EventID] {
			continue
		}
		seenID[ev.EventID] = true
		o, ok := eng.Order(ev.Order.ID)
		if !ok {
			continue
		}
		switch o.Status {
		case engine.StatusFilled:
			fp.filled++
		case engine.StatusCanceled, engine.StatusPartiallyCanceled:
			fp.canceled++
		case engine.StatusRejected:
			fp.rejected++
		}
	}
	return fp
}

// TestConcurrentShuffleDeterminism 并发乱序投递与顺序投递结果一致。
func TestConcurrentShuffleDeterminism(t *testing.T) {
	t.Log("业务[并发乱序] 用例1: 顺序单线程 vs 多 worker 并发+乱序输入, 最终账户/订单指纹一致")
	evs := buildScenario()

	baseline := runOnce(t, evs, false, 1)
	t.Logf("  基线指纹: cash=%.2f realized=%.2f fees=%.2f corp=%.2f equity=%.2f",
		baseline.cash, baseline.realized, baseline.fees, baseline.corp, baseline.equity)

	for trial := 0; trial < 5; trial++ {
		fp := runOnce(t, evs, true, 4)
		if fp != baseline {
			t.Fatalf("第%d次并发乱序结果不一致: %+v != %+v", trial, fp, baseline)
		}
	}
	t.Logf("  5 次并发乱序回放指纹完全一致(成交全成%d/取消%d/拒绝%d)",
		baseline.filled, baseline.canceled, baseline.rejected)
}

// TestRepeatedReplay 同一事件序列在独立引擎上反复回放结果一致。
func TestRepeatedReplay(t *testing.T) {
	t.Log("业务[重复回放] 用例2: 同一事件序列在全新引擎上回放 3 次, 结果逐位一致")
	evs := buildScenario()
	first := runOnce(t, evs, false, 4)
	for i := 0; i < 3; i++ {
		if got := runOnce(t, evs, false, 4); got != first {
			t.Fatalf("第%d次回放结果不一致: %+v != %+v", i, got, first)
		}
	}
	t.Log("  3 次独立回放指纹一致")
}

// TestDuplicateDeliveryInBatch 同一批事件重复投递不产生重复扣款/成交。
func TestDuplicateDeliveryInBatch(t *testing.T) {
	t.Log("业务[重复投递-并发] 用例3: 整批事件含重复 EventID, 重复部分只生效一次")
	evs := buildScenario()

	dupe := make([]*event.Event, 0, len(evs)*2)
	dupe = append(dupe, evs...)
	for _, ev := range evs {
		cp := *ev
		dupe = append(dupe, &cp)
	}

	baseline := runOnce(t, evs, true, 4)
	fp := runOnce(t, dupe, true, 4)
	if fp != baseline {
		t.Fatalf("重复投递改变结果: %+v != %+v", fp, baseline)
	}
	t.Logf("  事件数翻倍(含重复)后现金/盈亏/订单指纹不变, equity=%.2f", fp.equity)

	// 验证重复结果被标记
	acct := account.New(1_000_000)
	eng := engine.New(acct)
	r := New(eng)
	outs := r.Run([]*event.Event{evs[0], evs[0]}, 2)
	if !outs[1].Result.Duplicate {
		t.Fatal("第二条重复事件应标记 Duplicate")
	}
	t.Log("  重复事件 Result.Duplicate=true")
}
