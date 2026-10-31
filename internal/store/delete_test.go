package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestDeleteMessageRemovesRowAndFTS(t *testing.T) {
	tmp := t.TempDir()
	st, err := Open(filepath.Join(tmp, "messages.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	chatJID := "testchat@s.whatsapp.net"
	msgID := "MSG_TO_REVOKE"
	secretText := "topsecretpassword123"

	// Insert message
	err = st.UpsertMessage(ctx, StoredMessage{
		Message: Message{
			ID:        msgID,
			ChatJID:   chatJID,
			Content:   secretText,
			Timestamp: time.Now(),
		},
		MediaPath: "/some/cached/media.jpg",
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Verify it can be found in search
	hits, err := st.Search(ctx, "topsecretpassword123", "", 10)
	if err != nil || len(hits) != 1 {
		t.Fatalf("search before delete want 1 hit, got: %d (err: %v)", len(hits), err)
	}

	// Now delete it
	mediaPath, err := st.DeleteMessage(ctx, chatJID, msgID)
	if err != nil {
		t.Fatalf("DeleteMessage: %v", err)
	}
	if mediaPath != "/some/cached/media.jpg" {
		t.Errorf("expected mediaPath '/some/cached/media.jpg', got %q", mediaPath)
	}

	// Verify message row is gone
	msgs, err := st.ListMessages(ctx, MessageFilter{ChatJID: chatJID})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("expected 0 messages in chat, got %d", len(msgs))
	}

	// Verify FTS search returns 0 hits
	hits, err = st.Search(ctx, "topsecretpassword123", "", 10)
	if err != nil {
		t.Fatalf("SearchMessages after delete: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("expected 0 hits in FTS search after delete, got %d", len(hits))
	}
}

func TestRevokeBeforeMessagePreventsLateInsertion(t *testing.T) {
	tmp := t.TempDir()
	st, err := Open(filepath.Join(tmp, "messages.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	chatJID := "chat1@s.whatsapp.net"
	msgID := "MSG_EARLY_REVOKE"

	// Revoke before the message ever arrives
	_, err = st.DeleteMessage(ctx, chatJID, msgID)
	if err != nil {
		t.Fatalf("DeleteMessage early revoke: %v", err)
	}

	// Now message arrives (e.g. out-of-order live or history sync)
	err = st.UpsertMessage(ctx, StoredMessage{
		Message: Message{
			ID:        msgID,
			ChatJID:   chatJID,
			Content:   "should never be stored",
			Timestamp: time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Must NOT exist in list or search
	msgs, err := st.ListMessages(ctx, MessageFilter{ChatJID: chatJID})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected 0 messages, got %d", len(msgs))
	}

	hits, err := st.Search(ctx, "should never be stored", "", 10)
	if err != nil || len(hits) != 0 {
		t.Fatalf("expected 0 search hits, got %d", len(hits))
	}
}

func TestLateSTTTranscriptDoesNotResurrectDeletedMessage(t *testing.T) {
	tmp := t.TempDir()
	st, err := Open(filepath.Join(tmp, "messages.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	chatJID := "chat2@s.whatsapp.net"
	msgID := "VOICE_MSG_1"

	// Message inserted
	err = st.UpsertMessage(ctx, StoredMessage{
		Message: Message{
			ID:        msgID,
			ChatJID:   chatJID,
			Content:   "[voice note]",
			Timestamp: time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Message deleted by revoke while STT was running
	_, err = st.DeleteMessage(ctx, chatJID, msgID)
	if err != nil {
		t.Fatalf("DeleteMessage: %v", err)
	}

	// Late STT callback calls UpdateTranscript
	err = st.UpdateTranscript(ctx, chatJID, msgID, "transcribed voice content")
	if err != nil {
		t.Fatalf("UpdateTranscript: %v", err)
	}

	// Message must remain deleted, never resurrected
	msgs, err := st.ListMessages(ctx, MessageFilter{ChatJID: chatJID})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected 0 messages after late STT, got %d", len(msgs))
	}

	hits, err := st.Search(ctx, "transcribed voice content", "", 10)
	if err != nil || len(hits) != 0 {
		t.Fatalf("expected 0 search hits after late STT, got %d", len(hits))
	}
}
