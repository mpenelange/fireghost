package budget_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
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

func TestFileLedgerUsesConfiguredMonthlyResetDayAfterReload(t *testing.T) {
	now := time.Date(2026, time.August, 18, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "budget.json")
	ledger, err := budget.NewFileWithResetDay(path, 0, 2, 3, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Reserve(context.Background(), 2); err != nil {
		t.Fatal(err)
	}

	now = time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	reloaded, err := budget.NewFileWithResetDay(path, 0, 2, 3, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := reloaded.Reserve(context.Background(), 1); !errors.Is(err, budget.ErrLimitExceeded) {
		t.Fatalf("reserve before reset day = %v, want ErrLimitExceeded", err)
	}
}

func TestFileLedgerResetDayChangePreservesUsageUntilNextBoundary(t *testing.T) {
	now := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "budget.json")
	ledger, err := budget.NewFileWithResetDay(path, 0, 2, 3, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Reserve(context.Background(), 2); err != nil {
		t.Fatal(err)
	}

	reloaded, err := budget.NewFileWithResetDay(path, 0, 2, 1, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := reloaded.Reserve(context.Background(), 1); !errors.Is(err, budget.ErrLimitExceeded) {
		t.Fatalf("reserve immediately after reset-day change = %v, want ErrLimitExceeded", err)
	}

	now = time.Date(2026, time.October, 1, 0, 0, 0, 0, time.UTC)
	if err := reloaded.Reserve(context.Background(), 1); err != nil {
		t.Fatalf("reserve at next configured boundary: %v", err)
	}
}

func TestFileLedgerMigratesLegacyMonthlyStateConservatively(t *testing.T) {
	tests := []struct {
		name     string
		resetDay int
	}{
		{name: "configured reset day", resetDay: 3},
		{name: "default reset day", resetDay: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "budget.json")
			legacy := []byte(`{"day":"2026-09-01","month":"2026-09","dailyUsed":0,"monthlyUsed":2}`)
			if err := os.WriteFile(path, legacy, 0o600); err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
			ledger, err := budget.NewFileWithResetDay(path, 0, 2, test.resetDay, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			contents, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var persisted struct {
				MonthlyResetDay int `json:"monthlyResetDay"`
			}
			if err := json.Unmarshal(contents, &persisted); err != nil {
				t.Fatal(err)
			}
			if persisted.MonthlyResetDay != test.resetDay {
				t.Fatalf("persisted monthly reset day = %d, want %d", persisted.MonthlyResetDay, test.resetDay)
			}
			if err := ledger.Reserve(context.Background(), 1); !errors.Is(err, budget.ErrLimitExceeded) {
				t.Fatalf("reserve after legacy reload = %v, want ErrLimitExceeded", err)
			}
		})
	}
}

func TestFileTokenBucketStartsFullForLegacyLedgerAndPreservesUsage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.json")
	legacy := []byte(`{"day":"2026-08-19","month":"2026-08","dailyUsed":2,"monthlyUsed":2,"monthlyResetDay":1}`)
	if err := os.WriteFile(path, legacy, 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 19, 12, 0, 0, 0, time.UTC)
	ledger, err := budget.NewFileWithOptions(path, budget.Options{
		DailyLimit: 0, MonthlyLimit: 5, MonthlyResetDay: 1,
		BurstCredits: 3, RefillCreditsPerDay: 1,
	}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Reserve(context.Background(), 3); err != nil {
		t.Fatalf("reserve full initial bucket from legacy ledger: %v", err)
	}
	if err := ledger.Reserve(context.Background(), 1); !errors.Is(err, budget.ErrLimitExceeded) {
		t.Fatalf("reserve after preserved monthly usage = %v, want ErrLimitExceeded", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("migrated ledger mode = %o, want 600", got)
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

func TestMemoryLedgerResetsMonthlyLimitOnConfiguredBillingDay(t *testing.T) {
	now := time.Date(2026, time.August, 18, 12, 0, 0, 0, time.UTC)
	ledger := budget.NewMemoryWithResetDay(0, 2, 3, func() time.Time { return now })
	if err := ledger.Reserve(context.Background(), 2); err != nil {
		t.Fatalf("reserve in August billing period: %v", err)
	}

	now = time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	if err := ledger.Reserve(context.Background(), 1); !errors.Is(err, budget.ErrLimitExceeded) {
		t.Fatalf("reserve before reset day = %v, want ErrLimitExceeded", err)
	}

	now = time.Date(2026, time.September, 3, 0, 0, 0, 0, time.UTC)
	if err := ledger.Reserve(context.Background(), 2); err != nil {
		t.Fatalf("reserve after reset day: %v", err)
	}
}

func TestMemoryTokenBucketAllowsImmediateBurst(t *testing.T) {
	now := time.Date(2026, time.August, 19, 12, 0, 0, 0, time.UTC)
	ledger, err := budget.NewMemoryWithOptions(budget.Options{
		MonthlyLimit:        100,
		MonthlyResetDay:     1,
		BurstCredits:        5,
		RefillCreditsPerDay: 2,
	}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Reserve(context.Background(), 5); err != nil {
		t.Fatalf("reserve full initial burst: %v", err)
	}
}

func TestMemoryTokenBucketRejectsWhenDepletedWithoutConsumingUsage(t *testing.T) {
	now := time.Date(2026, time.August, 19, 12, 0, 0, 0, time.UTC)
	ledger, err := budget.NewMemoryWithOptions(budget.Options{
		DailyLimit:          3,
		MonthlyLimit:        3,
		MonthlyResetDay:     1,
		BurstCredits:        2,
		RefillCreditsPerDay: 1,
	}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Reserve(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Reserve(context.Background(), 1); !errors.Is(err, budget.ErrLimitExceeded) {
		t.Fatalf("reserve against depleted bucket = %v, want ErrLimitExceeded", err)
	}

	// A new UTC day restores the legacy daily allowance, but not bucket tokens.
	// After a full day of refill, this succeeds only if the rejected reservation
	// did not consume monthly usage.
	now = now.Add(24 * time.Hour)
	if err := ledger.Reserve(context.Background(), 1); err != nil {
		t.Fatalf("reserve after refill: %v", err)
	}
}

func TestFileTokenBucketPersistsPartialRefillAcrossRestart(t *testing.T) {
	now := time.Date(2026, time.August, 19, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "budget.json")
	options := budget.Options{
		MonthlyLimit:        100,
		MonthlyResetDay:     1,
		BurstCredits:        4,
		RefillCreditsPerDay: 4,
	}
	ledger, err := budget.NewFileWithOptions(path, options, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Reserve(context.Background(), 4); err != nil {
		t.Fatal(err)
	}
	now = now.Add(6 * time.Hour)
	reloaded, err := budget.NewFileWithOptions(path, options, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := reloaded.Reserve(context.Background(), 1); err != nil {
		t.Fatalf("reserve one credit after quarter-day refill: %v", err)
	}
	if err := reloaded.Reserve(context.Background(), 1); !errors.Is(err, budget.ErrLimitExceeded) {
		t.Fatalf("reserve beyond persisted partial refill = %v, want ErrLimitExceeded", err)
	}
}

func TestFileReservePersistFailureDoesNotConsumeBurstOrMonthlyAllowance(t *testing.T) {
	now := time.Date(2026, time.August, 19, 12, 0, 0, 0, time.UTC)
	directory := t.TempDir()
	path := filepath.Join(directory, "budget.json")
	ledger, err := budget.NewFileWithOptions(path, budget.Options{
		MonthlyLimit: 3, MonthlyResetDay: 1, BurstCredits: 3, RefillCreditsPerDay: 1,
	}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Reserve(context.Background(), 1); err != nil {
		t.Fatal(err)
	}

	backup := filepath.Join(directory, "budget.backup.json")
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Reserve(context.Background(), 1); err == nil {
		t.Fatal("reserve with unwritable persistence target succeeded, want error")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}

	if err := ledger.Reserve(context.Background(), 2); err != nil {
		t.Fatalf("reserve remaining burst and monthly allowance after persist failure: %v", err)
	}
}

func TestMemoryTokenBucketRefillClampsAtCapacity(t *testing.T) {
	now := time.Date(2026, time.August, 19, 12, 0, 0, 0, time.UTC)
	ledger, err := budget.NewMemoryWithOptions(budget.Options{
		MonthlyLimit:        100,
		MonthlyResetDay:     1,
		BurstCredits:        3,
		RefillCreditsPerDay: 6,
	}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Reserve(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	now = now.Add(48 * time.Hour)
	if err := ledger.Reserve(context.Background(), 3); err != nil {
		t.Fatalf("reserve refilled capacity: %v", err)
	}
	if err := ledger.Reserve(context.Background(), 1); !errors.Is(err, budget.ErrLimitExceeded) {
		t.Fatalf("reserve above clamped capacity = %v, want ErrLimitExceeded", err)
	}
}

func TestFileTokenBucketClampsPersistedBalanceToNewCapacity(t *testing.T) {
	now := time.Date(2026, time.August, 19, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "budget.json")
	ledger, err := budget.NewFileWithOptions(path, budget.Options{
		MonthlyLimit: 100, MonthlyResetDay: 1, BurstCredits: 5, RefillCreditsPerDay: 1,
	}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Reserve(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	reloaded, err := budget.NewFileWithOptions(path, budget.Options{
		MonthlyLimit: 100, MonthlyResetDay: 1, BurstCredits: 2, RefillCreditsPerDay: 1,
	}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := reloaded.Reserve(context.Background(), 3); !errors.Is(err, budget.ErrLimitExceeded) {
		t.Fatalf("reserve above reduced capacity = %v, want ErrLimitExceeded", err)
	}
}

func TestMemoryTokenBucketResetsFullAtNewBillingCycle(t *testing.T) {
	now := time.Date(2026, time.September, 2, 23, 59, 59, 0, time.UTC)
	ledger, err := budget.NewMemoryWithOptions(budget.Options{
		MonthlyLimit:        100,
		MonthlyResetDay:     3,
		BurstCredits:        3,
		RefillCreditsPerDay: 1,
	}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Reserve(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if err := ledger.Reserve(context.Background(), 3); err != nil {
		t.Fatalf("reserve full burst at new billing cycle: %v", err)
	}
}

func TestOptionsRejectHalfConfiguredTokenBucket(t *testing.T) {
	for _, options := range []budget.Options{
		{MonthlyResetDay: 1, BurstCredits: 2},
		{MonthlyResetDay: 1, RefillCreditsPerDay: 2},
	} {
		if _, err := budget.NewMemoryWithOptions(options, time.Now); err == nil {
			t.Fatalf("NewMemoryWithOptions(%+v) succeeded, want error", options)
		}
		path := filepath.Join(t.TempDir(), "budget.json")
		if _, err := budget.NewFileWithOptions(path, options, time.Now); err == nil {
			t.Fatalf("NewFileWithOptions(%+v) succeeded, want error", options)
		}
	}
}

func TestOptionsRejectNegativeTokenBucketValues(t *testing.T) {
	tests := []budget.Options{
		{MonthlyResetDay: 1, BurstCredits: -1, RefillCreditsPerDay: 1},
		{MonthlyResetDay: 1, BurstCredits: 1, RefillCreditsPerDay: -1},
	}
	for _, options := range tests {
		if _, err := budget.NewMemoryWithOptions(options, time.Now); err == nil {
			t.Fatalf("NewMemoryWithOptions(%+v) succeeded, want error", options)
		}
	}
}

func TestOptionsRejectTokenBucketCapacityThatCannotBeRepresented(t *testing.T) {
	_, err := budget.NewMemoryWithOptions(budget.Options{
		MonthlyResetDay: 1, BurstCredits: math.MaxInt, RefillCreditsPerDay: 1,
	}, time.Now)
	if err == nil {
		t.Fatal("oversized burst capacity succeeded, want error")
	}
}

func TestMonthlyHardCapRejectsBeforeAvailableTokenBucketBalance(t *testing.T) {
	now := time.Date(2026, time.August, 19, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "budget.json")
	ledger, err := budget.NewFileWithOptions(path, budget.Options{
		MonthlyLimit: 1, MonthlyResetDay: 1, BurstCredits: 3, RefillCreditsPerDay: 3,
	}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Reserve(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Reserve(context.Background(), 1); !errors.Is(err, budget.ErrLimitExceeded) {
		t.Fatalf("reserve above monthly cap = %v, want ErrLimitExceeded", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("monthly rejection changed persisted usage or tokens:\nbefore %s\nafter  %s", before, after)
	}
}
