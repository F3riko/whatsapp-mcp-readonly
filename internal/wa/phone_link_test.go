package wa

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
)

// Synthetic values only. Never a real number or a real pairing code.
const (
	synthPhone  = "+14155550100"
	synthDigits = "14155550100"
	synthJIDNum = "10000000000"
)

// linkRun is a LinkWithPhone call running in the background.
type linkRun struct {
	done chan error
}

func startPhoneLink(ctx context.Context, c *Client, phone string) *linkRun {
	r := &linkRun{done: make(chan error, 1)}
	go func() { r.done <- c.LinkWithPhone(ctx, phone) }()
	return r
}

// linkBounded runs LinkWithPhone to completion, failing the test rather than
// hanging if it never returns.
func linkBounded(t *testing.T, c *Client, phone string) error {
	t.Helper()
	return startPhoneLink(context.Background(), c, phone).result(t)
}

// result waits for LinkWithPhone to return. The timeout only bounds a broken test.
func (r *linkRun) result(t *testing.T) error {
	t.Helper()
	select {
	case err := <-r.done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("LinkWithPhone did not return")
		return nil
	}
}

// waitForCall waits for the fake to see a call, failing fast if linking ended first.
func (f *fakePairing) waitForCall(t *testing.T, r *linkRun, name string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case got := <-f.events:
			if got == name {
				return
			}
		case err := <-r.done:
			r.done <- err
			t.Fatalf("LinkWithPhone ended (%v) before %s; calls: %v", err, name, f.callLog())
		case <-deadline:
			t.Fatalf("timed out waiting for %s; calls: %v", name, f.callLog())
		}
	}
}

func TestPhoneLinkRequestsExactlyOneCodeAfterFirstReadinessEvent(t *testing.T) {
	f := newFakePairing()
	c, out, clock := newLinkClient(f)
	qrs := make(chan string, 4)
	c.qrCodes = qrs
	f.qr = make(chan whatsmeow.QRChannelItem) // unbuffered: we pace every event

	var firstEventDelivered atomic.Bool
	var deliveredBeforePair bool
	f.pairPhone = func(ctx context.Context) (string, error) {
		deliveredBeforePair = firstEventDelivered.Load()
		return "SYNTH-CODE", nil
	}

	r := startPhoneLink(context.Background(), c, synthPhone)
	f.waitForCall(t, r, "Connect")
	if f.count("PairPhone") != 0 {
		t.Fatalf("PairPhone ran before any readiness event; calls: %v", f.callLog())
	}

	firstEventDelivered.Store(true)
	f.qr <- code("SYNTH-QR-1")
	f.waitForCall(t, r, "PairPhone")

	// Later QR events must not draw, relay or request anything.
	f.qr <- code("SYNTH-QR-2")
	f.qr <- code("SYNTH-QR-3")
	f.setLinked()
	f.qr <- whatsmeow.QRChannelSuccess
	close(f.qr)

	if err := r.result(t); err != nil {
		t.Fatalf("LinkWithPhone: %v", err)
	}
	if !deliveredBeforePair {
		t.Error("PairPhone was called before the first readiness event was delivered")
	}
	if got, want := strings.Join(f.callLog(), ","), "GetQRChannel,Connect,PairPhone"; got != want {
		t.Errorf("call order = %s, want %s", got, want)
	}
	want := pairCall{phone: synthDigits, push: true, client: whatsmeow.PairClientChrome, display: "Chrome (Linux)"}
	if len(f.pairs) != 1 || f.pairs[0] != want {
		t.Errorf("PairPhone calls = %+v, want exactly [%+v]", f.pairs, want)
	}
	o := out.String()
	if n := strings.Count(o, "PAIR-CODE: SYNTH-CODE\n"); n != 1 {
		t.Errorf("PAIR-CODE line appeared %d times, want 1", n)
	}
	for _, banned := range []string{"QR-CODE", "SYNTH-QR", synthDigits, synthJIDNum} {
		if strings.Contains(o, banned) {
			t.Errorf("output contains %q", banned)
		}
	}
	if len(qrs) != 0 {
		t.Errorf("phone mode relayed %d QR payloads, want 0", len(qrs))
	}
	if d := clock.durations(); len(d) != 1 || d[0] != 15*time.Second {
		t.Errorf("handshake waits = %v, want exactly [15s]", d)
	}
	if !strings.Contains(o, "Linked.") {
		t.Errorf("output lacks the success line: %q", o)
	}
}

