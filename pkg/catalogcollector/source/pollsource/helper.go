// Package pollsource provides a reusable polling loop with bounded exponential
// backoff and jitter for pull-based catalog collector sources.
//
// Any pull source supplies a collect callback and receives the poll loop,
// backoff, cancellation, and collection-metrics behavior without touching the
// shared pipeline. Process readiness is not managed here; it is owned by the
// healthcheck extension.
package pollsource

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/flightctl/flightctl/internal/util"
	catalogcollector "github.com/flightctl/flightctl/pkg/catalogcollector"
	"github.com/sirupsen/logrus"
)

// CollectFunc is the signature of a complete-snapshot collection function.
// On error the source must not have emitted any partial state to the consumer;
// the helper discards the result and applies backoff.
type CollectFunc func(ctx context.Context) (*catalogcollector.CatalogSnapshot, error)

// NowFunc returns the current time. Used only for the last-success gauge.
// Inject a fake in tests.
type NowFunc func() time.Time

// JitterFunc returns a non-negative random duration in [0, max). Inject a
// deterministic function in tests.
type JitterFunc func(max time.Duration) time.Duration

// BackoffConfig holds parameters for bounded exponential backoff with jitter.
type BackoffConfig struct {
	// InitialInterval is the first wait duration after a failure.
	InitialInterval util.Duration `json:"initialInterval,omitempty"`

	// MaxInterval caps the computed backoff duration.
	MaxInterval util.Duration `json:"maxInterval,omitempty"`

	// Multiplier is applied to the current interval after each failure (≥ 1).
	Multiplier float64 `json:"multiplier,omitempty"`

	// RandomizationFactor applies symmetric jitter ±(factor × current) to the
	// computed duration. Must be in [0, 1].
	RandomizationFactor float64 `json:"randomizationFactor,omitempty"`
}

// Validate returns an error if the BackoffConfig is not usable.
func (c *BackoffConfig) Validate() error {
	if time.Duration(c.InitialInterval) <= 0 {
		return fmt.Errorf("backoff.initialInterval must be positive, got %s", time.Duration(c.InitialInterval))
	}
	if time.Duration(c.MaxInterval) <= 0 {
		return fmt.Errorf("backoff.maxInterval must be positive, got %s", time.Duration(c.MaxInterval))
	}
	if c.InitialInterval > c.MaxInterval {
		return fmt.Errorf("backoff.initialInterval (%s) must not exceed backoff.maxInterval (%s)",
			time.Duration(c.InitialInterval), time.Duration(c.MaxInterval))
	}
	if c.Multiplier < 1.0 {
		return fmt.Errorf("backoff.multiplier must be >= 1.0, got %g", c.Multiplier)
	}
	if c.RandomizationFactor < 0 || c.RandomizationFactor > 1 {
		return fmt.Errorf("backoff.randomizationFactor must be in [0, 1], got %g", c.RandomizationFactor)
	}
	return nil
}

// DefaultBackoffConfig returns a BackoffConfig with safe default values.
func DefaultBackoffConfig() BackoffConfig {
	return BackoffConfig{
		InitialInterval:     util.Duration(1 * time.Second),
		MaxInterval:         util.Duration(5 * time.Minute),
		Multiplier:          2.0,
		RandomizationFactor: 0.5,
	}
}

// Helper drives a periodic complete-snapshot polling loop.
//
// The first collection runs immediately. After a fully successful cycle
// (collect + downstream consume both succeed) the helper waits pollInterval.
// On any failure it waits the current backoff duration and advances the
// exponential multiplier. Backoff resets only after a fully successful cycle.
//
// Collections never overlap. Context cancellation interrupts any in-flight
// HTTP call (via the passed ctx) and any inter-cycle wait.
type Helper struct {
	id           string
	pollInterval time.Duration
	backoff      BackoffConfig
	log          *logrus.Entry
	now          NowFunc
	jitter       JitterFunc

	// OnSuccess is called after each fully successful cycle (collect + consume).
	// Use it to record metrics. May be nil.
	OnSuccess func(elapsed time.Duration)
	// OnFailure is called after each failed cycle (collect or consume error).
	// Use it to record metrics. May be nil.
	OnFailure func(elapsed time.Duration, err error)
}

