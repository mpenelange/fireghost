package singleflight_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"web-retrieval/internal/singleflight"
)

func TestUniqueFlightsAreBoundedAndCapacityIsReusable(t *testing.T) {
	const limit = 2
	group := singleflight.NewWithLimit[string](limit)
	started := make(chan string, 3)
	release := make(chan struct{})
	results := make(chan error, 3)
	var running atomic.Int32
	var maximum atomic.Int32

	start := func(key string) {
		go func() {
			_, err := group.Do(context.Background(), key, func() (string, error) {
				current := running.Add(1)
				for old := maximum.Load(); current > old && !maximum.CompareAndSwap(old, current); old = maximum.Load() {
				}
				started <- key
				<-release
				running.Add(-1)
				return key, nil
			})
			results <- err
		}()
	}

	start("one")
	start("two")
	for range limit {
		<-started
	}
	start("three")
	select {
	case key := <-started:
		t.Fatalf("flight %q started above unique limit", key)
	case <-time.After(50 * time.Millisecond):
	}
	if got := group.Len(); got != limit {
		t.Fatalf("retained flights = %d, want %d", got, limit)
	}

	close(release)
	for range 3 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if got := maximum.Load(); got != limit {
		t.Fatalf("maximum concurrent fn calls = %d, want %d", got, limit)
	}
	if got := group.Len(); got != 0 {
		t.Fatalf("retained completed flights = %d, want 0", got)
	}
}

func TestCanceledCapacityWaiterReturnsWithoutCreatingFlight(t *testing.T) {
	group := singleflight.NewWithLimit[string](1)
	started := make(chan struct{})
	release := make(chan struct{})
	leaderDone := make(chan error, 1)
	go func() {
		_, err := group.Do(context.Background(), "occupied", func() (string, error) {
			close(started)
			<-release
			return "done", nil
		})
		leaderDone <- err
	}()
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	waiterDone := make(chan error, 1)
	var called atomic.Bool
	go func() {
		_, err := group.Do(ctx, "blocked", func() (string, error) {
			called.Store(true)
			return "must not run", nil
		})
		waiterDone <- err
	}()
	cancel()
	select {
	case err := <-waiterDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("capacity waiter error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled capacity waiter did not exit")
	}
	if called.Load() {
		t.Fatal("canceled capacity waiter's fn ran")
	}
	if got := group.Len(); got != 1 {
		t.Fatalf("retained flights = %d, want only the occupied flight", got)
	}
	if got := group.Participants(); got != 1 {
		t.Fatalf("participants = %d, want only the occupied flight's caller", got)
	}

	close(release)
	if err := <-leaderDone; err != nil {
		t.Fatal(err)
	}
}

func TestSameKeyWaitersCoalesceWhileUniqueCapacityIsFull(t *testing.T) {
	group := singleflight.NewWithLimit[string](1)
	started := make(chan struct{})
	release := make(chan struct{})
	results := make(chan string, 2)
	var calls atomic.Int32
	fn := func() (string, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return "shared", nil
	}
	go func() {
		value, _ := group.Do(context.Background(), "same", fn)
		results <- value
	}()
	<-started
	go func() {
		value, _ := group.Do(context.Background(), "same", fn)
		results <- value
	}()

	deadline := time.Now().Add(time.Second)
	for group.Participants() != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := group.Participants(); got != 2 {
		t.Fatalf("participants = %d, want 2 same-key callers", got)
	}
	close(release)
	for range 2 {
		if got := <-results; got != "shared" {
			t.Fatalf("result = %q, want shared", got)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("fn calls = %d, want 1", got)
	}
}

func TestCapacityWaitersRecheckForAFlightCreatedWhileWaiting(t *testing.T) {
	group := singleflight.NewWithLimit[string](1)
	occupiedStarted := make(chan struct{})
	occupiedRelease := make(chan struct{})
	go func() {
		_, _ = group.Do(context.Background(), "occupied", func() (string, error) {
			close(occupiedStarted)
			<-occupiedRelease
			return "occupied", nil
		})
	}()
	<-occupiedStarted

	sharedStarted := make(chan struct{})
	sharedRelease := make(chan struct{})
	results := make(chan string, 2)
	var calls atomic.Int32
	var ready sync.WaitGroup
	ready.Add(2)
	for range 2 {
		go func() {
			ready.Done()
			value, err := group.Do(context.Background(), "shared", func() (string, error) {
				if calls.Add(1) == 1 {
					close(sharedStarted)
				}
				<-sharedRelease
				return "shared", nil
			})
			if err != nil {
				results <- fmt.Sprintf("error: %v", err)
				return
			}
			results <- value
		}()
	}
	ready.Wait()
	close(occupiedRelease)
	select {
	case <-sharedStarted:
	case <-time.After(time.Second):
		t.Fatal("shared flight did not start after capacity became available")
	}
	deadline := time.Now().Add(time.Second)
	for group.Participants() != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := group.Participants(); got != 2 {
		t.Fatalf("participants = %d, want both capacity waiters on one flight", got)
	}
	close(sharedRelease)
	for range 2 {
		if got := <-results; got != "shared" {
			t.Fatalf("result = %q, want shared", got)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("fn calls = %d, want 1 after map recheck", got)
	}
}

func TestCanceledWaiterDoesNotCancelLeaderAndCompletedFlightIsRemoved(t *testing.T) {
	group := singleflight.New[string]()
	started := make(chan struct{})
	release := make(chan struct{})
	leaderResult := make(chan error, 1)
	go func() {
		value, err := group.Do(context.Background(), "key", func() (string, error) {
			close(started)
			<-release
			return "complete", nil
		})
		if err == nil && value != "complete" {
			err = errors.New("leader received wrong value")
		}
		leaderResult <- err
	}()
	<-started

	waiterContext, cancel := context.WithCancel(context.Background())
	waiterResult := make(chan error, 1)
	go func() {
		_, err := group.Do(waiterContext, "key", func() (string, error) {
			return "must not run", nil
		})
		waiterResult <- err
	}()
	cancel()
	select {
	case err := <-waiterResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled waiter did not exit")
	}

	close(release)
	if err := <-leaderResult; err != nil {
		t.Fatalf("leader failed after waiter cancellation: %v", err)
	}
	if got := group.Len(); got != 0 {
		t.Fatalf("retained completed flights = %d, want 0", got)
	}
}
