// Package budget enforces reserved Firecrawl Cloud credit limits.
package budget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

type state struct {
	Day         string `json:"day"`
	Month       string `json:"month"`
	DailyUsed   int    `json:"dailyUsed"`
	MonthlyUsed int    `json:"monthlyUsed"`
}

// Memory is a concurrency-safe in-memory ledger.
type Memory struct {
	mu           sync.Mutex
	dailyLimit   int
	monthlyLimit int
	clock        func() time.Time
	state        state
}

// File is a concurrency-safe ledger persisted as an atomic JSON snapshot.
type File struct {
	mu           sync.Mutex
	path         string
	dailyLimit   int
	monthlyLimit int
	clock        func() time.Time
	state        state
}

// NewMemory creates an in-memory ledger. A zero limit is unlimited.
func NewMemory(dailyLimit, monthlyLimit int, clock func() time.Time) *Memory {
	if clock == nil {
		clock = time.Now
	}
	return &Memory{dailyLimit: dailyLimit, monthlyLimit: monthlyLimit, clock: clock}
}

// NewFile opens a file-backed ledger, loading an existing snapshot when present.
func NewFile(path string, dailyLimit, monthlyLimit int, clock func() time.Time) (*File, error) {
	if clock == nil {
		clock = time.Now
	}
	l := &File{path: path, dailyLimit: dailyLimit, monthlyLimit: monthlyLimit, clock: clock}
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read budget ledger: %w", err)
	}
	if err := json.Unmarshal(contents, &l.state); err != nil {
		return nil, fmt.Errorf("decode budget ledger: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("secure budget ledger: %w", err)
	}
	return l, nil
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

	return reserve(&l.state, l.dailyLimit, l.monthlyLimit, l.clock(), credits)
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
	if err := reserve(&next, l.dailyLimit, l.monthlyLimit, l.clock(), credits); err != nil {
		return err
	}
	if err := persist(l.path, next); err != nil {
		return err
	}
	l.state = next
	return nil
}

func reserve(current *state, dailyLimit, monthlyLimit int, now time.Time, credits int) error {
	day, month := now.UTC().Format("2006-01-02"), now.UTC().Format("2006-01")
	if current.Day != day {
		current.Day, current.DailyUsed = day, 0
	}
	if current.Month != month {
		current.Month, current.MonthlyUsed = month, 0
	}
	if dailyLimit > 0 && current.DailyUsed+credits > dailyLimit {
		return ErrLimitExceeded
	}
	if monthlyLimit > 0 && current.MonthlyUsed+credits > monthlyLimit {
		return ErrLimitExceeded
	}
	current.DailyUsed += credits
	current.MonthlyUsed += credits
	return nil
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