func TestPhoneLinkRejectsInvalidNumberBeforeAnyRPC(t *testing.T) {
	for _, bad := range []string{"", "14155550100", "04155550100", "+1 415 555 0100"} {
		f := newFakePairing()
		c, out, _ := newLinkClient(f)
		err := linkBounded(t, c, bad)
		if !errors.Is(err, ErrInvalidPhone) {
			t.Errorf("LinkWithPhone(len %d) error = %v, want ErrInvalidPhone", len(bad), err)
		}
		if calls := f.callLog(); len(calls) != 0 {
			t.Errorf("invalid number caused RPC calls: %v", calls)
		}
		if out.String() != "" {
			t.Errorf("invalid number produced output: %q", out.String())
		}
	}
}

func TestPhoneLinkRefusesAnAlreadyLinkedDevice(t *testing.T) {
	f := newFakePairing()
	c, out, _ := newLinkClient(f)
	f.setLinked()

	err := linkBounded(t, c, synthPhone)
	if !errors.Is(err, ErrAlreadyLinked) {
		t.Fatalf("error = %v, want ErrAlreadyLinked", err)
	}
	if calls := f.callLog(); len(calls) != 0 {
		t.Errorf("an already-linked device caused calls (no connect, no unlink, no repair allowed): %v", calls)
	}
	if strings.Contains(out.String(), "PAIR-CODE") {
		t.Error("a pairing code was shown for an already-linked device")
	}
}

func TestPhoneLinkProviderErrorIsSanitizedAndCleanedUp(t *testing.T) {
	providerErr := errors.New("boom")
	f := newFakePairing()
	c, out, _ := newLinkClient(f)
	f.pairPhone = func(ctx context.Context) (string, error) {
		return "", fmt.Errorf("iq to %s@s.whatsapp.net with code SYNTH-SECRET-CODE: %w", synthDigits, providerErr)
	}
	f.qr <- code("SYNTH-QR-1")

	err := linkBounded(t, c, synthPhone)
	if !errors.Is(err, ErrPairCodeFailed) {
		t.Fatalf("error = %v, want ErrPairCodeFailed", err)
	}
	if errors.Is(err, providerErr) {
		t.Error("the provider error is wrapped into the returned error")
	}
	for _, text := range []string{err.Error(), out.String()} {
		for _, secret := range []string{synthDigits, "SYNTH-SECRET-CODE", "whatsapp.net"} {
			if strings.Contains(text, secret) {
				t.Errorf("output or error leaks %q", secret)
			}
		}
	}
	if got, want := strings.Join(f.callLog(), ","), "GetQRChannel,Connect,PairPhone,Disconnect"; got != want {
		t.Errorf("call order = %s, want %s", got, want)
	}
}

func TestPhoneLinkEmptyCodeIsAFailure(t *testing.T) {
	f := newFakePairing()
	c, out, _ := newLinkClient(f)
	f.pairPhone = func(ctx context.Context) (string, error) { return "", nil }
	f.qr <- code("SYNTH-QR-1")

	err := linkBounded(t, c, synthPhone)
	if !errors.Is(err, ErrPairCodeFailed) {
		t.Fatalf("error = %v, want ErrPairCodeFailed", err)
	}
	if strings.Contains(out.String(), "PAIR-CODE") {
		t.Error("printed a PAIR-CODE line for an empty code")
	}
	if f.count("Disconnect") != 1 {
		t.Errorf("Disconnect calls = %d, want 1", f.count("Disconnect"))
	}
}

func TestPhoneLinkSetupFailuresDisconnectAndStaySanitized(t *testing.T) {
	f := newFakePairing()
	c, _, _ := newLinkClient(f)
	f.connectErr = errors.New("dial tcp: no route")
	if err := linkBounded(t, c, synthPhone); err == nil {
		t.Fatal("expected a connect failure")
	}
	if f.count("PairPhone") != 0 {
		t.Error("PairPhone ran although Connect failed")
	}
	if f.count("Disconnect") != 1 {
		t.Errorf("Disconnect calls = %d, want 1", f.count("Disconnect"))
	}
}

// ---- the QR channel ends without a durable link ----

func TestPhoneLinkChannelClosingEarlyIsAFailureNotASuccess(t *testing.T) {
	cases := map[string][]whatsmeow.QRChannelItem{
		"before any event": nil,
		"after the code":   {code("SYNTH-QR-1")},
	}
	for name, items := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakePairing()
			c, out, _ := newLinkClient(f)
			for _, it := range items {
				f.qr <- it
			}
			close(f.qr)

			err := linkBounded(t, c, synthPhone)
			if !errors.Is(err, ErrPairingFailed) {
				t.Fatalf("error = %v, want ErrPairingFailed", err)
			}
			if f.count("Disconnect") != 1 {
				t.Errorf("Disconnect calls = %d, want 1", f.count("Disconnect"))
			}
			if strings.Contains(out.String(), "Linked.") {
				t.Error("reported a link that never happened")
			}
		})
	}
}

