package engine

import (
	"errors"
	"sort"

	"github.com/lechandonga/backtest-matching-engine-go/account"
	"github.com/lechandonga/backtest-matching-engine-go/event"
)

func errorIs(err, target error) bool { return errors.Is(err, target) }

// worstBuyPriceLocked 给出市价买预留用的保守价格：
// 外部卖报价最高与挂簿最低卖价中取较高者（市价单可能吃到的最差价）。
func worstBuyPriceLocked(b *symbolBook) (float64, bool) {
	var price float64
	found := false
	if b.hasPrice && b.last > 0 {
		price, found = b.last, true
	}
	for _, l := range b.external {
		if !found || l.Price > price {
			price, found = l.Price, true
		}
	}
	if len(b.asks) > 0 {
		ask := b.asks[0].order.Price
		if !found || ask > price {
			price, found = ask, true
		}
	}
	return price, found
}

func currentExternalLocked(b *symbolBook) []event.Liquidity {
	return cloneLiquidity(b.external)
}

// execute 记账一笔撮合成交，并同步订单状态与预留。
func (e *Engine) execute(aggr, passive *Order, qty, price float64, seq int64,
	liq *[]event.Liquidity, liqIdx *int, fills *[]Fill) error {

	e.mu.Lock()
	e.fillSeq++
	n := e.fillSeq
	e.mu.Unlock()

	fee := e.fee.Fee(qty*price, qty)
	fid := e.newFillID(seq, int(n))

	fill := account.Fill{
		FillID: fid,
		Symbol: aggr.Symbol,
		Side:   aggr.Side,
		Qty:    qty,
		Price:  price,
		Fee:    fee,
	}
	if aggr.Side == string(event.Buy) {
		if err := e.acct.BuyFilled(fill); err != nil {
			return err
		}
	} else {
		if err := e.acct.SellFilled(fill); err != nil {
			return err
		}
	}

	*fills = append(*fills, Fill{
		ID: fid, Seq: seq, Symbol: aggr.Symbol, OrderID: aggr.ID,
		Side: aggr.Side, Qty: qty, Price: price, Fee: fee,
	})

	// 主动方状态
	aggrTarget := StatusPartiallyFilled
	if qty >= aggr.Remaining()-1e-12 {
		aggrTarget = StatusFilled
	}
	if err := aggr.applyFill(min(qty, aggr.Remaining()), price, aggrTarget); err != nil {
		return err
	}
	if err := syncReservation(aggr, e, price); err != nil {
		return err
	}

	if passive != nil {
		pFee := e.fee.Fee(qty*price, qty)
		pfid := fid + "-P"
		pf := account.Fill{
			FillID: pfid, Symbol: passive.Symbol, Side: passive.Side,
			Qty: qty, Price: price, Fee: pFee,
		}
		if passive.Side == string(event.Buy) {
			if err := e.acct.BuyFilled(pf); err != nil {
				return err
			}
		} else {
			if err := e.acct.SellFilled(pf); err != nil {
				return err
			}
		}
		*fills = append(*fills, Fill{
			ID: pfid, Seq: seq, Symbol: passive.Symbol, OrderID: passive.ID,
			Side: passive.Side, Qty: qty, Price: price, Fee: pFee,
		})
		pTarget := StatusPartiallyFilled
		if qty >= passive.Remaining()-1e-12 {
			pTarget = StatusFilled
		}
		if err := passive.applyFill(min(qty, passive.Remaining()), price, pTarget); err != nil {
			return err
		}
		if err := syncReservation(passive, e, price); err != nil {
			return err
		}
	}
	return nil
}

func syncReservation(o *Order, e *Engine, fillPrice float64) error {
	rem := o.Remaining()
	if rem <= 1e-12 {
		return e.acct.Release(o.ID)
	}
	if o.Side == string(event.Buy) {
		price := o.Price
		if o.Type == string(event.Market) {
			price = fillPrice
		}
		return e.acct.AdjustBuyReservation(o.ID, o.Symbol, rem, price, e.fee)
	}
	return e.acct.AdjustSellReservation(o.ID, o.Symbol, rem)
}

// sweepAggressiveLocked 让主动单（新提交）先吃外部流动性，再吃对手挂簿。
func (e *Engine) sweepAggressiveLocked(b *symbolBook, o *Order, seq int64, liq []event.Liquidity) []Fill {
	var fills []Fill
	// 外部流动性：买吃最低卖价，卖吃最高买价。
	sort.Slice(liq, func(i, j int) bool {
		if o.Side == string(event.Buy) {
			return liq[i].Price < liq[j].Price
		}
		return liq[i].Price > liq[j].Price
	})
	for i := range liq {
		if o.Remaining() <= 1e-12 {
			break
		}
		l := &liq[i]
		if !crossable(o, l.Price) {
			continue
		}
		qty := min(o.Remaining(), l.Qty)
		if qty <= 1e-12 {
			continue
		}
		if err := e.execute(o, nil, qty, l.Price, seq, &liq, &i, &fills); err != nil {
			panic("accounting invariant broken on aggressive fill: " + err.Error())
		}
		l.Qty -= qty
	}

	// 再吃对手挂簿
	if o.Side == string(event.Buy) {
		e.matchWithBook(b, o, seq, &fills, true)
	} else {
		e.matchWithBook(b, o, seq, &fills, false)
	}
	b.external = pruneLiquidity(liq)
	return fills
}

