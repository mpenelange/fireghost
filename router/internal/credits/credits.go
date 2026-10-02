// Package credits guards Firecrawl Cloud spending with the live account balance.
package credits

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	budgetpkg "web-retrieval/internal/budget"
	"web-retrieval/internal/upstream"
)

// ErrBelowFloor indicates that the account balance is at or below the floor.
// It wraps budget.ErrLimitExceeded so existing budget handling applies.
var ErrBelowFloor = fmt.Errorf("%w: account balance at or below floor", budgetpkg.ErrLimitExceeded)

// ErrUnavailable indicates that the account balance could not be read. The
// guard fails closed, so callers must treat this as a denial.
var ErrUnavailable = errors.New("cloud credit balance unavailable")

const creditUsagePath = "/v2/team/credit-usage"

// Floor reads the live balance before every billable cloud request. The
// balance is never cached: variable-cost jobs bill after they start, so each
// check observes whatever the previous request actually consumed.
type Floor struct {
	client *upstream.Client
	floor  int
}

// NewFloor creates a guard that denies cloud work once remaining credits are
// at or below floor.
func NewFloor(client *upstream.Client, floor int) *Floor {
	return &Floor{client: client, floor: floor}
}

// Check returns nil when the account has more than floor credits remaining.
func (f *Floor) Check(ctx context.Context) error {
	remaining, err := f.Remaining(ctx)
	if err != nil {
		return err
	}
	if remaining <= f.floor {
		return ErrBelowFloor
	}
	return nil
}

// Remaining returns the account's remaining credits.
func (f *Floor) Remaining(ctx context.Context) (int, error) {
	response, err := f.client.Send(ctx, upstream.Request{Method: http.MethodGet, PathAndQuery: creditUsagePath})
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("%w: status %d", ErrUnavailable, response.StatusCode)
	}
	var result struct {
		Success bool `json:"success"`
		Data    *struct {
			RemainingCredits *float64 `json:"remainingCredits"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &result) != nil || !result.Success || result.Data == nil || result.Data.RemainingCredits == nil {
		return 0, fmt.Errorf("%w: malformed credit usage response", ErrUnavailable)
	}
	return int(*result.Data.RemainingCredits), nil
}

// Ledger applies the floor before an inner ledger's reservation, so a denied
// floor never consumes local budget.
type Ledger struct {
	Floor *Floor
	Inner budgetpkg.Ledger
}

// Reserve checks the floor, then reserves credits in the inner ledger.
func (l Ledger) Reserve(ctx context.Context, credits int) error {
	if l.Floor != nil {
		if err := l.Floor.Check(ctx); err != nil {
			return err
		}
	}
	if l.Inner == nil {
		return nil
	}
	return l.Inner.Reserve(ctx, credits)
}
