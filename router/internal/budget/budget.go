// Package budget enforces reserved Firecrawl Cloud credit limits.
package budget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ErrLimitExceeded indicates that a reservation would exceed a configured limit.
var ErrLimitExceeded = errors.New("cloud credit budget exceeded")

// Ledger atomically reserves estimated credits before a cloud request.
type Ledger interface {
	Reserve(context.Context, int) error
}

// Options configures calendar limits and an optional token-bucket burst guard.
// A zero limit is unlimited. The burst guard is disabled when both burst
// values are zero.
type Options struct {
	DailyLimit          int
	MonthlyLimit        int
	MonthlyResetDay     int
	BurstCredits        int
	RefillCreditsPerDay int
}

type state struct {
	Day             string `json:"day"`
	Month           string `json:"month"`
	DailyUsed       int    `json:"dailyUsed"`
	MonthlyUsed     int    `json:"monthlyUsed"`
	MonthlyResetDay int    `json:"monthlyResetDay,omitempty"`
	BurstBalance    *int64 `json:"burstBalanceCreditNanoseconds,omitempty"`
	BurstUpdatedAt  int64  `json:"burstUpdatedUnixNano,omitempty"`
}

// Memory is a concurrency-safe in-memory ledger.
type Memory struct {
	mu                  sync.Mutex
	dailyLimit          int
	monthlyLimit        int
	resetDay            int
	clock               func() time.Time
	state               state
	burstCredits        int
	refillCreditsPerDay int
}

// File is a concurrency-safe ledger persisted as an atomic JSON snapshot.
type File struct {
	mu                  sync.Mutex
	path                string
	dailyLimit          int
	monthlyLimit        int
	resetDay            int
	clock               func() time.Time
	state               state
	burstCredits        int
	refillCreditsPerDay int
}

// NewMemory creates an in-memory ledger. A zero limit is unlimited.
func NewMemory(dailyLimit, monthlyLimit int, clock func() time.Time) *Memory {
	return NewMemoryWithResetDay(dailyLimit, monthlyLimit, 1, clock)
}

// NewMemoryWithResetDay creates an in-memory ledger whose monthly billing
// period rolls over at 00:00 UTC on resetDay.
func NewMemoryWithResetDay(dailyLimit, monthlyLimit, resetDay int, clock func() time.Time) *Memory {
	ledger, _ := NewMemoryWithOptions(Options{DailyLimit: dailyLimit, MonthlyLimit: monthlyLimit, MonthlyResetDay: resetDay}, clock)
	return ledger
}

// NewMemoryWithOptions creates an in-memory ledger with an explicit policy.
func NewMemoryWithOptions(options Options, clock func() time.Time) (*Memory, error) {
	if err := validateOptions(options); err != nil {
		return nil, err
	}
	if clock == nil {
		clock = time.Now
	}
	return &Memory{
		dailyLimit:          options.DailyLimit,
		monthlyLimit:        options.MonthlyLimit,
		resetDay:            options.MonthlyResetDay,
		burstCredits:        options.BurstCredits,
		refillCreditsPerDay: options.RefillCreditsPerDay,
		clock:               clock,
		state:               state{MonthlyResetDay: options.MonthlyResetDay},
	}, nil
}

// NewFile opens a file-backed ledger, loading an existing snapshot when present.
func NewFile(path string, dailyLimit, monthlyLimit int, clock func() time.Time) (*File, error) {
	return NewFileWithResetDay(path, dailyLimit, monthlyLimit, 1, clock)
}

// NewFileWithResetDay opens a file-backed ledger whose monthly billing period
// rolls over at 00:00 UTC on resetDay.
func NewFileWithResetDay(path string, dailyLimit, monthlyLimit, resetDay int, clock func() time.Time) (*File, error) {
	return NewFileWithOptions(path, Options{DailyLimit: dailyLimit, MonthlyLimit: monthlyLimit, MonthlyResetDay: resetDay}, clock)
}

// NewFileWithOptions opens a file-backed ledger with an explicit policy.
func NewFileWithOptions(path string, options Options, clock func() time.Time) (*File, error) {
	if err := validateOptions(options); err != nil {
		return nil, err
	}
	if clock == nil {
		clock = time.Now
	}
	l := &File{
		path: path, dailyLimit: options.DailyLimit, monthlyLimit: options.MonthlyLimit,
		resetDay: options.MonthlyResetDay, burstCredits: options.BurstCredits,
		refillCreditsPerDay: options.RefillCreditsPerDay, clock: clock,
	}
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		l.state.MonthlyResetDay = options.MonthlyResetDay
		return l, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read budget ledger: %w", err)
	}
	if err := json.Unmarshal(contents, &l.state); err != nil {
		return nil, fmt.Errorf("decode budget ledger: %w", err)
	}
	storedResetDay := l.state.MonthlyResetDay
	legacy := storedResetDay == 0
	if legacy {
		storedResetDay = 1
	}
	if storedResetDay != options.MonthlyResetDay {
		l.state.Month = monthlyPeriod(clock(), options.MonthlyResetDay)
	}
	if legacy || storedResetDay != options.MonthlyResetDay {
		l.state.MonthlyResetDay = options.MonthlyResetDay
		if err := persist(path, l.state); err != nil {
			return nil, fmt.Errorf("migrate budget ledger reset day: %w", err)
		}
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("secure budget ledger: %w", err)
	}
	return l, nil
}

