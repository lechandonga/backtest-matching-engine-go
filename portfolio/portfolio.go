// Package portfolio 实现现金、持仓、冻结、已实现/未实现盈亏与公司行为核算。
//
// 账户权益恒等式（每个事件处理后都会校验）：
//
//	权益 = 现金 + 冻结现金 + Σ 持仓数量 × 最新价
//	     = 初始资金 + 已实现盈亏 + 未实现盈亏 − 累计费用 + 公司行为调整
//
// 等式两侧差额超过容差时返回 ReconcileError，绝不静默吸收。
package portfolio

import (
	"fmt"
	"math"
	"sync"
)

// Epsilon 浮点对账容差（绝对 + 相对）。
const (
	absEps = 1e-9
	relEps = 1e-9
)

// ReconcileError 权益拆解对不上时抛出。
type ReconcileError struct {
	Equity   float64
	Expected float64
	Diff     float64
	Detail   string
}

func (e *ReconcileError) Error() string {
	return fmt.Sprintf("portfolio reconcile failed: equity=%.8f expected=%.8f diff=%.3e (%s)",
		e.Equity, e.Expected, e.Diff, e.Detail)
}

// Position 单标的持仓（本实现只支持多头、T+0 可卖）。
type Position struct {
	Qty     int64   // 可用持仓
	Frozen  int64   // 冻结持仓（待卖出）
	AvgCost float64 // 每股持仓成本
}

// Snapshot 账户快照。
type Snapshot struct {
	InitialCash      float64
	Cash             float64
	FrozenCash       float64
	RealizedPnL      float64
	UnrealizedPnL    float64
	Fees             float64
	CorporateActions float64
	Equity           float64
	Positions        map[string]Position
	Marks            map[string]float64
}

// Portfolio 线程安全的组合账户。
type Portfolio struct {
	mu sync.Mutex

	initialCash float64
	cash        float64
	frozenCash  float64

	positions map[string]*Position
	marks     map[string]float64

	realizedPnL float64
	fees        float64
	corpActions float64

	feeRate float64
}

// New 创建账户，initialCash 为初始资金，feeRate 为按成交金额收取的费用比例。
func New(initialCash float64, feeRate float64) *Portfolio {
	if initialCash < 0 || feeRate < 0 {
		panic("portfolio: negative initial cash or fee rate")
	}
	return &Portfolio{
		initialCash: initialCash,
		cash:        initialCash,
		positions:   make(map[string]*Position),
		marks:       make(map[string]float64),
		feeRate:    feeRate,
	}
}

func (p *Portfolio) fee(notional float64) float64 { return notional * p.feeRate }

// SetMark 更新某标的盯市价格（不改变任何现金流与盈亏）。
func (p *Portfolio) SetMark(symbol string, price float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if price <= 0 {
		panic("portfolio: non-positive mark price")
	}
	p.marks[symbol] = price
}

// ReserveBuy 买入下单时冻结预估资金（限价按限价、市价由调用方按对手价传入）。
func (p *Portfolio) ReserveBuy(symbol string, qty int64, refPrice float64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	need := refPrice * float64(qty) * (1 + p.feeRate)
	if p.cash < need-absEps {
		return fmt.Errorf("insufficient cash: available=%.4f need=%.4f", p.cash, need)
	}
	p.cash -= need
	p.frozenCash += need
	return nil
}

// ReserveSell 卖出下单时冻结持仓。
func (p *Portfolio) ReserveSell(symbol string, qty int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	pos := p.positions[symbol]
	if pos == nil || pos.Qty < qty {
		avail := int64(0)
		if pos != nil {
			avail = pos.Qty
		}
		return fmt.Errorf("insufficient position %s: available=%d need=%d", symbol, avail, qty)
	}
	pos.Qty -= qty
	pos.Frozen += qty
	return nil
}

// ReleaseBuy 撤单/剩余终止时释放买入冻结资金。
func (p *Portfolio) ReleaseBuy(refPrice float64, qty int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	amount := refPrice * float64(qty) * (1 + p.feeRate)
	p.frozenCash -= amount
	p.cash += amount
}

// ReleaseSell 撤单/剩余终止时释放卖出冻结持仓。
func (p *Portfolio) ReleaseSell(symbol string, qty int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pos := p.positions[symbol]
	if pos == nil {
		panic("portfolio: release sell on missing position")
	}
	pos.Frozen -= qty
	pos.Qty += qty
}

