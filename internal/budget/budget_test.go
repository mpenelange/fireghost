package budget_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"web-retrieval/internal/budget"
)

func TestFileLedgerPersistsAtomicallyWithPrivateModeAndReloads(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "budget.json")
	ledger, err := budget.NewFile(path, 4, 5, func() time.Time { return now })
	if err != nil {
		t.Fatalf("new file ledger: %v", err)
	}
	if err := ledger.Reserve(context.Background(), 3); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat ledger: %v", err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Fatalf("mode = %o, want %o", got, want)
	}
	if matches, err := filepath.Glob(path + ".tmp-*"); err != nil || len(matches) != 0 {
		t.Fatalf("temporary files = %v, err = %v", matches, err)
	}

	reloaded, err := budget.NewFile(path, 4, 5, func() time.Time { return now })
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if err := reloaded.Reserve(context.Background(), 2); !errors.Is(err, budget.ErrLimitExceeded) {
		t.Fatalf("reserve after reload = %v, want ErrLimitExceeded", err)
	}
	if err := reloaded.Reserve(context.Background(), 1); err != nil {
		t.Fatalf("reserve remaining daily credit: %v", err)
	}
}

func TestFileLedgerReloadResetsExpiredPeriods(t *testing.T) {
	now := time.Date(2026, 8, 31, 23, 59, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "budget.json")
	ledger, err := budget.NewFile(path, 2, 2, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Reserve(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	reloaded, err := budget.NewFile(path, 2, 2, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := reloaded.Reserve(context.Background(), 2); err != nil {
		t.Fatalf("reserve after reload into new UTC periods: %v", err)
	}
}

func TestMemoryLedgerReservesAgainstDailyAndMonthlyLimitsAndResetsUTC(t *testing.T) {
	now := time.Date(2026, time.August, 31, 23, 59, 0, 0, time.UTC)
	ledger := budget.NewMemory(4, 6, func() time.Time { return now })

	if err := ledger.Reserve(context.Background(), 4); err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	if err := ledger.Reserve(context.Background(), 1); !errors.Is(err, budget.ErrLimitExceeded) {
		t.Fatalf("daily overage error = %v, want ErrLimitExceeded", err)
	}

	// In UTC this crosses both the day and month boundary.
	now = now.Add(2 * time.Hour)
	if err := ledger.Reserve(context.Background(), 2); err != nil {
		t.Fatalf("reserve after UTC reset: %v", err)
	}
}

func TestMemoryLedgerZeroLimitsAreUnlimitedAndReservationsAreConcurrent(t *testing.T) {
	ledger := budget.NewMemory(0, 0, func() time.Time { return time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC) })
	var wait sync.WaitGroup
	for range 100 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := ledger.Reserve(context.Background(), 10); err != nil {
				t.Errorf("reserve: %v", err)
			}
		}()
	}
	wait.Wait()
}

func TestMemoryLedgerMonthlyLimitSurvivesDailyReset(t *testing.T) {
	now := time.Date(2026, 8, 15, 23, 0, 0, 0, time.UTC)
	ledger := budget.NewMemory(3, 4, func() time.Time { return now })
	if err := ledger.Reserve(context.Background(), 3); err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	now = now.Add(2 * time.Hour)
	if err := ledger.Reserve(context.Background(), 2); !errors.Is(err, budget.ErrLimitExceeded) {
		t.Fatalf("monthly overage error = %v, want ErrLimitExceeded", err)
	}
}
