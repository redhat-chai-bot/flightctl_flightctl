package kubeflowmodelregistrysource

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	meterName = "flightctl.catalogcollector"

	collectionCounterName    = "flightctl.catalogcollector.source.collections"
	collectionDurationName   = "flightctl.catalogcollector.source.collection.duration"
	lastSuccessTimestampName = "flightctl.catalogcollector.source.last_success.timestamp"
)

// sourceMetrics holds the per-source collection instruments.
//
// These are separate from the five service-owned pipeline instruments (which
// fire only after a snapshot is delivered downstream); these record every
// collection attempt including failures that occur before any snapshot exists.
type sourceMetrics struct {
	sourceID string

	collections      metric.Int64Counter
	duration         metric.Float64Histogram
	lastSuccessTS    int64 // Unix epoch seconds, updated after each success
	lastSuccessGauge metric.Int64ObservableGauge
}

func newMetrics(sourceID string, mp metric.MeterProvider) (*sourceMetrics, error) {
	meter := mp.Meter(meterName)

	collections, err := meter.Int64Counter(
		collectionCounterName,
		metric.WithDescription("Number of collection attempts by the source"),
		metric.WithUnit("{collection}"),
	)
	if err != nil {
		return nil, fmt.Errorf("creating %s counter: %w", collectionCounterName, err)
	}

	dur, err := meter.Float64Histogram(
		collectionDurationName,
		metric.WithDescription("Duration of each collection cycle"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, fmt.Errorf("creating %s histogram: %w", collectionDurationName, err)
	}

	m := &sourceMetrics{
		sourceID:    sourceID,
		collections: collections,
		duration:    dur,
	}

	gauge, err := meter.Int64ObservableGauge(
		lastSuccessTimestampName,
		metric.WithDescription("Unix timestamp (seconds) of the last successful collection"),
		metric.WithUnit("s"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			ts := m.lastSuccessTS
			if ts > 0 {
				o.Observe(ts, metric.WithAttributes(attribute.String("source.id", m.sourceID)))
			}
			return nil
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("creating %s gauge: %w", lastSuccessTimestampName, err)
	}
	m.lastSuccessGauge = gauge

	return m, nil
}

// recordSuccess records a successful collection cycle.
func (m *sourceMetrics) recordSuccess(elapsed time.Duration) {
	ctx := context.Background()
	attrs := metric.WithAttributes(
		attribute.String("source.id", m.sourceID),
		attribute.String("outcome", "success"),
	)
	m.collections.Add(ctx, 1, attrs)
	m.duration.Record(ctx, elapsed.Seconds(), attrs)
	m.lastSuccessTS = time.Now().Unix()
}

// recordFailure records a failed collection cycle.
func (m *sourceMetrics) recordFailure(elapsed time.Duration, err error) {
	ctx := context.Background()
	outcome := "failure"
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		outcome = "cancelled"
	}
	attrs := metric.WithAttributes(
		attribute.String("source.id", m.sourceID),
		attribute.String("outcome", outcome),
	)
	m.collections.Add(ctx, 1, attrs)
	m.duration.Record(ctx, elapsed.Seconds(), attrs)
}
