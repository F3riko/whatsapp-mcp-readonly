package wa

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/thenavidm/whatsapp-mcp/internal/store"
	"github.com/thenavidm/whatsapp-mcp/internal/transcribe"
)

type mockGatedDownloader struct {
	calls atomic.Int64
	enter chan struct{}
	gate  chan struct{}
	data  []byte
}

func (m *mockGatedDownloader) Download(ctx context.Context, msg whatsmeow.DownloadableMessage) ([]byte, error) {
	m.calls.Add(1)
	if m.enter != nil {
		select {
		case m.enter <- struct{}{}:
		default:
		}
	}
	if m.gate != nil {
		select {
		case <-m.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if m.data != nil {
		return m.data, nil
	}
	return []byte("fake-downloaded-media-payload"), nil
}

func TestMediaPublicationEEXISTRejectsSymlinkAndDirectory(t *testing.T) {
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "wa_data")
	st, err := store.Open(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	mockDL := &mockGatedDownloader{data: []byte("secure-payload")}
	c := &Client{
		dataDir:    dataDir,
		st:         st,
		downloader: mockDL,
	}

	ctx := context.Background()
	chatJID := "chat_eexist@s.whatsapp.net"
	msgID := "msg_eexist_001"

	// Record message with attachment
	err = st.UpsertMessage(ctx, store.StoredMessage{
		Message: store.Message{
			ID:        msgID,
			ChatJID:   chatJID,
			MediaType: "image",
			Filename:  "photo.jpg",
		},
		MediaURL:   "https://mmg.whatsapp.net/v/photo.jpg",
		FileLength: 50,
	})
	if err != nil {
		t.Fatalf("UpsertMessage: %v", err)
	}

	targetPath := c.resolveMediaPath(chatJID, msgID, "photo.jpg", "image")
	mediaRoot := filepath.Dir(targetPath)
	if err := os.MkdirAll(mediaRoot, 0o700); err != nil {
		t.Fatalf("mkdir mediaRoot: %v", err)
	}

	// 1. Target already exists as a symlink pointing to an outside file
	outside := filepath.Join(tmp, "victim.txt")
	if err := os.WriteFile(outside, []byte("victim-original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, targetPath); err != nil {
		t.Fatal(err)
	}

	_, err = c.DownloadMedia(ctx, chatJID, msgID)
	if err == nil {
		t.Fatal("expected DownloadMedia to fail when target exists as symlink, got nil")
	}
	if !strings.Contains(err.Error(), "not a valid") && !strings.Contains(err.Error(), "regular") {
		t.Errorf("expected error about invalid/non-regular cache file, got: %v", err)
	}
	// Verify victim was not overwritten
	victimContent, _ := os.ReadFile(outside)
	if string(victimContent) != "victim-original" {
		t.Fatalf("victim file was overwritten! Content: %s", string(victimContent))
	}

	// Clean up symlink
	_ = os.Remove(targetPath)

	// 2. Target already exists as a directory
	if err := os.Mkdir(targetPath, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err = c.DownloadMedia(ctx, chatJID, msgID)
	if err == nil {
		t.Fatal("expected DownloadMedia to fail when target exists as directory, got nil")
	}
	_ = os.Remove(targetPath)
}

func TestConcurrentMediaPublicationAndRevoke(t *testing.T) {
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "wa_data")
	st, err := store.Open(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	enterDL := make(chan struct{}, 1)
	gateDL := make(chan struct{})
	mockDL := &mockGatedDownloader{
		enter: enterDL,
		gate:  gateDL,
		data:  []byte("private-photo-bytes"),
	}

	c := &Client{
		dataDir:    dataDir,
		st:         st,
		downloader: mockDL,
	}

	ctx := context.Background()
	chatJID := "chat_revoke@s.whatsapp.net"
	msgID := "msg_revoke_001"

	err = st.UpsertMessage(ctx, store.StoredMessage{
		Message: store.Message{
			ID:        msgID,
			ChatJID:   chatJID,
			MediaType: "image",
			Filename:  "photo.jpg",
		},
		MediaURL:   "https://mmg.whatsapp.net/v/photo.jpg",
		FileLength: 50,
	})
	if err != nil {
		t.Fatalf("UpsertMessage: %v", err)
	}

	targetPath := c.resolveMediaPath(chatJID, msgID, "photo.jpg", "image")

	// Start slow download in background
	dlDone := make(chan error, 1)
	go func() {
		_, dlErr := c.DownloadMedia(ctx, chatJID, msgID)
		dlDone <- dlErr
	}()

	// Wait until downloader starts downloading
	select {
	case <-enterDL:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for download to enter")
	}

	// While download is in-flight, a REVOKE event arrives
	revokeEvt := &events.Message{
		Info: types.MessageInfo{
			ID: "revoke_ctrl_001",
			MessageSource: types.MessageSource{
				Chat:   types.JID{User: "chat_revoke", Server: "s.whatsapp.net"},
				Sender: types.JID{User: "sender", Server: "s.whatsapp.net"},
			},
			Timestamp: time.Now(),
		},
		Message: &waE2E.Message{
			ProtocolMessage: &waE2E.ProtocolMessage{
				Type: waE2E.ProtocolMessage_REVOKE.Enum(),
				Key: &waCommon.MessageKey{
					ID: proto.String(msgID),
				},
			},
		},
	}
	c.onMessage(ctx, revokeEvt)

	// Verify message was deleted from DB
	isRevoked, err := st.IsRevoked(ctx, chatJID, msgID)
	if err != nil || !isRevoked {
		t.Fatalf("expected message to be revoked in DB, got isRevoked=%v, err=%v", isRevoked, err)
	}

	// Now unblock the slow download
	close(gateDL)

	dlErr := <-dlDone
	if dlErr == nil {
		t.Fatal("expected DownloadMedia to return error because message was revoked during download, got nil")
	}

	// Crucial assertion: the revoked media must NOT exist on disk!
	if _, err := os.Stat(targetPath); !os.IsNotExist(err) {
		t.Fatalf("revoked media file %s still exists on disk after revoke!", targetPath)
	}
}

func TestMediaCacheFullBeforeDownload(t *testing.T) {
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "wa_data")
	st, err := store.Open(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	mockDL := &mockGatedDownloader{data: []byte("1234567890")}
	c := &Client{
		dataDir:       dataDir,
		st:            st,
		downloader:    mockDL,
		maxCacheBytes: 30, // tiny budget: 30 bytes
	}

	ctx := context.Background()
	chatJID := "chat_quota@s.whatsapp.net"

	// Message 1: 20 bytes declared
	_ = st.UpsertMessage(ctx, store.StoredMessage{
		Message:    store.Message{ID: "m1", ChatJID: chatJID, MediaType: "image"},
		MediaURL:   "https://mmg.whatsapp.net/v/1.jpg",
		FileLength: 20,
	})
	// Message 2: 25 bytes declared (10 actual bytes from m1 + 25 bytes for m2 = 35 > 30 bytes quota)
	_ = st.UpsertMessage(ctx, store.StoredMessage{
		Message:    store.Message{ID: "m2", ChatJID: chatJID, MediaType: "image"},
		MediaURL:   "https://mmg.whatsapp.net/v/2.jpg",
		FileLength: 25,
	})

	// Download m1 -> succeeds (uses 10 bytes actual)
	_, err = c.DownloadMedia(ctx, chatJID, "m1")
	if err != nil {
		t.Fatalf("first download failed: %v", err)
	}
	if mockDL.calls.Load() != 1 {
		t.Fatalf("expected 1 download call, got %d", mockDL.calls.Load())
	}

	// Now try download m2 with declared size 25 bytes (10 used + 25 needed = 35 > 30 budget)
	_, err = c.DownloadMedia(ctx, chatJID, "m2")
	if err == nil {
		t.Fatal("expected download to be rejected due to cache budget, got nil")
	}
	if !errors.Is(err, ErrMediaCacheFull) && !strings.Contains(err.Error(), "cache") {
		t.Fatalf("expected ErrMediaCacheFull, got: %v", err)
	}

	// Verify downloader was NOT called for m2 (rejected before network!)
	if mockDL.calls.Load() != 1 {
		t.Fatalf("downloader was called despite budget exhaustion! calls=%d", mockDL.calls.Load())
	}
}

func TestSharedDownloadConcurrencyAdmission(t *testing.T) {
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "wa_data")
	st, err := store.Open(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	gate := make(chan struct{})
	entered := make(chan struct{}, 10)
	mockDL := &mockGatedDownloader{
		enter: entered,
		gate:  gate,
		data:  []byte("audio-bytes"),
	}

	sttQueue := transcribe.NewQueue(&dummyTranscriber{}, transcribe.QueueConfig{
		Capacity:      4,
		MaxAudioBytes: 1024 * 1024,
	})
	defer sttQueue.Close()

	c := &Client{
		dataDir:    dataDir,
		st:         st,
		downloader: mockDL,
		sttQueue:   sttQueue,
		out:        os.Stderr,
	}

	ctx := context.Background()
	chatJID := "chat_shared@s.whatsapp.net"

	// Prepare 3 messages
	for _, id := range []string{"slot1", "slot2", "slot3"} {
		_ = st.UpsertMessage(ctx, store.StoredMessage{
			Message:    store.Message{ID: id, ChatJID: chatJID, MediaType: "image"},
			MediaURL:   "https://mmg.whatsapp.net/v/" + id + ".jpg",
			FileLength: 50,
		})
	}

	// Start 2 concurrent MCP downloads (MaxConcurrentMediaDownloads = 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = c.DownloadMedia(ctx, chatJID, "slot1")
	}()
	go func() {
		defer wg.Done()
		_, _ = c.DownloadMedia(ctx, chatJID, "slot2")
	}()

	// Wait until both enter downloader
	<-entered
	<-entered

	// Now slots are fully saturated (2 active downloads).
	// Attempt a 3rd MCP DownloadMedia: must reject immediately with overload!
	_, err = c.DownloadMedia(ctx, chatJID, "slot3")
	if err == nil {
		t.Fatal("expected 3rd concurrent download to be rejected by admission limiter, got nil")
	}
	if !errors.Is(err, ErrDownloadOverload) && !strings.Contains(err.Error(), "admission") && !strings.Contains(err.Error(), "overload") {
		t.Fatalf("expected ErrDownloadOverload, got: %v", err)
	}

	// Downloader calls must be exactly 2 (the 3rd was rejected before downloading)
	if mockDL.calls.Load() != 2 {
		t.Fatalf("downloader was called %d times, expected 2", mockDL.calls.Load())
	}

	// Unblock downloads
	close(gate)
	wg.Wait()

	// Now that slots are released, a subsequent download succeeds
	mockDL.gate = nil // no more gating
	resPath, err := c.DownloadMedia(ctx, chatJID, "slot3")
	if err != nil {
		t.Fatalf("subsequent download failed after slots freed: %v", err)
	}
	if resPath == "" {
		t.Fatal("expected valid result path")
	}
}
