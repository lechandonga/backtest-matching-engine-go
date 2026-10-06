package engine

import (
	"fmt"
	"runtime"
	"sync"
	"testing"

	"github.com/lechandonga/backtest-matching-engine-go/event"
)

// buildReplay 生成跨多个交易日、数万条事件的确定性回放序列：
// 限价/市价/IOC/FOK/止损/跟踪止损混合 + 行情 + 每日收盘。
func buildReplay(days, perDay int) []event.Event {
	var evs []event.Event
	seq := int64(0)
	symbols := []string{"AAA", "BBB", "CCC"}
	for d := 0; d < days; d++ {
		date := fmt.Sprintf("2026-10-%02d", d+1)
		price := 10.0
		for i := 0; i < perDay; i++ {
			seq++
			sym := symbols[i%len(symbols)]
			if i%3 == 0 {
				price += 0.05
			} else {
				price -= 0.02
			}
			if price < 5 {
				price = 5
			}
			evs = append(evs, tradeEvent(fmt.Sprintf("%d-tr-%d", d, i), seq, sym, price, 50, date))
			if i%5 == 0 {
				seq++
				kind := i % 4
				var ev event.Event
				id := fmt.Sprintf("%d-ord-%d", d, i)
				switch kind {
				case 0:
					ev = submitEvent(id, seq, sym, event.Buy, event.Limit, 10, price+0.5, event.GTC)
				case 1:
					ev = submitEvent(id, seq, sym, event.Buy, event.Market, 10, 0, event.IOC)
				case 2:
					ev = submitEvent(id, seq, sym, event.Buy, event.Limit, 10, price, event.FOK)
				default:
					ev = stopSubmit(id, seq, sym, event.Buy, event.StopLimit, 10, price*1.2, price*1.3, event.GTC)
				}
				ev.TradeDate = date
				evs = append(evs, ev)
			}
		}
		seq++
		evs = append(evs, closeEvent(fmt.Sprintf("close-%d", d), seq, "", 0, date))
	}
	return evs
}

func runReplay(evs []event.Event, parallel bool) (float64, int, []string) {
	e := newTestEngine(1_000_000_000)
	if !parallel {
		e.Process(evs)
	} else {
		// 多个投递方各自把“同一批完整事件”并发、乱序投递：
		// 引擎锁保证串行化，(Seq,EventID) 排序保证与投递顺序无关，EventID 幂等保证重复批次不二次生效。
		chunks := 6
		var wg sync.WaitGroup
		for c := 0; c < chunks; c++ {
			wg.Add(1)
			part := append([]event.Event(nil), evs...)
			if c%2 == 1 {
				for i, j := 0, len(part)-1; i < j; i, j = i+1, j-1 {
					part[i], part[j] = part[j], part[i]
				}
			}
			go func(p []event.Event) { defer wg.Done(); e.Process(p) }(part)
		}
		wg.Wait()
	}
	snap, _, err := e.Snapshot()
	if err != nil {
		panic(err)
	}
	return snap.Equity, len(e.ClosedDates()), e.ClosedDates()
}

func init() {
	register("回放压测", "数万事件跨多日回放稳定且对账通过", func(t *testing.T) {
		evs := buildReplay(5, 8000) // 4 万+ 行情/委托事件 + 5 个收盘
		if len(evs) < 40_000 {
			t.Fatalf("replay too short: %d", len(evs))
		}
		var m1, m2 runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m1)
		eq1, days1, _ := runReplay(evs, false)
		runtime.ReadMemStats(&m2)
		if days1 != 5 {
			t.Fatalf("closed days=%d", days1)
		}
		heapMB := float64(m2.HeapAlloc-m1.HeapAlloc) / 1024 / 1024
		if heapMB > 512 {
			t.Fatalf("heap growth too large: %.1f MB", heapMB)
		}
		t.Logf("events=%d equity=%.2f heapGrowth=%.1fMB", len(evs), eq1, heapMB)
	})

	register("回放压测", "并发分片回放与串行回放结果一致", func(t *testing.T) {
		evs := buildReplay(3, 4000)
		eq1, days1, cd1 := runReplay(evs, false)
		eq2, days2, cd2 := runReplay(evs, true)
		if days1 != days2 || days1 != 3 || eq1 != eq2 {
			t.Fatalf("divergence: eq %.4f vs %.4f, days %v/%v %v/%v", eq1, eq2, days1, days2, cd1, cd2)
		}
	})

	register("回放压测", "整段重复回放幂等不产生二次成交", func(t *testing.T) {
		evs := buildReplay(2, 2000)
		e := newTestEngine(1_000_000_000)
		e.Process(evs)
		snap1, _, err := e.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		e.Process(evs) // 全部重复
		snap2, _, err := e.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if snap1.Equity != snap2.Equity || snap1.Cash != snap2.Cash {
			t.Fatalf("replay not idempotent: %.4f vs %.4f", snap1.Equity, snap2.Equity)
		}
	})
}