func TestPhoneLinkTimeoutIsReportedAndCleanedUp(t *testing.T) {
	f := newFakePairing()
	c, _, _ := newLinkClient(f)
	f.qr <- code("SYNTH-QR-1")
	f.qr <- whatsmeow.QRChannelTimeout
	close(f.qr)

	err := linkBounded(t, c, synthPhone)
	if !errors.Is(err, ErrPairingTimeout) {
		t.Fatalf("error = %v, want ErrPairingTimeout", err)
	}
	if f.count("Disconnect") != 1 {
		t.Errorf("Disconnect calls = %d, want 1", f.count("Disconnect"))
	}
}

func TestPhoneLinkPairErrorEventIsSanitized(t *testing.T) {
	providerErr := errors.New("boom")
	f := newFakePairing()
	c, out, _ := newLinkClient(f)
	f.qr <- code("SYNTH-QR-1")
	f.qr <- whatsmeow.QRChannelItem{
		Event: whatsmeow.QRChannelEventError,
		Error: fmt.Errorf("pair-success for %s@s.whatsapp.net code SYNTH-SECRET-CODE: %w", synthDigits, providerErr),
	}
	close(f.qr)

	err := linkBounded(t, c, synthPhone)
	if !errors.Is(err, ErrPairingFailed) {
		t.Fatalf("error = %v, want ErrPairingFailed", err)
	}
	if errors.Is(err, providerErr) {
		t.Error("the provider error is wrapped into the returned error")
	}
	for _, text := range []string{err.Error(), out.String()} {
		for _, secret := range []string{synthDigits, "SYNTH-SECRET-CODE"} {
			if strings.Contains(text, secret) {
				t.Errorf("output or error leaks %q", secret)
			}
		}
	}
	if f.count("Disconnect") != 1 {
		t.Errorf("Disconnect calls = %d, want 1", f.count("Disconnect"))
	}
}

func TestPhoneLinkTerminalLibraryEventsFail(t *testing.T) {
	for _, evt := range []whatsmeow.QRChannelItem{
		whatsmeow.QRChannelErrUnexpectedEvent,
		whatsmeow.QRChannelClientOutdated,
		whatsmeow.QRChannelScannedWithoutMultidevice,
	} {
		t.Run(evt.Event, func(t *testing.T) {
			f := newFakePairing()
			c, _, _ := newLinkClient(f)
			f.qr <- code("SYNTH-QR-1")
			f.qr <- evt
			// The channel stays open on purpose: some of these events do not close it.

			err := linkBounded(t, c, synthPhone)
			if !errors.Is(err, ErrPairingFailed) {
				t.Fatalf("error = %v, want ErrPairingFailed", err)
			}
			if f.count("Disconnect") != 1 {
				t.Errorf("Disconnect calls = %d, want 1", f.count("Disconnect"))
			}
		})
	}
}

func TestPhoneLinkSuccessNeedsADurableStoredID(t *testing.T) {
	f := newFakePairing()
	c, out, clock := newLinkClient(f)
	f.qr <- code("SYNTH-QR-1")
	f.qr <- whatsmeow.QRChannelSuccess // note: no setLinked, so the handshake left no stored ID
	close(f.qr)

	err := linkBounded(t, c, synthPhone)
	if err == nil || !strings.Contains(err.Error(), "handshake did not complete") {
		t.Fatalf("error = %v, want handshake failure", err)
	}
	if strings.Contains(out.String(), "Linked.") {
		t.Error("reported success without a stored ID")
	}
	if d := clock.durations(); len(d) != 1 || d[0] != 15*time.Second {
		t.Errorf("handshake waits = %v, want exactly [15s]", d)
	}
	if f.count("Disconnect") != 1 {
		t.Errorf("Disconnect calls = %d, want 1", f.count("Disconnect"))
	}
}

// ---- cancellation ----

