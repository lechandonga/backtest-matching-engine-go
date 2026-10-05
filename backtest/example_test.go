package backtest_test

import (
	"fmt"

	"github.com/lechandonga/backtest-matching-engine-go/account"
	"github.com/lechandonga/backtest-matching-engine-go/backtest"
	"github.com/lechandonga/backtest-matching-engine-go/engine"
	"github.com/lechandonga/backtest-matching-engine-go/event"
)

func Example() {
	acct := account.New(100000)
	eng := engine.New(acct, engine.WithFeeModel(account.BpsFee(5)))
	r := backtest.New(eng)

	events := []*event.Event{
		{Seq: 1, EventID: "m1", Kind: event.KindMarket, Symbol: "AAA", LastPrice: 10,
			Liquidity: []event.Liquidity{{Price: 10, Qty: 100}}},
		{Seq: 2, EventID: "s1", Kind: event.KindSubmit, Order: event.Order{
			ID: "o1", Symbol: "AAA", Side: event.Buy, Type: event.Market, Qty: 100}},
		{Seq: 3, EventID: "m2", Kind: event.KindMarket, Symbol: "AAA", LastPrice: 11},
		{Seq: 4, EventID: "d1", Kind: event.KindDividend, CorporateSymbol: "AAA", PerShareCash: 1},
	}

	for _, o := range r.Run(events, 4) {
		if o.Err != nil {
			panic(o.Err)
		}
	}

	s, err := r.Reconcile(map[string]float64{"AAA": 11})
	if err != nil {
		panic(err)
	}
	fmt.Printf("cash=%.2f upl=%.2f fees=%.2f corp=%.2f equity=%.2f\n",
		s.Cash, s.UnrealizedPnL, s.Fees, s.CorporateAdjustments, s.Equity)

	// Output:
	// cash=99099.50 upl=100.00 fees=0.50 corp=100.00 equity=100199.50
}