func validateOptions(options Options) error {
	if options.BurstCredits < 0 || options.RefillCreditsPerDay < 0 {
		return fmt.Errorf("token-bucket values must not be negative")
	}
	if (options.BurstCredits == 0) != (options.RefillCreditsPerDay == 0) {
		return fmt.Errorf("burst credits and refill credits per day must both be zero or both be positive")
	}
	if int64(options.BurstCredits) > math.MaxInt64/int64(24*time.Hour) {
		return fmt.Errorf("burst credits are too large")
	}
	return nil
}

// Reserve records credits unless doing so would exceed a positive limit.
func (l *Memory) Reserve(ctx context.Context, credits int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if credits < 0 {
		return fmt.Errorf("credits must not be negative")
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	return reserve(&l.state, l.dailyLimit, l.monthlyLimit, l.resetDay, l.burstCredits, l.refillCreditsPerDay, l.clock(), credits)
}

// Reserve persists the updated counts before reporting a successful reservation.
func (l *File) Reserve(ctx context.Context, credits int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if credits < 0 {
		return fmt.Errorf("credits must not be negative")
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	next := l.state
	if l.state.BurstBalance != nil {
		balance := *l.state.BurstBalance
		next.BurstBalance = &balance
	}
	if err := reserve(&next, l.dailyLimit, l.monthlyLimit, l.resetDay, l.burstCredits, l.refillCreditsPerDay, l.clock(), credits); err != nil {
		return err
	}
	if err := persist(l.path, next); err != nil {
		return err
	}
	l.state = next
	return nil
}

func reserve(current *state, dailyLimit, monthlyLimit, resetDay, burstCredits, refillCreditsPerDay int, now time.Time, credits int) error {
	now = now.UTC()
	day := now.Format("2006-01-02")
	month := monthlyPeriod(now, resetDay)
	if current.Day != day {
		current.Day, current.DailyUsed = day, 0
	}
	newBillingCycle := current.Month != "" && current.Month != month
	if current.Month != month {
		current.Month, current.MonthlyUsed = month, 0
	}
	current.MonthlyResetDay = resetDay
	if dailyLimit > 0 && current.DailyUsed+credits > dailyLimit {
		return ErrLimitExceeded
	}
	if monthlyLimit > 0 && current.MonthlyUsed+credits > monthlyLimit {
		return ErrLimitExceeded
	}
	if burstCredits > 0 {
		const unitsPerCredit = int64(24 * time.Hour)
		if newBillingCycle {
			current.BurstBalance = nil
		}
		if current.BurstBalance == nil {
			balance := int64(burstCredits) * unitsPerCredit
			current.BurstBalance = &balance
			current.BurstUpdatedAt = now.UnixNano()
		} else {
			capacity := int64(burstCredits) * unitsPerCredit
			if *current.BurstBalance > capacity {
				*current.BurstBalance = capacity
			}
			if elapsed := now.UnixNano() - current.BurstUpdatedAt; elapsed > 0 {
				room := capacity - *current.BurstBalance
				if room > 0 {
					refillRate := int64(refillCreditsPerDay)
					refillTime := room / refillRate
					if room%refillRate != 0 {
						refillTime++
					}
					if elapsed >= refillTime {
						*current.BurstBalance = capacity
					} else {
						*current.BurstBalance += elapsed * refillRate
					}
				}
				current.BurstUpdatedAt = now.UnixNano()
			}
		}
		if credits > burstCredits {
			return ErrLimitExceeded
		}
		cost := int64(credits) * unitsPerCredit
		if cost > *current.BurstBalance {
			return ErrLimitExceeded
		}
		*current.BurstBalance -= cost
	}
	current.DailyUsed += credits
	current.MonthlyUsed += credits
	return nil
}

func monthlyPeriod(now time.Time, resetDay int) string {
	now = now.UTC()
	if now.Day() < resetDay {
		now = now.AddDate(0, -1, 0)
	}
	return now.Format("2006-01")
}

func persist(path string, current state) error {
	contents, err := json.Marshal(current)
	if err != nil {
		return fmt.Errorf("encode budget ledger: %w", err)
	}
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create budget ledger temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("secure budget ledger temporary file: %w", err)
	}
	if _, err := temporary.Write(contents); err != nil {
		temporary.Close()
		return fmt.Errorf("write budget ledger: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync budget ledger: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close budget ledger: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace budget ledger: %w", err)
	}
	dir, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("open budget ledger directory: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync budget ledger directory: %w", err)
	}
	return nil
}
