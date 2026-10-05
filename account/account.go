// Package account 实现回测的组合核算：现金、持仓、已实现/未实现盈亏、
// 费用与公司行为，并在任意时刻强制对账。
//
// 权益恒等式（对不上时 Reconcile 显式返回 *ReconcileError，不静默吸收）：
//
//	Equity = InitialCash + RealizedPnL + UnrealizedPnL - Fees + CorporateAdjustments
//
// 所有成交按 FillID 幂等：同一条成交重复记账不会重复扣款或重复计入盈亏。
package account

import (
	"errors"
	"fmt"
	"sync"
)

var (
	ErrInsufficientCash     = errors.New("insufficient cash")
	ErrInsufficientPosition = errors.New("insufficient position")
	ErrUnknownReservation   = errors.New("unknown order reservation")
	ErrDuplicateReservation = errors.New("reservation already exists")
	ErrDuplicateFill        = errors.New("duplicate fill with different content")
)

// FeeModel 计算单笔成交费用。
type FeeModel interface {
	Fee(notional, qty float64) float64
}

// FeeFunc 让普通函数实现 FeeModel。
type FeeFunc func(notional, qty float64) float64

// Fee implements FeeModel.
func (f FeeFunc) Fee(notional, qty float64) float64 { return f(notional, qty) }

// ZeroFee 零费用模型。
var ZeroFee FeeFunc = func(_, _ float64) float64 { return 0 }

// BpsFee 按成交金额收费，bps=10 表示 0.1%。
func BpsFee(bps float64) FeeFunc {
	return func(notional, _ float64) float64 { return notional * bps / 10000 }
}

// Fill 是一笔成交。
type Fill struct {
	FillID string
	Symbol string
	Side   string
	Qty    float64
	Price  float64
	Fee    float64
}

// Position 是持仓快照。
type Position struct {
	Symbol  string
	Qty     float64
	AvgCost float64
}

// Snapshot 是某一时刻的账户核算快照。
type Snapshot struct {
	InitialCash          float64
	Cash                 float64
	RealizedPnL          float64
	UnrealizedPnL        float64
	Fees                 float64
	CorporateAdjustments float64
	Equity               float64
	Positions            []Position
}

// ReconcileError 表示权益恒等式对不上。
type ReconcileError struct {
	Snap Snapshot
	Want float64
	Got  float64
	Diff float64
}

func (e *ReconcileError) Error() string {
	return fmt.Sprintf("account reconcile mismatch: got equity %.12f, identity %.12f, diff %.3e",
		e.Got, e.Want, e.Diff)
}

type reserveKind int

const (
	reserveBuy reserveKind = iota
	reserveSell
)

type reservation struct {
	kind   reserveKind
	symbol string
	amount float64
}

type position struct {
	qty     float64
	avgCost float64
}

// LedgerEntry 是不可变账务流水，用于可解释性与排查。
type LedgerEntry struct {
	Kind     string
	RefID    string
	Symbol   string
	Qty      float64
	Price    float64
	Amount   float64
	Realized float64
}

// Account 是线程安全的组合账户。
type Account struct {
	mu sync.Mutex

	initialCash float64
	cash        float64

	positions map[string]*position
	reserves  map[string]*reservation

	realizedPnL  float64
	fees         float64
	corporateAdj float64

	seenFills map[string]Fill
	ledger    []LedgerEntry
}

// New 创建账户，initialCash 为初始资金。
func New(initialCash float64) *Account {
	if initialCash < 0 {
		panic("initial cash must be >= 0")
	}
	return &Account{
		initialCash: initialCash,
		cash:        initialCash,
		positions:   make(map[string]*position),
		reserves:    make(map[string]*reservation),
		seenFills:   make(map[string]Fill),
	}
}
