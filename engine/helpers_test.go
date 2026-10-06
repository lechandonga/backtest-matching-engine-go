package engine

import (
	"testing"

	"github.com/lechandonga/backtest-matching-engine-go/event"
	"github.com/lechandonga/backtest-matching-engine-go/portfolio"
)

const testFeeRate = 0.0

func newTestEngine(cash float64) *Engine {
	return New(portfolio.New(cash, testFeeRate))
}

func submitEvent(id string, seq int64, symbol string, side event.Side, typ event.OrderType, qty int64, price float64, tif event.TimeInForce) event.Event {
	return event.Event{
		EventID:   "ev-" + id,
		Seq:       seq,
		Kind:      event.KindOrderSubmit,
		Symbol:    symbol,
		OrderID:   "ord-" + id,
		Side:      side,
		Type:      typ,
		Qty:       qty,
		Price:     price,
		TIF:       tif,
		TradeDate: "D1",
	}
}

func tradeEvent(id string, seq int64, symbol string, price float64, size int64, date string) event.Event {
	return event.Event{EventID: "ev-" + id, Seq: seq, Kind: event.KindTrade, Symbol: symbol, TradePrice: price, TradeSize: size, TradeDate: date}
}

func quoteEvent(id string, seq int64, symbol string, bid, ask float64, bidSize, askSize int64, date string) event.Event {
	return event.Event{EventID: "ev-" + id, Seq: seq, Kind: event.KindQuote, Symbol: symbol, BidPrice: bid, AskPrice: ask, BidSize: bidSize, AskSize: askSize, TradeDate: date}
}

func closeEvent(id string, seq int64, symbol string, price float64, date string) event.Event {
	return event.Event{EventID: "ev-" + id, Seq: seq, Kind: event.KindSessionClose, Symbol: symbol, ClosePrice: price, TradeDate: date}
}

func cancelEvent(id string, seq int64, orderID string, qty int64, date string) event.Event {
	return event.Event{EventID: "ev-cx-" + id, Seq: seq, Kind: event.KindOrderCancel, Symbol: "X", OrderID: orderID, Qty: qty, TradeDate: date}
}

func mustAccept(t *testing.T, r Result) {
	t.Helper()
	if !r.Accepted {
		t.Fatalf("expected accepted, got reject %s: %s", r.Reject.Reason, r.Reject.Detail)
	}
}

func mustReject(t *testing.T, r Result, reason RejectReason) {
	t.Helper()
	if r.Accepted || r.Reject == nil {
		t.Fatalf("expected reject %s, got accepted", reason)
	}
	if r.Reject.Reason != reason {
		t.Fatalf("expected reject reason %s, got %s", reason, r.Reject.Reason)
	}
}

func statusOf(e *Engine, id string) OrderStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.orders[id].Status
}
