package wa

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/thenavidm/whatsapp-mcp/internal/store"
	"github.com/thenavidm/whatsapp-mcp/internal/transcribe"
)

func TestMediaCompositeIdentityAndPathContainment(t *testing.T) {
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "wa_data")

	st, err := store.Open(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	c := &Client{
		dataDir: dataDir,
		st:      st,
	}

	// 1. Two messages with same filename in same chat have distinct composite hash paths
	path1 := c.resolveMediaPath("chat1@s.whatsapp.net", "MSG_001", "photo.jpg", "image")
	path2 := c.resolveMediaPath("chat1@s.whatsapp.net", "MSG_002", "photo.jpg", "image")

	if path1 == path2 {
		t.Fatalf("expected distinct paths for different message IDs, got identical path: %s", path1)
	}

	// 2. Same message in distinct chats have distinct composite hash paths
	path3 := c.resolveMediaPath("chat2@s.whatsapp.net", "MSG_001", "photo.jpg", "image")
	if path1 == path3 {
		t.Fatalf("expected distinct paths for different chats with same message ID, got identical path: %s", path1)
	}

	// 3. Traversal in filename or chatID must remain strictly contained within mediaRoot
	mediaRoot := filepath.Join(dataDir, "media")
	traversalPath := c.resolveMediaPath("../../bad_chat", "MSG_003", "../../../etc/passwd", "document")
	rel, err := filepath.Rel(mediaRoot, traversalPath)
	if err != nil || strings.HasPrefix(rel, "..") || strings.HasPrefix(rel, "/") {
		t.Errorf("traversal path escaped media root: %s", traversalPath)
	}
	if filepath.Dir(traversalPath) != mediaRoot {
		t.Errorf("expected target directly under mediaRoot, got %s", traversalPath)
	}
}

