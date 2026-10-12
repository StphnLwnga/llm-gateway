package provider

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

// Watchdog wraps a provider with liveness deadlines on Stream. The first event
// must arrive within FirstEvent, and each later event within Stall of the one
// before. The HTTP client's header timeout cannot do this: some providers send
// headers before the first token (ADR 0015).
type Watchdog struct {
	Name       string
	Inner      Provider
	FirstEvent time.Duration
	Stall      time.Duration
}

var (
	errNoFirstEvent = errors.New("no event before the first-token deadline")
	errStalled      = errors.New("no event within the stall deadline")
)

// Chat calls the inner provider unchanged.
func (w *Watchdog) Chat(ctx context.Context, req Request) (Response, error) {
	return w.Inner.Chat(ctx, req)
}

// Stream calls the inner provider and cancels it when a deadline passes. A
// passed deadline returns an *Error of kind timeout; a caller whose own ctx
// ended still gets an error that wraps ctx.Err().
func (w *Watchdog) Stream(ctx context.Context, req Request, fn func(Event) error) error {
	inner, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var seen atomic.Bool
	timer := time.AfterFunc(w.FirstEvent, func() {
		if seen.Load() {
			cancel(errStalled)
		} else {
			cancel(errNoFirstEvent)
		}
	})
	defer timer.Stop()

	err := w.Inner.Stream(inner, req, func(e Event) error {
		// Time spent in fn, such as writing to a slow client, is not upstream silence.
		timer.Stop()
		seen.Store(true)
		ferr := fn(e)
		timer.Reset(w.Stall)
		return ferr
	})
	if err == nil || ctx.Err() != nil {
		return err
	}
	if cause := context.Cause(inner); errors.Is(cause, errNoFirstEvent) || errors.Is(cause, errStalled) {
		return &Error{Provider: w.Name, Kind: KindTimeout, Err: fmt.Errorf("%w (%s)", cause, w.limit(cause))}
	}
	return err
}

func (w *Watchdog) limit(cause error) time.Duration {
	if errors.Is(cause, errNoFirstEvent) {
		return w.FirstEvent
	}
	return w.Stall
}
