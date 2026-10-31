package wa

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// pairCall is the arguments of one PairPhone call.
type pairCall struct {
	phone   string
	push    bool
	client  whatsmeow.PairClientType
	display string
}

// fakePairing is a typed stand-in for the WhatsApp RPC surface that linking
// drives. It records the order of calls and nothing else; behaviour is
// scripted by the test through its fields and the qr channel.
type fakePairing struct {
	mu    sync.Mutex
	calls []string
	pairs []pairCall
	id    *types.JID

	qr         chan whatsmeow.QRChannelItem
	qrErr      error
	connectErr error
	// pairPhone answers PairPhone. Nil means a fixed synthetic code.
	pairPhone func(ctx context.Context) (string, error)

	// events carries each call name as it happens, so a test can wait for a
	// call without sleeping.
	events chan string
}

func newFakePairing() *fakePairing {
	return &fakePairing{
		qr:     make(chan whatsmeow.QRChannelItem, 8), // the real channel is also 8 deep
		events: make(chan string, 64),
	}
}

func (f *fakePairing) record(name string) {
	f.mu.Lock()
	f.calls = append(f.calls, name)
	f.mu.Unlock()
	f.events <- name
}

func (f *fakePairing) GetQRChannel(ctx context.Context) (<-chan whatsmeow.QRChannelItem, error) {
	f.record("GetQRChannel")
	if f.qrErr != nil {
		return nil, f.qrErr
	}
	return f.qr, nil
}

func (f *fakePairing) Connect() error {
	f.record("Connect")
	return f.connectErr
}

func (f *fakePairing) Disconnect() { f.record("Disconnect") }

func (f *fakePairing) PairPhone(ctx context.Context, phone string, push bool, ct whatsmeow.PairClientType, display string) (string, error) {
	f.mu.Lock()
	f.pairs = append(f.pairs, pairCall{phone, push, ct, display})
	f.mu.Unlock()
	f.record("PairPhone")
	if f.pairPhone != nil {
		return f.pairPhone(ctx)
	}
	return "SYNTH-CODE", nil
}

func (f *fakePairing) LinkedID() *types.JID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.id
}

func (f *fakePairing) setLinked() {
	jid := types.NewJID("10000000000", types.DefaultUserServer)
	f.mu.Lock()
	f.id = &jid
	f.mu.Unlock()
}

func (f *fakePairing) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakePairing) count(name string) int {
	n := 0
	for _, c := range f.callLog() {
		if c == name {
			n++
		}
	}
	return n
}

// waitFor blocks until the named call has happened. The timeout only bounds a
// broken test; a passing run never waits on it.
func (f *fakePairing) waitFor(t *testing.T, name string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case got := <-f.events:
			if got == name {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s; calls so far: %v", name, f.callLog())
		}
	}
}

// handshakeClock stands in for the handshake wait: it records what was asked
// for and either fires at once or never.
type handshakeClock struct {
	mu        sync.Mutex
	asked     []time.Duration
	neverFire bool
	// requested is closed on the first wait, so a test can act while the
	// client is inside the handshake gate.
	requested chan struct{}
	once      sync.Once
}

func (h *handshakeClock) wait(d time.Duration) <-chan time.Time {
	h.mu.Lock()
	h.asked = append(h.asked, d)
	h.mu.Unlock()
	h.once.Do(func() { close(h.requested) })
	ch := make(chan time.Time, 1)
	if !h.neverFire {
		ch <- time.Time{}
	}
	return ch
}

func (h *handshakeClock) durations() []time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]time.Duration(nil), h.asked...)
}

// syncBuffer is a bytes.Buffer safe to read while the client may still write.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newLinkClient(f *fakePairing) (*Client, *syncBuffer, *handshakeClock) {
	out := &syncBuffer{}
	clock := &handshakeClock{requested: make(chan struct{})}
	return &Client{pair: f, out: out, handshakeWait: clock.wait}, out, clock
}

// feed delivers steps to the QR channel in order, each only after the consumer
// has taken the previous one, then closes it. A step is a QRChannelItem to send
// or a func() to run in between (for example setting the stored ID the way
// whatsmeow does just before it reports success).
func (f *fakePairing) feed(steps ...any) {
	f.qr = make(chan whatsmeow.QRChannelItem) // unbuffered: the consumer paces us
	go func() {
		defer close(f.qr)
		for _, step := range steps {
			switch v := step.(type) {
			case whatsmeow.QRChannelItem:
				f.qr <- v
			case func():
				v()
			}
		}
	}()
}

func code(s string) whatsmeow.QRChannelItem {
	return whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventCode, Code: s, Timeout: time.Minute}
}

// ---- QR login, existing behaviour (characterization: written against the
// pairingAPI seam after it was introduced, so green on first run) ----

func TestQRLoginStillDrawsCodeAndFinishesAfterDurableHandshake(t *testing.T) {
	f := newFakePairing()
	c, out, clock := newLinkClient(f)
	qrs := make(chan string, 4)
	c.qrCodes = qrs

	f.feed(code("SYNTH-QR-1"), code("SYNTH-QR-2"), f.setLinked, whatsmeow.QRChannelSuccess)

	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if got, want := strings.Join(f.callLog(), ","), "GetQRChannel,Connect"; got != want {
		t.Errorf("call order = %s, want %s", got, want)
	}
	for _, want := range []string{"QR-CODE: SYNTH-QR-1", "QR-CODE: SYNTH-QR-2", "Linked as "} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output is missing %q", want)
		}
	}
	if len(qrs) != 2 {
		t.Errorf("qrCodes received %d codes, want 2", len(qrs))
	}
	if d := clock.durations(); len(d) != 1 || d[0] != 15*time.Second {
		t.Errorf("handshake waits = %v, want exactly [15s]", d)
	}
	if f.count("PairPhone") != 0 {
		t.Error("QR login must never request a phone pairing code")
	}
}

func TestQRLoginFailsWhenHandshakeLeavesNoStoredID(t *testing.T) {
	f := newFakePairing()
	c, _, _ := newLinkClient(f)
	f.qr <- whatsmeow.QRChannelSuccess // note: no setLinked
	close(f.qr)

	err := c.Connect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "handshake did not complete") {
		t.Fatalf("Connect error = %v, want handshake failure", err)
	}
}

func TestQRLoginTimeoutIsAnError(t *testing.T) {
	f := newFakePairing()
	c, _, _ := newLinkClient(f)
	f.qr <- whatsmeow.QRChannelTimeout
	close(f.qr)

	err := c.Connect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("Connect error = %v, want timeout", err)
	}
}

func TestAlreadyPairedConnectReconnectsWithoutQR(t *testing.T) {
	f := newFakePairing()
	c, out, _ := newLinkClient(f)
	f.setLinked()

	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if got, want := strings.Join(f.callLog(), ","), "Connect"; got != want {
		t.Errorf("call order = %s, want %s", got, want)
	}
	if strings.Contains(out.String(), "QR-CODE") {
		t.Error("a paired reconnect must not draw a QR code")
	}
}