// FillBuy 买入成交：释放对应冻结，扣减实际价款与费用，增加持仓成本。
func (p *Portfolio) FillBuy(symbol string, qty int64, price, refPrice float64) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	reserved := refPrice * float64(qty) * (1 + p.feeRate)
	notional := price * float64(qty)
	fee := p.fee(notional)

	p.frozenCash -= reserved
	cost := notional + fee
	// 冻结按 refPrice 预估：多退少补从现金出。
	p.cash += reserved - cost

	pos := p.positions[symbol]
	if pos == nil {
		pos = &Position{}
		p.positions[symbol] = pos
	}
	newQty := pos.Qty + qty
	// 持仓成本不含交易费用：费用在权益拆解中单独扣减，避免双扣。
	pos.AvgCost = (pos.AvgCost*float64(pos.Qty) + notional) / float64(newQty)
	pos.Qty = newQty
	p.fees += fee

	p.marks[symbol] = price
	return p.reconcileLocked("fill buy "+symbol)
}

// FillSell 卖出成交：释放冻结持仓，回收价款（扣费用），结转已实现盈亏。
func (p *Portfolio) FillSell(symbol string, qty int64, price float64) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	pos := p.positions[symbol]
	if pos == nil || pos.Frozen < qty {
		panic("portfolio: sell fill without reserved position")
	}
	notional := price * float64(qty)
	fee := p.fee(notional)

	// 统一口径：已实现盈亏不含交易费用，费用在权益拆解中单独扣减。
	p.realizedPnL += notional - pos.AvgCost*float64(qty)

	pos.Frozen -= qty
	if pos.Qty == 0 && pos.Frozen == 0 {
		pos.AvgCost = 0
	}
	p.cash += notional - fee
	p.fees += fee
	p.marks[symbol] = price
	return p.reconcileLocked("fill sell "+symbol)
}

// ApplyDividend 现金分红：按当前总持仓（含冻结）派现。
func (p *Portfolio) ApplyDividend(symbol string, perShare float64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	pos := p.positions[symbol]
	total := int64(0)
	if pos != nil {
		total = pos.Qty + pos.Frozen
	}
	amount := perShare * float64(total)
	p.cash += amount
	p.corpActions += amount
	return p.reconcileLocked("dividend " + symbol)
}

// ApplySplit 拆股：总持仓与每股成本按比例调整，除权参考价用于盯市。
func (p *Portfolio) ApplySplit(symbol string, ratio, postPrice float64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	pos := p.positions[symbol]
	if pos != nil && (pos.Qty != 0 || pos.Frozen != 0) {
		newQty := float64(pos.Qty) * ratio
		newFrozen := float64(pos.Frozen) * ratio
		if newQty != math.Tr(newQty) || newFrozen != math.Tr(newFrozen) {
			return fmt.Errorf("split ratio %g produces fractional shares for %s", ratio, symbol)
		}
		pos.Qty = int64(newQty)
		pos.Frozen = int64(newFrozen)
		pos.AvgCost /= ratio
	}
	if postPrice > 0 {
		p.marks[symbol] = postPrice
	}
	return p.reconcileLocked("split " + symbol)
}

func approxEqual(a, b float64) bool {
	diff := math.Abs(a - b)
	return diff <= absEps+relEps*math.Max(math.Abs(a), math.Abs(b))
}

// reconcileLocked 校验权益恒等式。
func (p *Portfolio) reconcileLocked(detail string) error {
	var holdingValue, unrealized float64
	for sym, pos := range p.positions {
		total := float64(pos.Qty + pos.Frozen)
		mark := p.marks[sym]
		holdingValue += total * mark
		unrealized += total * (mark - pos.AvgCost)
	}
	equity := p.cash + p.frozenCash + holdingValue
	expected := p.initialCash + p.realizedPnL + unrealized - p.fees + p.corpActions
	if !approxEqual(equity, expected) {
		return &ReconcileError{
			Equity:   equity,
			Expected: expected,
			Diff:     equity - expected,
			Detail:   detail,
		}
	}
	return nil
}

// Snapshot 返回账户快照（复制值），并执行一次对账。
func (p *Portfolio) Snapshot() (Snapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	snap := Snapshot{
		InitialCash:      p.initialCash,
		Cash:             p.cash,
		FrozenCash:       p.frozenCash,
		RealizedPnL:      p.realizedPnL,
		Fees:             p.fees,
		CorporateActions: p.corpActions,
		Positions:        make(map[string]Position, len(p.positions)),
		Marks:            make(map[string]float64, len(p.marks)),
	}
	for sym, pos := range p.positions {
		snap.Positions[sym] = *pos
	}
	for sym, m := range p.marks {
		snap.Marks[sym] = m
	}
	for sym, pos := range p.positions {
		mark := p.marks[sym]
		total := float64(pos.Qty + pos.Frozen)
		snap.UnrealizedPnL += total * (mark - pos.AvgCost)
		snap.Equity += total * mark
	}
	snap.Equity += p.cash + p.frozenCash
	if err := p.reconcileLocked("snapshot"); err != nil {
		return snap, err
	}
	return snap, nil
}
