package wa

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/thenavidm/whatsapp-mcp/internal/store"
)

func TestViewOnceAndEphemeralSuppression(t *testing.T) {
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "wa_data")
	st, err := store.Open(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	defer st.Close()

	c := &Client{
		st:      st,
		dataDir: dataDir,
	}
	ctx := context.Background()

	chatJID, _ := types.ParseJID("target@s.whatsapp.net")
	senderJID, _ := types.ParseJID("sender@s.whatsapp.net")

	// 1. Live view-once image message
	viewOnceEvt := &events.Message{
		Info: types.MessageInfo{
			ID: "VO_MSG_001",
			MessageSource: types.MessageSource{
				Chat:   chatJID,
				Sender: senderJID,
			},
			Timestamp: time.Now(),
		},
		Message: &waE2E.Message{
			ViewOnceMessage: &waE2E.FutureProofMessage{
				Message: &waE2E.Message{
					ImageMessage: &waE2E.ImageMessage{
						Caption: proto.String("Secret view-once photo"),
					},
				},
			},
		},
	}
	c.onMessage(ctx, viewOnceEvt)

	// Verify view-once message was NOT stored
	msgs, err := st.ListMessages(ctx, store.MessageFilter{ChatJID: "target@s.whatsapp.net"})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	for _, m := range msgs {
		if m.ID == "VO_MSG_001" || strings.Contains(m.Content, "Secret") {
			t.Fatalf("view-once message should not be ingested, found: %+v", m)
		}
	}

	// 2. Ephemeral message
	ephemeralEvt := &events.Message{
		Info: types.MessageInfo{
			ID: "EPH_MSG_002",
			MessageSource: types.MessageSource{
				Chat:   chatJID,
				Sender: senderJID,
			},
			Timestamp: time.Now(),
		},
		Message: &waE2E.Message{
			EphemeralMessage: &waE2E.FutureProofMessage{
				Message: &waE2E.Message{
					Conversation: proto.String("Disappearing secret message"),
				},
			},
		},
	}
	c.onMessage(ctx, ephemeralEvt)

	// Verify ephemeral message was NOT stored
	msgs, _ = st.ListMessages(ctx, store.MessageFilter{ChatJID: "target@s.whatsapp.net"})
	for _, m := range msgs {
		if m.ID == "EPH_MSG_002" || strings.Contains(m.Content, "Disappearing") {
			t.Fatalf("ephemeral message should not be ingested, found: %+v", m)
		}
	}

	// 3. Nested edited message wrapping a view-once payload
	nestedEditEvt := &events.Message{
		Info: types.MessageInfo{
			ID: "EDIT_WRAP_003",
			MessageSource: types.MessageSource{
				Chat:   chatJID,
				Sender: senderJID,
			},
			Timestamp: time.Now(),
		},
		Message: &waE2E.Message{
			EditedMessage: &waE2E.FutureProofMessage{
				Message: &waE2E.Message{
					ViewOnceMessage: &waE2E.FutureProofMessage{
						Message: &waE2E.Message{
							Conversation: proto.String("Nested view-once inside edit"),
						},
					},
				},
			},
		},
	}
	c.onMessage(ctx, nestedEditEvt)

	msgs, _ = st.ListMessages(ctx, store.MessageFilter{ChatJID: "target@s.whatsapp.net"})
	for _, m := range msgs {
		if m.ID == "EDIT_WRAP_003" || strings.Contains(m.Content, "Nested") {
			t.Fatalf("nested view-once inside edited message should be suppressed, found: %+v", m)
		}
	}

	// 4. ExtendedTextMessage honoring Ephemeral Expiration metadata in ContextInfo
	ephemeralCtxEvt := &events.Message{
		Info: types.MessageInfo{
			ID: "CTX_EPH_004",
			MessageSource: types.MessageSource{
				Chat:   chatJID,
				Sender: senderJID,
			},
			Timestamp: time.Now(),
		},
		Message: &waE2E.Message{
			ExtendedTextMessage: &waE2E.ExtendedTextMessage{
				Text: proto.String("Ephemeral via context info"),
				ContextInfo: &waE2E.ContextInfo{
					Expiration: proto.Uint32(86400),
				},
			},
		},
	}
	c.onMessage(ctx, ephemeralCtxEvt)

	msgs, _ = st.ListMessages(ctx, store.MessageFilter{ChatJID: "target@s.whatsapp.net"})
	for _, m := range msgs {
		if m.ID == "CTX_EPH_004" || strings.Contains(m.Content, "Ephemeral via context") {
			t.Fatalf("message with ephemeral ContextInfo.Expiration should be suppressed, found: %+v", m)
		}
	}
}

