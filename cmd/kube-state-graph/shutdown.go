package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"
)

const (
	// minShutdownGrace floors the grace period for very short build timeouts.
	minShutdownGrace = 10 * time.Second
	// maxFlushReserve caps the slice of the grace period kept back from the
	// HTTP drain for the telemetry flush.
	maxFlushReserve = 5 * time.Second
)

// shutdownBudget derives the one shutdown grace period from the server's
// WriteTimeout (so it covers the slowest legitimate in-flight request, floored
// at minShutdownGrace) and the flush reserve the HTTP drain leaves before that
// deadline: min(grace/4, maxFlushReserve).
//
// The drain is therefore grace-reserve. With WriteTimeout = BuildTimeout+5s
// and reserve <= 5s that is never shorter than --build-timeout, the deadline
// that already bounds every graph request.
func shutdownBudget(writeTimeout time.Duration) (grace, reserve time.Duration) {
	grace = max(writeTimeout, minShutdownGrace)
	reserve = min(grace/4, maxFlushReserve)
	return grace, reserve
}

// shutdown drains the HTTP server and then flushes telemetry, both inside ONE
// grace deadline: the telemetry exporter never extends the grace period.
//
// The drain runs under a child context whose deadline is reserve earlier, so a
// drain that times out still leaves the flush reserve of live time — the
// abnormal shutdown, whose spans and log records (including the drain failure
// logged here) matter most. Handing the flush the drain's own context would
// give it an expired one in exactly that case, and the OTel SDK Shutdowns
// return immediately on an expired context, exporting nothing.
//
// The flush runs even when the drain fails; both errors are returned joined.
// The flush failure is written to stderr directly, bypassing the slog OTLP
// bridge whose provider is the thing being torn down.
func shutdown(
	parent context.Context,
	grace, reserve time.Duration,
	drain, flush func(context.Context) error,
	logger *slog.Logger,
	stderr io.Writer,
) error {
	deadline := time.Now().Add(grace)
	graceCtx, cancel := context.WithDeadline(parent, deadline)
	defer cancel()
	drainCtx, drainCancel := context.WithDeadline(graceCtx, deadline.Add(-reserve))
	defer drainCancel()

	var errs []error
	if err := drain(drainCtx); err != nil {
		logger.Error("http server shutdown failed", "err", err)
		errs = append(errs, fmt.Errorf("shutdown: %w", err))
	}
	if err := flush(graceCtx); err != nil {
		_, _ = fmt.Fprintf(stderr, "otlp shutdown timed out: %v\n", err)
		errs = append(errs, fmt.Errorf("otlp shutdown: %w", err))
	}
	return errors.Join(errs...)
}
