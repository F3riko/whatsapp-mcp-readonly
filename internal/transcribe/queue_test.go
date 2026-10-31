package transcribe

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type mockTranscriber struct {
	concurrency int32
	maxSeen     int32
	delay       time.Duration
}

func (m *mockTranscriber) Transcribe(ctx context.Context, audio []byte, filename string) (string, error) {
	c := atomic.AddInt32(&m.concurrency, 1)
	defer atomic.AddInt32(&m.concurrency, -1)

	// Update max concurrency observed
	for {
		curr := atomic.LoadInt32(&m.maxSeen)
		if c <= curr || atomic.CompareAndSwapInt32(&m.maxSeen, curr, c) {
			break
		}
	}

	time.Sleep(m.delay)
	return "transcribed: " + string(audio), nil
}

func (m *mockTranscriber) Name() string { return "mock" }

func TestQueueSingleInferenceWorker(t *testing.T) {
	mock := &mockTranscriber{delay: 20 * time.Millisecond}
	q := NewQueue(mock, QueueConfig{
		Capacity:      10,
		MaxAudioBytes: 1024 * 1024,
	})
	defer q.Close()

	ctx := context.Background()
	jobs := make([]*Job, 5)
	for i := 0; i < 5; i++ {
		jobs[i] = &Job{
			MessageID: string(rune('A' + i)),
			Audio:     []byte("test"),
			Done:      make(chan struct{}),
		}
		if err := q.Submit(ctx, jobs[i]); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}

	for _, j := range jobs {
		select {
		case <-j.Done:
			if j.Err != nil {
				t.Errorf("job failed: %v", j.Err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for jobs")
		}
	}

	if atomic.LoadInt32(&mock.maxSeen) != 1 {
		t.Errorf("expected max concurrency 1, got %d", mock.maxSeen)
	}
}

func TestQueueCapacityAndDropPolicy(t *testing.T) {
	// A transcriber that hangs until test releases it
	mock := &mockTranscriber{delay: 50 * time.Millisecond}
	q := NewQueue(mock, QueueConfig{
		Capacity:      2,
		MaxAudioBytes: 1024,
	})
	defer q.Close()

	ctx := context.Background()
	// Submit 1 (active in worker), plus 2 (filling capacity 2)
	for i := 0; i < 3; i++ {
		job := &Job{
			MessageID: string(rune('1' + i)),
			Audio:     []byte("hi"),
			Done:      make(chan struct{}),
		}
		_ = q.Submit(ctx, job)
	}

	// 4th submit should be dropped due to backpressure
	droppedJob := &Job{
		MessageID: "4",
		Audio:     []byte("hi"),
		Done:      make(chan struct{}),
	}
	err := q.Submit(ctx, droppedJob)
	if err != ErrQueueFull {
		t.Fatalf("expected ErrQueueFull on overload, got: %v", err)
	}

	status := q.Status()
	if status.Dropped < 1 {
		t.Errorf("expected Dropped >= 1 in status, got: %d", status.Dropped)
	}
}

func TestQueueRejectsOversizedAudio(t *testing.T) {
	mock := &mockTranscriber{}
	q := NewQueue(mock, QueueConfig{
		Capacity:      5,
		MaxAudioBytes: 100, // 100 bytes max
	})
	defer q.Close()

	ctx := context.Background()
	largeAudio := make([]byte, 200)
	job := &Job{
		MessageID: "large",
		Audio:     largeAudio,
		Done:      make(chan struct{}),
	}
	err := q.Submit(ctx, job)
	if err != ErrAudioTooLarge {
		t.Fatalf("expected ErrAudioTooLarge, got: %v", err)
	}
}

type probeBackend struct {
	calls   atomic.Int64
	started chan struct{}
	release chan struct{}
}

func (p *probeBackend) Name() string { return "probe" }
func (p *probeBackend) Transcribe(ctx context.Context, b []byte, n string) (string, error) {
	p.calls.Add(1)
	if p.started != nil {
		select {
		case <-p.started:
		default:
			close(p.started)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-p.release:
			return "", errors.New("released")
		}
	}
	return "ok", nil
}

func TestControllerCrossChatIdentity(t *testing.T) {
	b := &probeBackend{}
	q := NewQueue(b, QueueConfig{Capacity: 2})
	defer q.Close()
	a := &Job{ChatJID: "chat-a", MessageID: "same", Audio: []byte("x"), Done: make(chan struct{})}
	z := &Job{ChatJID: "chat-b", MessageID: "same", Audio: []byte("y"), Done: make(chan struct{})}
	if err := q.Submit(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	<-a.Done
	if err := q.Submit(context.Background(), z); err != nil {
		t.Fatal(err)
	}
	<-z.Done
	if n := b.calls.Load(); n != 2 {
		t.Fatalf("distinct chats with same message ID require 2 jobs; processed %d", n)
	}
}

func TestControllerCloseCancelsActive(t *testing.T) {
	b := &probeBackend{started: make(chan struct{}), release: make(chan struct{})}
	q := NewQueue(b, QueueConfig{Capacity: 1})
	j := &Job{Audio: []byte("x"), Done: make(chan struct{})}
	if err := q.Submit(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	<-b.started
	done := make(chan struct{})
	go func() {
		q.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Error("Close did not cancel active transcription context")
		close(b.release)
		<-done
	}
}

func TestQueueOverloadThenRetry(t *testing.T) {
	gate := make(chan struct{})
	started := make(chan struct{})
	b := &controllableBackend{gate: gate, started: started}
	q := NewQueue(b, QueueConfig{Capacity: 1})
	defer q.Close()

	// Fill queue with 1 running + 1 queued
	j1 := &Job{ChatJID: "c1", MessageID: "m1", Audio: []byte("1"), Done: make(chan struct{})}
	if err := q.Submit(context.Background(), j1); err != nil {
		t.Fatal(err)
	}

	// Wait until j1 starts in worker
	<-started

	j2 := &Job{ChatJID: "c1", MessageID: "m2", Audio: []byte("2"), Done: make(chan struct{})}
	if err := q.Submit(context.Background(), j2); err != nil {
		t.Fatal(err)
	}

	// 3rd job exceeds capacity 1 in channel -> ErrQueueFull
	j3 := &Job{ChatJID: "c1", MessageID: "m3", Audio: []byte("3"), Done: make(chan struct{})}
	if err := q.Submit(context.Background(), j3); err != ErrQueueFull {
		t.Fatalf("expected ErrQueueFull, got %v", err)
	}

	// Open the gate so j1 and j2 finish
	close(gate)
	<-j1.Done
	<-j2.Done

	// Now m3 should be able to retry and succeed, NOT dropped as duplicate!
	j3Retry := &Job{ChatJID: "c1", MessageID: "m3", Audio: []byte("3"), Done: make(chan struct{})}
	if err := q.Submit(context.Background(), j3Retry); err != nil {
		t.Fatalf("retry for overloaded job failed: %v", err)
	}
	<-j3Retry.Done
	if j3Retry.Err != nil {
		t.Fatalf("retry job error: %v", j3Retry.Err)
	}
}

type controllableBackend struct {
	gate    chan struct{}
	started chan struct{}
	once    sync.Once
}

func (c *controllableBackend) Name() string { return "controllable" }
func (c *controllableBackend) Transcribe(ctx context.Context, b []byte, n string) (string, error) {
	c.once.Do(func() {
		if c.started != nil {
			close(c.started)
		}
	})
	if c.gate != nil {
		select {
		case <-c.gate:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return "ok", nil
}

func TestQueueCloseDrainsPendingJobs(t *testing.T) {
	b := &probeBackend{started: make(chan struct{}), release: make(chan struct{})}
	q := NewQueue(b, QueueConfig{Capacity: 5})

	j1 := &Job{ChatJID: "c1", MessageID: "m1", Audio: []byte("1"), Done: make(chan struct{})}
	_ = q.Submit(context.Background(), j1)
	<-b.started // j1 is active

	j2 := &Job{ChatJID: "c1", MessageID: "m2", Audio: []byte("2"), Done: make(chan struct{})}
	_ = q.Submit(context.Background(), j2)

	j3 := &Job{ChatJID: "c1", MessageID: "m3", Audio: []byte("3"), Done: make(chan struct{})}
	_ = q.Submit(context.Background(), j3)

	// Close queue while j1 is active and j2, j3 are queued
	q.Close()

	// j2 and j3 must be drained and completed with ErrQueueClosed
	select {
	case <-j2.Done:
		if j2.Err != ErrQueueClosed {
			t.Errorf("expected ErrQueueClosed for j2, got %v", j2.Err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for j2 to complete on Close")
	}

	select {
	case <-j3.Done:
		if j3.Err != ErrQueueClosed {
			t.Errorf("expected ErrQueueClosed for j3, got %v", j3.Err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for j3 to complete on Close")
	}

	// Late submit must be rejected
	jLate := &Job{ChatJID: "c1", MessageID: "mLate", Audio: []byte("late"), Done: make(chan struct{})}
	if err := q.Submit(context.Background(), jLate); err != ErrQueueClosed {
		t.Errorf("expected ErrQueueClosed for late submit, got %v", err)
	}
	<-jLate.Done
}