func TestRevokeTargetDeletesMessageAndCleansMedia(t *testing.T) {
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "wa_data")
	mediaDir := filepath.Join(dataDir, "media", "target@s.whatsapp.net")
	if err := os.MkdirAll(mediaDir, 0o700); err != nil {
		t.Fatalf("mkdir mediaDir: %v", err)
	}

	st, err := store.Open(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	defer st.Close()

	c := &Client{
		st:      st,
		dataDir: dataDir,
	}
	ctx := context.Background()

	// Seed a message with cached media
	targetMsgID := "MSG_TO_DELETE"
	cachedMediaFile := filepath.Join(mediaDir, "MSG_TO_DELETE_photo.jpg")
	if err := os.WriteFile(cachedMediaFile, []byte("fake image content"), 0o600); err != nil {
		t.Fatalf("write fake media: %v", err)
	}

	err = st.UpsertMessage(ctx, store.StoredMessage{
		Message: store.Message{
			ID:        targetMsgID,
			ChatJID:   "target@s.whatsapp.net",
			Content:   "Please delete this mistake",
			Timestamp: time.Now(),
			MsgType:   "image",
		},
		MediaPath: cachedMediaFile,
	})
	if err != nil {
		t.Fatalf("seed message: %v", err)
	}

	// Incoming REVOKE protocol message pointing to targetMsgID
	chatJID, _ := types.ParseJID("target@s.whatsapp.net")
	senderJID, _ := types.ParseJID("sender@s.whatsapp.net")
	revokeEvt := &events.Message{
		Info: types.MessageInfo{
			ID: "REVOKE_EVENT_001",
			MessageSource: types.MessageSource{
				Chat:   chatJID,
				Sender: senderJID,
			},
			Timestamp: time.Now(),
		},
		Message: &waE2E.Message{
			ProtocolMessage: &waE2E.ProtocolMessage{
				Type: waE2E.ProtocolMessage_REVOKE.Enum(),
				Key: &waCommon.MessageKey{
					ID: proto.String(targetMsgID),
				},
			},
		},
	}

	c.onMessage(ctx, revokeEvt)

	// Target message should be deleted from store
	msgs, _ := st.ListMessages(ctx, store.MessageFilter{ChatJID: "target@s.whatsapp.net"})
	for _, m := range msgs {
		if m.ID == targetMsgID {
			t.Errorf("expected target message %s to be deleted from store, but it remains", targetMsgID)
		}
	}

	// Cached media file on disk must be removed
	if _, err := os.Stat(cachedMediaFile); !os.IsNotExist(err) {
		t.Errorf("expected cached media file %s to be deleted, but it still exists", cachedMediaFile)
	}

	// 2. History sync batch carrying a REVOKE protocol message must also delete target
	histTargetID := "HIST_MSG_TO_REVOKE"
	err = st.UpsertMessage(ctx, store.StoredMessage{
		Message: store.Message{
			ID:        histTargetID,
			ChatJID:   "target@s.whatsapp.net",
			Content:   "to be purged by history sync revoke",
			Timestamp: time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("upsert hist message: %v", err)
	}

	histEvt := &events.HistorySync{
		Data: &waHistorySync.HistorySync{
			Conversations: []*waHistorySync.Conversation{
				{
					ID: proto.String("target@s.whatsapp.net"),
					Messages: []*waHistorySync.HistorySyncMsg{
						{
							Message: &waWeb.WebMessageInfo{
								Key: &waCommon.MessageKey{
									ID: proto.String("HIST_REVOKE_EVT_001"),
								},
								Message: &waE2E.Message{
									ProtocolMessage: &waE2E.ProtocolMessage{
										Type: waE2E.ProtocolMessage_REVOKE.Enum(),
										Key: &waCommon.MessageKey{
											ID: proto.String(histTargetID),
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
	c.onHistorySync(ctx, histEvt)

	msgs, _ = st.ListMessages(ctx, store.MessageFilter{ChatJID: "target@s.whatsapp.net"})
	for _, m := range msgs {
		if m.ID == histTargetID {
			t.Errorf("expected history message %s to be deleted by history sync revoke, but it remains", histTargetID)
		}
	}
}
