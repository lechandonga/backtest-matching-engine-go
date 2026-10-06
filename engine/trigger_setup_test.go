package engine

import (
	"testing"

	"github.com/lechandonga/backtest-matching-engine-go/event"
)

// buildBook：在 sym 上准备 totalQty 股可卖持仓（一张全部成交的买单），
// 并按 wallPrice 挂 wallQty 的卖单留在簿中，供触发后的买市价吃单。
func buildSellWall(t *testing.T, eng *Engine, seq int64, sym string, askPx float64, total, wall int64) {
	t.Helper()
	eng.Process(event.Event{EventID: "wallbuy-" + sym + "-" + itoa(seq), Seq: seq,
		Kind: event.KindOrderSubmit, Symbol: sym, OrderID: "wallbuy-" + sym + "-" + itoa(seq),
		Side: event.Buy, Type: event.Limit, Price: askPx + 1, Qty: total})
	eng.Process(event.Event{EventID: "wallsell-" + sym + "-" + itoa(seq+1), Seq: seq + 1,
		Kind: event.KindOrderSubmit, Symbol: sym, OrderID: "wallsell-" + sym + "-" + itoa(seq+1),
		Side: event.Sell, Type: event.Limit, Price: askPx, Qty: wall})
}

// buildBuyWall：准备一张 bidPrice 的买单留在簿中（需要账户内已有足额现金）。
func buildBuyWall(t *testing.T, eng *Engine, seq int64, sym string, bidPx float64, wall int64, oid string) {
	t.Helper()
	eng.Process(event.Event{EventID: oid, Seq: seq, Kind: event.KindOrderSubmit,
		Symbol: sym, OrderID: oid, Side: event.Buy, Type: event.Limit,
		Price: bidPx, Qty: wall})
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// buildSellWallEvents 与 buildSellWall 相同，但返回事件供调用方自行 Submit。
func buildSellWallEvents(seq int64, sym string, wallPx float64, total, wall int64) []event.Event {
	buyID := "wallbuy-" + sym + "-" + itoa(seq)
	sellID := "wallsell-" + sym + "-" + itoa(seq+1)
	return []event.Event{
		{EventID: "ev-" + buyID, Seq: seq, Kind: event.KindOrderSubmit, Symbol: sym,
			OrderID: buyID, Side: event.Buy, Type: event.Limit, Price: wallPx + 1, Qty: total,
			TIF: event.TGTC},
		{EventID: "ev-" + sellID, Seq: seq + 1, Kind: event.KindOrderSubmit, Symbol: sym,
			OrderID: sellID, Side: event.Sell, Type: event.Limit, Price: wallPx, Qty: wall,
			TIF: event.TGTC},
	}
}