func TestPhoneLinkCancelledBeforeStartMakesNoRPC(t *testing.T) {
	f := newFakePairing()
	c, _, _ := newLinkClient(f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := startPhoneLink(ctx, c, synthPhone).result(t)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if calls := f.callLog(); len(calls) != 0 {
		t.Errorf("a cancelled context still caused calls: %v", calls)
	}
}

func TestPhoneLinkCancelledBeforeReadinessNeverRequestsACode(t *testing.T) {
	f := newFakePairing() // the channel stays open and empty: the connection is not ready yet
	c, _, _ := newLinkClient(f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := startPhoneLink(ctx, c, synthPhone)
	f.waitForCall(t, r, "Connect")
	cancel()

	if err := r.result(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if f.count("PairPhone") != 0 {
		t.Error("PairPhone ran although the context was cancelled before readiness")
	}
	if got, want := strings.Join(f.callLog(), ","), "GetQRChannel,Connect,Disconnect"; got != want {
		t.Errorf("call order = %s, want %s", got, want)
	}
}

// Cancellation and the first readiness item can both be ready when linking
// first waits, and select then chooses at random. The code request must not go
// out on a dead context in either case.
func TestPhoneLinkCancelledWithReadinessAlreadyBufferedNeverRequestsACode(t *testing.T) {
	for i := 0; i < 200; i++ {
		f := newFakePairing()
		c, out, _ := newLinkClient(f)
		ctx, cancel := context.WithCancel(context.Background())
		f.onConnect = func() {
			f.qr <- code("SYNTH-QR-1")
			cancel()
		}

		err := startPhoneLink(ctx, c, synthPhone).result(t)
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("run %d: error = %v, want context.Canceled", i, err)
		}
		if f.count("PairPhone") != 0 {
			t.Fatalf("run %d: PairPhone was called on a cancelled context; calls: %v", i, f.callLog())
		}
		if out.String() != "" {
			t.Fatalf("run %d: output was written: %q", i, out.String())
		}
		if f.count("Disconnect") != 1 {
			t.Fatalf("run %d: Disconnect calls = %d, want 1", i, f.count("Disconnect"))
		}
	}
}

func TestPhoneLinkCancelledDuringPairPhoneShowsNoCode(t *testing.T) {
	f := newFakePairing()
	c, out, _ := newLinkClient(f)
	f.pairPhone = func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "", fmt.Errorf("sendIQ for %s: %w", synthDigits, ctx.Err())
	}
	f.qr <- code("SYNTH-QR-1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := startPhoneLink(ctx, c, synthPhone)
	f.waitForCall(t, r, "PairPhone")
	cancel()

	err := r.result(t)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if strings.Contains(err.Error(), synthDigits) || strings.Contains(out.String(), "PAIR-CODE") {
		t.Error("cancellation leaked the number or showed a code")
	}
	if f.count("Disconnect") != 1 {
		t.Errorf("Disconnect calls = %d, want 1", f.count("Disconnect"))
	}
}

func TestPhoneLinkCancelledDuringPairPhoneEvenIfTheProviderIgnoresItShowsNoCode(t *testing.T) {
	f := newFakePairing()
	c, out, _ := newLinkClient(f)
	release := make(chan struct{})
	f.pairPhone = func(context.Context) (string, error) {
		<-release // ignores ctx entirely, then answers with a valid code
		return "SYNTH-CODE", nil
	}
	f.qr <- code("SYNTH-QR-1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := startPhoneLink(ctx, c, synthPhone)
	f.waitForCall(t, r, "PairPhone")
	cancel()
	close(release)

	if err := r.result(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if out.String() != "" {
		t.Errorf("output after cancellation: %q", out.String())
	}
	if f.count("Disconnect") != 1 {
		t.Errorf("Disconnect calls = %d, want 1", f.count("Disconnect"))
	}
}

func TestPhoneLinkCancelledWhileWaitingForTheOwnerDisconnects(t *testing.T) {
	f := newFakePairing()
	c, _, _ := newLinkClient(f)
	f.qr <- code("SYNTH-QR-1") // channel then stays open: the owner has not typed the code yet
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := startPhoneLink(ctx, c, synthPhone)
	f.waitForCall(t, r, "PairPhone")
	cancel()

	if err := r.result(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if f.count("PairPhone") != 1 || f.count("Disconnect") != 1 {
		t.Errorf("calls = %v, want one PairPhone and one Disconnect", f.callLog())
	}
}

func TestPhoneLinkCancelledDuringHandshakeIsNotASuccess(t *testing.T) {
	f := newFakePairing()
	c, out, clock := newLinkClient(f)
	clock.neverFire = true
	f.qr <- code("SYNTH-QR-1")
	f.qr <- whatsmeow.QRChannelSuccess
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := startPhoneLink(ctx, c, synthPhone)
	select {
	case <-clock.requested:
	case <-time.After(10 * time.Second):
		t.Fatal("never reached the handshake gate")
	}
	cancel()

	if err := r.result(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if strings.Contains(out.String(), "Linked.") {
		t.Error("reported success although cancelled mid-handshake")
	}
	if f.count("Disconnect") != 1 {
		t.Errorf("Disconnect calls = %d, want 1", f.count("Disconnect"))
	}
}
