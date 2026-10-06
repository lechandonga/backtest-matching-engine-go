package engine

import (
	"fmt"
	"math"
	"sync"
	"testing"

	"github.com/lechandonga/backtest-matching-engine-go/event"
	"github.com/lechandonga/backtest-matching-engine-go/portfolio"
)

// 生成跨 days 个交易日、约 n 条事件的确定性回放（简单线性同余）。
func genReplay(n, days int, symbols int, seed int64, duplicate bool) []event.Event {
	rng := seed
	next := func() int64 {
		rng = (rng*1103515245 + 12345) & 0x7fffffff
		return rng
	}
	evs := make([]event.Event, 0, n+n/50)
	perDay := n / days
	sym := func(i int64) string { return fmt.Sprintf("S%03d", int(i%int64(symbols))) }
	var seq int64
	for d := 1; d <= days; d++ {
		for i := 0; i < perDay; i++ {
			seq++
			s := sym(seq)
			px := 50 + float64(seq%200)/4
			bid, ask := px-0.5, px+0.5
			switch next() % 5 {
			case 0, 1:
				evs = append(evs, event.Event{EventID: fmt.Sprintf("q-%d", seq), Seq: seq,
					Kind: event.KindQuote, Symbol: s, BidPrice: bid, AskPrice: ask,
					BidSize: 1000, AskSize: 1000})
			case 2:
				evs = append(evs, event.Event{EventID: fmt.Sprintf("t-%d", seq), Seq: seq,
					Kind: event.KindTrade, Symbol: s, TradePrice: px, TradeSize: 1 + next()%50})
			case 3:
				side := event.Buy
				if next()%2 == 0 {
					side = event.Sell
				}
				tif := []event.TIF{event.TIODay, event.TIFO, event.TFOK, event.TGTC}[next()%4]
				typ := event.Limit
				var stopPx, trail float64
				switch next() % 4 {
				case 0:
					typ = event.Stop
					stopPx = px + 3
					if side == event.Sell {
						stopPx = px - 3
					}
				case 1:
					typ = event.TrailingStop
					trail = 0.02
				}
				evs = append(evs, event.Event{
					EventID: fmt.Sprintf("o-%d", seq), Seq: seq, Kind: event.KindOrderSubmit,
					Symbol: s, OrderID: fmt.Sprintf("ord-%d", seq), Side: side, Type: typ,
					Price: px, StopPrice: stopPx, TrailPct: trail, Qty: 1 + next()%5, TIF: tif,
				})
			case 4:
				evs = append(evs, event.Event{EventID: fmt.Sprintf("t2-%d", seq), Seq: seq,
					Kind: event.KindTrade, Symbol: s, TradePrice: px, TradeSize: 1 + next()%20})
			}
			if duplicate && next()%47 == 0 && len(evs) > 0 {
				evs = append(evs, evs[len(evs)-1]) // 重复投递
			}
		}
		for s := 0; s < symbols; s++ {
			seq++
			evs = append(evs, event.Event{EventID: fmt.Sprintf("close-%d-%d", d, s), Seq: seq,
				Kind: event.KindDayClose, Symbol: sym(int64(s)), TradingDay: int64(d),
				ClosePrice: 50 + float64(seq%200)/4})
		}
	}
	return evs
}

func replayState(eng *Engine) (float64, int64, int) {
	snap, _ := eng.Snapshot()
	var open, waiting int
	for _, o := range snap.Orders {
		switch o.Status {
		case StatusNew, StatusPartiallyFilled:
			open++
		case StatusWaitingTrigger:
			waiting++
		}
	}
	return snap.Portfolio.Equity, snap.TradingDay, open + waiting
}

// 三万条事件、5 个交易日、8 个标的：两次回放结果完全一致；耗时与内存可控。
func TestLargeReplayStable(t *testing.T) {
	ok := group(t, grpReplay)
	run := func(concurrent bool) (float64, int64, int) {
		pf := portfolio.New(1e10, 0.0001)
		eng := New(pf)
		evs := genReplay(30000, 5, 8, 42, true)
		if concurrent {
			var wg sync.WaitGroup
			chunk := 200
			for i := 0; i < len(evs); i += chunk {
				end := i + chunk
				if end > len(evs) {
					end = len(evs)
				}
				wg.Add(1)
				go func(batch []event.Event) { defer wg.Done(); eng.Submit(batch...) }(evs[i:end])
			}
			wg.Wait()
			eng.Drain()
		} else {
			eng.Process(evs...)
		}
		eq, day, open := replayState(eng)
		snap, err := eng.Snapshot()
		ok(err == nil)
		return eq, day, open + len(snap.WaitingTrigger)
	}

	eq1, day1, open1 := run(false)
	eq2, day2, open2 := run(true)
	ok(day1 == 5 && day1 == day2)
	ok(math.Abs(eq1-eq2) < 1e-4)
	ok(open1 == open2)
}

func BenchmarkReplay30k(b *testing.B) {
	evs := genReplay(30000, 5, 8, 42, false)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		eng := New(portfolio.New(1e10, 0))
		eng.Process(evs...)
		if _, err := eng.Snapshot(); err != nil {
			b.Fatal(err)
		}
	}
}