func TestMediaCacheRejectsSymlinkAndDirectoryHits(t *testing.T) {
	tmp := t.TempDir()
	mediaRoot := filepath.Join(tmp, "media")
	if err := os.Mkdir(mediaRoot, 0o700); err != nil {
		t.Fatal(err)
	}

	// 1. Target is a directory -> must be rejected
	dirHit := filepath.Join(mediaRoot, "dirhit.jpg")
	if err := os.Mkdir(dirHit, 0o700); err != nil {
		t.Fatal(err)
	}
	if isSecureCacheHit(mediaRoot, dirHit) {
		t.Errorf("isSecureCacheHit accepted directory as cache hit")
	}

	// 2. Target is a symlink to outside -> must be rejected
	outside := filepath.Join(tmp, "outside.jpg")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	symHit := filepath.Join(mediaRoot, "symhit.jpg")
	if err := os.Symlink(outside, symHit); err != nil {
		t.Fatal(err)
	}
	if isSecureCacheHit(mediaRoot, symHit) {
		t.Errorf("isSecureCacheHit accepted symlink as cache hit")
	}

	// 3. Regular file -> accepted
	regFile := filepath.Join(mediaRoot, "valid.jpg")
	if err := os.WriteFile(regFile, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !isSecureCacheHit(mediaRoot, regFile) {
		t.Errorf("isSecureCacheHit rejected valid regular file")
	}
}

func TestSafeRemoveMediaRejectsSymlinksAndParentTraversal(t *testing.T) {
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "wa_data")
	mediaRoot := filepath.Join(dataDir, "media")
	if err := os.MkdirAll(mediaRoot, 0o700); err != nil {
		t.Fatal(err)
	}

	c := &Client{dataDir: dataDir}

	// Victim outside mediaRoot
	victim := filepath.Join(tmp, "important_config.json")
	if err := os.WriteFile(victim, []byte("do not delete"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 1. Path attempting relative traversal out of mediaRoot
	traversal := filepath.Join(mediaRoot, "..", "..", "important_config.json")
	c.safeRemoveMedia(traversal)
	if _, err := os.Stat(victim); os.IsNotExist(err) {
		t.Fatalf("safeRemoveMedia deleted victim via traversal: %s", traversal)
	}

	// 2. Symlink inside mediaRoot pointing to victim
	symlink := filepath.Join(mediaRoot, "evil_link.bin")
	if err := os.Symlink(victim, symlink); err != nil {
		t.Fatal(err)
	}
	c.safeRemoveMedia(symlink)
	if _, err := os.Stat(victim); os.IsNotExist(err) {
		t.Fatalf("safeRemoveMedia followed symlink to delete victim outside cache")
	}

	// 3. Valid file inside mediaRoot is cleanly removed
	valid := filepath.Join(mediaRoot, "normal.jpg")
	if err := os.WriteFile(valid, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.safeRemoveMedia(valid)
	if _, err := os.Stat(valid); !os.IsNotExist(err) {
		t.Errorf("safeRemoveMedia failed to delete valid media file")
	}
}

type fakeBurstDownloader struct {
	calls       atomic.Int64
	concurrency atomic.Int32
	maxSeen     atomic.Int32
	gate        chan struct{}
}

func (f *fakeBurstDownloader) Download(ctx context.Context, msg whatsmeow.DownloadableMessage) ([]byte, error) {
	cur := f.concurrency.Add(1)
	defer f.concurrency.Add(-1)

	for {
		m := f.maxSeen.Load()
		if cur <= m || f.maxSeen.CompareAndSwap(m, cur) {
			break
		}
	}

	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.calls.Add(1)
	return []byte("fake-ogg-audio"), nil
}

type dummyTranscriber struct{}

func (d *dummyTranscriber) Name() string { return "dummy" }
func (d *dummyTranscriber) Transcribe(ctx context.Context, b []byte, fn string) (string, error) {
	return "dummy transcript", nil
}

// TestSTTAdmissionPrecedesDownloadProvesBurstOverloadDoesNotDownload
// asserts that burst inbound events beyond capacity do NOT trigger download,
// and that active downloads are strictly serialized (concurrency <= 1).
func TestSTTAdmissionPrecedesDownloadProvesBurstOverloadDoesNotDownload(t *testing.T) {
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "wa_data")

	st, err := store.Open(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	gate := make(chan struct{})
	fakeDL := &fakeBurstDownloader{gate: gate}
	sttQueue := transcribe.NewQueue(&dummyTranscriber{}, transcribe.QueueConfig{
		Capacity:      2,
		MaxAudioBytes: 1024 * 1024,
	})
	defer sttQueue.Close()

	c := &Client{
		downloader: fakeDL,
		st:         st,
		sttQueue:   sttQueue,
		dataDir:    dataDir,
		out:        os.Stderr,
	}

	ctx := context.Background()

	// Simulate a burst of 10 voice note messages arriving rapidly
	for i := 0; i < 10; i++ {
		msgID := string(rune('A' + i))
		evt := &events.Message{
			Info: types.MessageInfo{
				ID: msgID,
				MessageSource: types.MessageSource{
					Chat:   types.JID{User: "chat1", Server: "s.whatsapp.net"},
					Sender: types.JID{User: "sender1", Server: "s.whatsapp.net"},
				},
				Timestamp: time.Now(),
			},
			Message: &waE2E.Message{
				AudioMessage: &waE2E.AudioMessage{
					URL:        proto.String("https://mmg.whatsapp.net/v/audio.ogg"),
					FileLength: proto.Uint64(100),
					PTT:        proto.Bool(true),
				},
			},
		}
		c.onMessage(ctx, evt)
	}

	// Unblock downloads now that burst submission is complete
	close(gate)

	// Wait for queue worker to finish admitted jobs
	time.Sleep(100 * time.Millisecond)

	calls := fakeDL.calls.Load()
	// With capacity 2 in channel and 1 in worker, exactly 3 jobs are admitted and processed;
	// the remaining 7 were rejected synchronously BEFORE any download occurred!
	if calls != 3 {
		t.Fatalf("burst of 10 events called download %d times; expected exactly 3 admitted without download storm", calls)
	}

	maxConc := fakeDL.maxSeen.Load()
	if maxConc > 1 {
		t.Fatalf("expected maximum download concurrency of 1, got %d", maxConc)
	}
}