// NewHelper constructs a polling helper.
//
// now and jitter may be nil; production defaults (time.Now and a rand-based
// jitter) are used in that case.
func NewHelper(
	id string,
	pollInterval time.Duration,
	backoff BackoffConfig,
	log *logrus.Entry,
	now NowFunc,
	jitter JitterFunc,
) *Helper {
	if now == nil {
		now = time.Now
	}
	if jitter == nil {
		jitter = func(max time.Duration) time.Duration {
			if max <= 0 {
				return 0
			}
			return time.Duration(rand.Int64N(int64(max)))
		}
	}
	return &Helper{
		id:           id,
		pollInterval: pollInterval,
		backoff:      backoff,
		log:          log,
		now:          now,
		jitter:       jitter,
	}
}

// Run executes the polling loop until ctx is cancelled.
//
// On each iteration, collect is called. If both collect and next.Consume
// succeed, the helper waits pollInterval before the next attempt and resets
// backoff. On any failure it waits the current backoff duration (with jitter)
// and multiplies the backoff for the next failure, up to MaxInterval.
//
// Run returns ctx.Err() when the context is cancelled.
func (h *Helper) Run(
	ctx context.Context,
	collect CollectFunc,
	next catalogcollector.Consumer,
) error {
	current := time.Duration(h.backoff.InitialInterval)

	for {
		start := h.now()

		snapshot, collectErr := collect(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if collectErr != nil {
			elapsed := h.now().Sub(start)
			h.log.WithError(collectErr).Error("collection failed; applying backoff")
			if h.OnFailure != nil {
				h.OnFailure(elapsed, collectErr)
			}
			sleep := h.withJitter(current)
			current = h.advance(current)
			if !h.wait(ctx, sleep) {
				return ctx.Err()
			}
			continue
		}

		consumeErr := next.Consume(ctx, snapshot)
		if ctx.Err() != nil {
			return ctx.Err()
		}

		elapsed := h.now().Sub(start)

		if consumeErr != nil {
			h.log.WithError(consumeErr).Error("downstream consume failed; applying backoff")
			if h.OnFailure != nil {
				h.OnFailure(elapsed, consumeErr)
			}
			sleep := h.withJitter(current)
			current = h.advance(current)
			if !h.wait(ctx, sleep) {
				return ctx.Err()
			}
			continue
		}

		// Fully successful cycle: reset backoff and wait the normal interval.
		if h.OnSuccess != nil {
			h.OnSuccess(elapsed)
		}
		current = time.Duration(h.backoff.InitialInterval)
		if !h.wait(ctx, h.pollInterval) {
			return ctx.Err()
		}
	}
}

// withJitter applies symmetric jitter to d using RandomizationFactor.
// The result is in [d*(1-rf), d*(1+rf)] and is always positive.
func (h *Helper) withJitter(d time.Duration) time.Duration {
	rf := h.backoff.RandomizationFactor
	if rf == 0 {
		return d
	}
	delta := time.Duration(float64(d) * rf)
	// Jitter in [0, 2*delta), mapped to [-delta, +delta).
	jitter := h.jitter(2*delta+1) - delta
	if result := d + jitter; result > 0 {
		return result
	}
	return d
}

// advance multiplies current by Multiplier and caps at MaxInterval.
func (h *Helper) advance(current time.Duration) time.Duration {
	next := time.Duration(float64(current) * h.backoff.Multiplier)
	if next > time.Duration(h.backoff.MaxInterval) {
		return time.Duration(h.backoff.MaxInterval)
	}
	return next
}

// wait blocks for d or until ctx is cancelled. Returns false if cancelled.
func (h *Helper) wait(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return false
		default:
			return true
		}
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