// sweepBookLocked 在新行情到达时，用外部报价驱动挂簿双向撮合。
func (e *Engine) sweepBookLocked(b *symbolBook, sym string, seq int64, incoming []event.Liquidity) []Fill {
	var fills []Fill
	combined := mergeExternal(b.external, incoming)

	// 买挂单吃外部卖报价（价格升序）
	sellLevels := filterLevels(combined, true)
	for _, l := range sellLevels {
		for len(b.bids) > 0 {
			best := b.bids[0]
			o := best.order
			if o.Terminal() {
				b.bids = b.bids[1:]
				continue
			}
			if o.Price < l.Price {
				break
			}
			qty := min(o.Remaining(), l.Qty)
			if qty <= 1e-12 {
				break
			}
			if err := e.execute(o, nil, qty, l.Price, seq, &combined, nil, &fills); err != nil {
				panic("accounting invariant broken on book fill: " + err.Error())
			}
			l.Qty -= qty
			if o.Remaining() <= 1e-12 {
				b.bids = b.bids[1:]
			}
			if l.Qty <= 1e-12 {
				break
			}
		}
	}

	// 卖挂单吃外部买报价（价格降序）
	buyLevels := filterLevels(combined, false)
	for _, l := range buyLevels {
		for len(b.asks) > 0 {
			best := b.asks[0]
			o := best.order
			if o.Terminal() {
				b.asks = b.asks[1:]
				continue
			}
			if o.Price > l.Price {
				break
			}
			qty := min(o.Remaining(), l.Qty)
			if qty <= 1e-12 {
				break
			}
			if err := e.execute(o, nil, qty, l.Price, seq, &combined, nil, &fills); err != nil {
				panic("accounting invariant broken on book fill: " + err.Error())
			}
			l.Qty -= qty
			if o.Remaining() <= 1e-12 {
				b.asks = b.asks[1:]
			}
			if l.Qty <= 1e-12 {
				break
			}
		}
	}

	b.external = pruneLiquidity(combined)
	return fills
}

func (e *Engine) matchWithBook(b *symbolBook, aggr *Order, seq int64, fills *[]Fill, aggrBuy bool) {
	for {
		var book *[]*bookOrder
		if aggrBuy {
			book = &b.asks
		} else {
			book = &b.bids
		}
		if len(*book) == 0 || aggr.Remaining() <= 1e-12 {
			break
		}
		best := (*book)[0].order
		if best.Terminal() {
			*book = (*book)[1:]
			continue
		}
		px := best.Price
		if !crossable(aggr, px) {
			break
		}
		qty := min(aggr.Remaining(), best.Remaining())
		liq := b.external
		var idx int
		if err := e.execute(aggr, best, qty, px, seq, &liq, &idx, fills); err != nil {
			panic("accounting invariant broken on crossing fill: " + err.Error())
		}
		if best.Remaining() <= 1e-12 {
			*book = (*book)[1:]
		}
		if aggr.Remaining() <= 1e-12 {
			break
		}
	}
	sortSide(b.bids, true)
	sortSide(b.asks, false)
}

func crossable(o *Order, px float64) bool {
	if o.Type == string(event.Market) {
		return px > 0
	}
	if o.Side == string(event.Buy) {
		return o.Price >= px
	}
	return o.Price <= px
}

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func filterLevels(all []event.Liquidity, ascending bool) []*event.Liquidity {
	var out []*event.Liquidity
	for i := range all {
		if all[i].Qty > 1e-12 {
			out = append(out, &all[i])
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if ascending {
			return out[i].Price < out[j].Price
		}
		return out[i].Price > out[j].Price
	})
	return out
}

func mergeExternal(existing, incoming []event.Liquidity) []event.Liquidity {
	out := cloneLiquidity(existing)
	for _, l := range incoming {
		merged := false
		for i := range out {
			if out[i].Price == l.Price {
				out[i].Qty += l.Qty
				merged = true
				break
			}
		}
		if !merged {
			out = append(out, l)
		}
	}
	return out
}

func pruneLiquidity(liq []event.Liquidity) []event.Liquidity {
	out := liq[:0]
	for _, l := range liq {
		if l.Qty > 1e-12 {
			out = append(out, l)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
