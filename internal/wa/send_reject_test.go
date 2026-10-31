package wa

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestLowLevelSendsUnconditionallyRejectedBeforeFileOrNetwork(t *testing.T) {
	// A client struct initialized with no connection / nil whatsmeow client
	c := &Client{}
	ctx := context.Background()

	// 1. SendText must fail with read-only error
	_, err := c.SendText(ctx, "12345@s.whatsapp.net", "hello")
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("SendText: expected read-only rejection, got: %v", err)
	}

	// 2. SendFile must fail with read-only error BEFORE reading the file.
	// If it checked the file first, it would return a file not found / os.Stat / os.ReadFile error.
	nonexistentPath := "/definitely/nonexistent/path/for/read_only_test.txt"
	_, err = c.SendFile(ctx, "12345@s.whatsapp.net", nonexistentPath, "caption")
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("SendFile: expected read-only rejection before file read, got: %v", err)
	}

	// 3. SendVoiceNote must fail with read-only error BEFORE checking file extension or reading file.
	_, err = c.SendVoiceNote(ctx, "12345@s.whatsapp.net", "invalid_ext.mp3")
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("SendVoiceNote: expected read-only rejection, got: %v", err)
	}

	// 4. SendVoiceBytes must fail with read-only error before upload/network
	_, err = c.SendVoiceBytes(ctx, "12345@s.whatsapp.net", []byte("fake ogg"))
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("SendVoiceBytes: expected read-only rejection, got: %v", err)
	}

	// 5. SendAudioBytes must fail with read-only error before upload/network
	_, err = c.SendAudioBytes(ctx, "12345@s.whatsapp.net", []byte("fake audio"), "test.mp3")
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("SendAudioBytes: expected read-only rejection, got: %v", err)
	}

	// Verify error matches ErrReadOnly if exported
	if !errors.Is(err, ErrReadOnly) {
		t.Errorf("expected error to be ErrReadOnly, got: %v", err)
	}
}
