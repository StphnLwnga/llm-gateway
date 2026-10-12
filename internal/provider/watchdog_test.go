package provider

import (
	"context"
	"errors"
	"testing"
	"time"
)

// script is a fake provider that sends each event after its delay, and stops
// at once when ctx ends, as a real adapter does.
type script struct {
	delays   []time.Duration
	progress bool
	ended    chan error // receives ctx.Err() when the fake stops early
}

func (s script) Chat(context.Context, Request) (Response, error) { return Response{}, nil }

func (s script) Stream(ctx context.Context, _ Request, fn func(Event) error) error {
	for i, d := range s.delays {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			if s.ended != nil {
				s.ended <- ctx.Err()
			}
			return TransportError(ctx, "fake", ctx.Err())
		}
		e := Event{Delta: "x"}
		if s.progress && i < len(s.delays)-1 {
			e = Event{Progress: true}
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	return nil
}

func watch(inner Provider) *Watchdog {
	return &Watchdog{Name: "fake", Inner: inner, FirstEvent: 100 * time.Millisecond, Stall: 100 * time.Millisecond}
}

func wantTimeout(t *testing.T, err error, cause error) {
	t.Helper()
	var pe *Error
	if !errors.As(err, &pe) || pe.Kind != KindTimeout || pe.Provider != "fake" {
		t.Fatalf("err = %v, want a provider error of kind timeout", err)
	}
	if !errors.Is(err, cause) {
		t.Errorf("err = %v, want cause %v", err, cause)
	}
}

func TestWatchdogPassesAFastStream(t *testing.T) {
	t.Parallel()
	n := 0
	err := watch(script{delays: []time.Duration{10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond}}).
		Stream(t.Context(), Request{}, func(Event) error { n++; return nil })
	if err != nil || n != 3 {
		t.Errorf("err = %v, events = %d; want nil and 3", err, n)
	}
}

func TestWatchdogFirstEventDeadline(t *testing.T) {
	t.Parallel()
	ended := make(chan error, 1)
	began := time.Now()
	err := watch(script{delays: []time.Duration{time.Second}, ended: ended}).Stream(t.Context(), Request{}, func(Event) error { return nil })
	wantTimeout(t, err, errNoFirstEvent)
	if d := time.Since(began); d > 500*time.Millisecond {
		t.Errorf("returned after %v, want about 100ms", d)
	}
	if got := <-ended; !errors.Is(got, context.Canceled) {
		t.Errorf("the inner provider saw %v, want its ctx cancelled", got)
	}
}

func TestWatchdogStallDeadline(t *testing.T) {
	t.Parallel()
	n := 0
	err := watch(script{delays: []time.Duration{10 * time.Millisecond, 10 * time.Millisecond, time.Second}}).
		Stream(t.Context(), Request{}, func(Event) error { n++; return nil })
	wantTimeout(t, err, errStalled)
	if n != 2 {
		t.Errorf("events before the stall = %d, want 2", n)
	}
}

func TestProgressEventsKeepTheStreamAlive(t *testing.T) {
	t.Parallel()
	delays := make([]time.Duration, 8) // 8 x 60ms = 480ms, far past either 100ms deadline
	for i := range delays {
		delays[i] = 60 * time.Millisecond
	}
	content := 0
	err := watch(script{delays: delays, progress: true}).Stream(t.Context(), Request{}, func(e Event) error {
		if e.Delta != "" {
			content++
		}
		return nil
	})
	if err != nil || content != 1 {
		t.Errorf("err = %v, content events = %d; want nil and 1 after 7 progress events", err, content)
	}
}

func TestTimeInTheCallbackIsNotUpstreamSilence(t *testing.T) {
	t.Parallel()
	err := watch(script{delays: []time.Duration{10 * time.Millisecond, 10 * time.Millisecond}}).
		Stream(t.Context(), Request{}, func(Event) error { time.Sleep(250 * time.Millisecond); return nil })
	if err != nil {
		t.Errorf("a slow callback tripped the watchdog: %v", err)
	}
}

func TestAClientCancelIsNotATimeout(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	err := watch(script{delays: []time.Duration{time.Second}}).Stream(ctx, Request{}, func(Event) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	var pe *Error
	if errors.As(err, &pe) {
		t.Errorf("a client cancel came back as a provider error: %v", err)
	}
}

func TestACallbackErrorIsReturnedUnchanged(t *testing.T) {
	t.Parallel()
	stop := errors.New("client gone")
	err := watch(script{delays: []time.Duration{10 * time.Millisecond, 10 * time.Millisecond}}).
		Stream(t.Context(), Request{}, func(Event) error { return stop })
	if !errors.Is(err, stop) {
		t.Errorf("err = %v, want the callback's error", err)
	}
}
