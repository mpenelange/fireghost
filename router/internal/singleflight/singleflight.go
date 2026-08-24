package singleflight

import (
	"context"
	"sync"
)

const defaultMaxUnique = 64

// Result is the value or error produced by one shared operation.
type Result[T any] struct {
	Value T
	Err   error
}

// FlightGroup coalesces concurrent work with the same key.
type FlightGroup[T any] interface {
	Do(context.Context, string, func() (T, error)) (T, error)
}

type call[T any] struct {
	done         chan struct{}
	result       Result[T]
	participants int
}

// Group is a bounded set of currently executing keyed calls.
type Group[T any] struct {
	mu      sync.Mutex
	calls   map[string]*call[T]
	slots   chan struct{}
	changed chan struct{}
}

// New creates an empty flight group with a safe default unique-flight limit.
func New[T any]() *Group[T] {
	return NewWithLimit[T](defaultMaxUnique)
}

// NewWithLimit creates a flight group that executes at most maxUnique unique
// calls concurrently. maxUnique must be positive.
func NewWithLimit[T any](maxUnique int) *Group[T] {
	if maxUnique <= 0 {
		panic("singleflight: maxUnique must be positive")
	}
	return &Group[T]{
		calls:   make(map[string]*call[T]),
		slots:   make(chan struct{}, maxUnique),
		changed: make(chan struct{}),
	}
}

// Do executes fn or waits for the existing call with key.
func (g *Group[T]) Do(ctx context.Context, key string, fn func() (T, error)) (T, error) {
	for {
		g.mu.Lock()
		if existing, ok := g.calls[key]; ok {
			existing.participants++
			g.mu.Unlock()
			return g.wait(ctx, existing)
		}
		changed := g.changed
		g.mu.Unlock()

		select {
		case <-ctx.Done():
			var zero T
			return zero, ctx.Err()
		case g.slots <- struct{}{}:
			if err := ctx.Err(); err != nil {
				<-g.slots
				var zero T
				return zero, err
			}
			g.mu.Lock()
			if existing, ok := g.calls[key]; ok {
				existing.participants++
				g.mu.Unlock()
				<-g.slots
				return g.wait(ctx, existing)
			}
			c := &call[T]{done: make(chan struct{}), participants: 1}
			g.calls[key] = c
			close(g.changed)
			g.changed = make(chan struct{})
			g.mu.Unlock()

			go g.run(key, c, fn)
			return g.wait(ctx, c)
		case <-changed:
		}
	}
}

func (g *Group[T]) wait(ctx context.Context, c *call[T]) (T, error) {
	defer g.leave(c)
	select {
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	case <-c.done:
		return c.result.Value, c.result.Err
	}
}

func (g *Group[T]) run(key string, c *call[T], fn func() (T, error)) {
	c.result.Value, c.result.Err = fn()
	g.mu.Lock()
	delete(g.calls, key)
	close(c.done)
	g.mu.Unlock()
	<-g.slots
}

func (g *Group[T]) leave(c *call[T]) {
	g.mu.Lock()
	c.participants--
	g.mu.Unlock()
}

// Len reports the number of calls currently retained.
func (g *Group[T]) Len() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.calls)
}

// Participants reports callers currently executing or waiting across all
// retained flights. It supports diagnostics and deterministic concurrency tests.
func (g *Group[T]) Participants() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	total := 0
	for _, c := range g.calls {
		total += c.participants
	}
	return total
}
