// Package payments contains the payment provider abstraction and the mock
// implementation used in place of a real gateway.
package payments

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	mathrand "math/rand/v2"
	"time"

	"github.com/Aryan-Jain06/seatsync/backend/internal/config"
)

// Result is the outcome of one charge attempt.
type Result struct {
	// Succeeded reports whether the provider accepted the charge.
	Succeeded bool
	// Reference is the provider's identifier for the attempt. It is recorded
	// for both outcomes so a failure can be traced as readily as a success.
	Reference string
	// DeclineReason explains a failure in terms suitable for an end user.
	DeclineReason string
}

// Provider charges a payment method.
//
// The interface exists so the confirm path is written against a contract
// rather than the mock, and so tests can drive specific outcomes without
// waiting on simulated latency.
type Provider interface {
	// Charge attempts a payment. A returned error means the provider could
	// not be reached at all, which is distinct from a Result reporting a
	// decline: the first is retryable, the second is an answer.
	Charge(ctx context.Context, amount int64, idempotencyKey string) (*Result, error)
}

// MockProvider stands in for a payment gateway. It sleeps for a configurable
// interval to imitate network latency, then succeeds or declines according to
// its mode.
type MockProvider struct {
	mode        config.PaymentMode
	successRate float64
	minLatency  time.Duration
	maxLatency  time.Duration
}

// NewMockProvider builds a MockProvider from configuration.
func NewMockProvider(cfg *config.Config) *MockProvider {
	return &MockProvider{
		mode:        cfg.PaymentMode,
		successRate: cfg.PaymentSuccessRate,
		minLatency:  cfg.PaymentMinLatency,
		maxLatency:  cfg.PaymentMaxLatency,
	}
}

// Charge simulates a payment attempt.
func (p *MockProvider) Charge(ctx context.Context, amount int64, idempotencyKey string) (*Result, error) {
	if amount <= 0 {
		return nil, fmt.Errorf("payments: refusing to charge a non-positive amount %d", amount)
	}

	if err := p.simulateLatency(ctx); err != nil {
		return nil, err
	}

	reference, err := newReference()
	if err != nil {
		return nil, err
	}

	if p.succeeds() {
		return &Result{Succeeded: true, Reference: reference}, nil
	}
	return &Result{
		Succeeded:     false,
		Reference:     reference,
		DeclineReason: "The card issuer declined the payment.",
	}, nil
}

// succeeds decides the outcome of an attempt.
func (p *MockProvider) succeeds() bool {
	switch p.mode {
	case config.PaymentModeAlwaysSuccess:
		return true
	case config.PaymentModeAlwaysFail:
		return false
	default:
		return mathrand.Float64() < p.successRate
	}
}

// simulateLatency waits for a random interval inside the configured window,
// abandoning the wait if the caller gives up first.
func (p *MockProvider) simulateLatency(ctx context.Context) error {
	if p.maxLatency <= 0 {
		return nil
	}

	delay := p.minLatency
	if spread := p.maxLatency - p.minLatency; spread > 0 {
		delay += mathrand.N(spread)
	}
	if delay <= 0 {
		return nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		// The client hung up or the request timed out. Reporting this as an
		// error rather than a decline keeps a cancelled request from being
		// recorded as a genuine payment failure.
		return fmt.Errorf("payments: charge abandoned: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

// newReference mints a provider-style reference for an attempt.
func newReference() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("payments: generate reference: %w", err)
	}
	return "mock_" + hex.EncodeToString(buf), nil
}
