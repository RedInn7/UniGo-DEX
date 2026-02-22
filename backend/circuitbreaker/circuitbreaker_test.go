package circuitbreaker

import (
	"errors"
	"testing"
	"time"
)

var errFake = errors.New("fake error")

func TestClosedToOpen(t *testing.T) {
	cfg := Config{MaxFailures: 3, Timeout: 100 * time.Millisecond, HalfOpenMaxRequests: 1}
	cb := New(cfg)

	// 3 次失败后应进入 Open
	for i := 0; i < 3; i++ {
		cb.Execute(func() (interface{}, error) { return nil, errFake })
	}
	if cb.State() != StateOpen {
		t.Fatalf("expected Open, got %s", cb.State())
	}

	// Open 状态下应快速失败
	_, err := cb.Execute(func() (interface{}, error) { return "ok", nil })
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected ErrCircuitOpen, got %v", err)
	}
}

func TestHalfOpenRecovery(t *testing.T) {
	cfg := Config{MaxFailures: 2, Timeout: 50 * time.Millisecond, HalfOpenMaxRequests: 1}
	cb := New(cfg)

	// 触发 Open
	for i := 0; i < 2; i++ {
		cb.Execute(func() (interface{}, error) { return nil, errFake })
	}
	if cb.State() != StateOpen {
		t.Fatalf("expected Open, got %s", cb.State())
	}

	// 等待冷却
	time.Sleep(60 * time.Millisecond)

	// 探测成功 → 回到 Closed
	result, err := cb.Execute(func() (interface{}, error) { return "recovered", nil })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "recovered" {
		t.Fatalf("unexpected result: %v", result)
	}
	if cb.State() != StateClosed {
		t.Fatalf("expected Closed after recovery, got %s", cb.State())
	}
}

func TestHalfOpenFailure(t *testing.T) {
	cfg := Config{MaxFailures: 1, Timeout: 50 * time.Millisecond, HalfOpenMaxRequests: 1}
	cb := New(cfg)

	// 触发 Open
	cb.Execute(func() (interface{}, error) { return nil, errFake })
	time.Sleep(60 * time.Millisecond)

	// 探测失败 → 回到 Open
	cb.Execute(func() (interface{}, error) { return nil, errFake })
	if cb.State() != StateOpen {
		t.Fatalf("expected Open after probe failure, got %s", cb.State())
	}
}

func TestStateChangeCallback(t *testing.T) {
	transitions := make([]string, 0)
	cfg := Config{
		MaxFailures:         2,
		Timeout:             50 * time.Millisecond,
		HalfOpenMaxRequests: 1,
		OnStateChange: func(from, to State) {
			transitions = append(transitions, from.String()+"->"+to.String())
		},
	}
	cb := New(cfg)

	// Closed -> Open
	for i := 0; i < 2; i++ {
		cb.Execute(func() (interface{}, error) { return nil, errFake })
	}
	// Open -> HalfOpen -> Closed
	time.Sleep(60 * time.Millisecond)
	cb.Execute(func() (interface{}, error) { return "ok", nil })

	expected := []string{"closed->open", "open->half-open", "half-open->closed"}
	if len(transitions) != len(expected) {
		t.Fatalf("expected %d transitions, got %d: %v", len(expected), len(transitions), transitions)
	}
	for i, e := range expected {
		if transitions[i] != e {
			t.Errorf("transition %d: expected %s, got %s", i, e, transitions[i])
		}
	}
}
