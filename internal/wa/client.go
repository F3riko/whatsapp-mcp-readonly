// Package wa owns the WhatsApp connection: pairing, live events and history
// sync. Everything it learns is written through internal/store.
package wa

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/proto/waE2E"
	waStore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/thenavidm/whatsapp-mcp/internal/store"
	"github.com/thenavidm/whatsapp-mcp/internal/transcribe"
)

// ErrReadOnly is returned by all send operations in this hardened read-only fork.
var ErrReadOnly = errors.New("read-only fork: sending messages is disabled")

// How much backlog to ask WhatsApp for during pairing. The defaults deliver
// about one message per chat, which builds a list but cannot be searched.
const (
	historySyncDays   = 1825 // five years
	historySyncSizeMB = 4096
)

type mediaDownloader interface {
	Download(ctx context.Context, msg whatsmeow.DownloadableMessage) ([]byte, error)
}

// Client is a linked WhatsApp Web device backed by a message store.
type Client struct {
	wm         *whatsmeow.Client
	downloader mediaDownloader
	st         *store.Store

	// out is where pairing instructions and the QR code are drawn.
	//
	// This must never be os.Stdout. In stdio mode stdout carries the MCP
	// JSON-RPC stream, and a single stray byte written there corrupts the
	// protocol and the client disconnects. other implementations prints
	// its QR to stdout, which is only survivable because its bridge is a
	// separate process from its MCP server. In a single binary it is fatal.
	out io.Writer

	// stt is optional. When nil, voice notes are stored but not transcribed.
	stt      transcribe.Transcriber
	sttQueue *transcribe.Queue

	// dataDir is where downloaded attachments are written.
	dataDir string

	// qrCodes, when set, receives each pairing code so a caller can render it
	// itself rather than relying on the terminal drawing.
	qrCodes chan<- string

	mu        sync.RWMutex
	connected bool
	syncing   bool

	// Media publication and revoke serialization
	mediaPubRevokeMu sync.Mutex

	// Shared download admission semaphore across MCP media and STT
	initMu      sync.Mutex
	downloadSem chan struct{}

	// Media disk cache quota
	quotaMu            sync.Mutex
	usedCacheBytes     int64
	reservedCacheBytes int64
	cacheScanned       bool
	maxCacheBytes      int64
}

// Options configures a client.
type Options struct {
	// DataDir holds the session database and downloaded media.
	DataDir string
	// Store is the message store to write history into.
	Store *store.Store
	// Out receives pairing output. Defaults to os.Stderr.
	Out io.Writer
	// Debug turns on whatsmeow's protocol logging, to Out.
	Debug bool
	// Transcriber turns voice notes into searchable text. Optional.
	Transcriber transcribe.Transcriber
	// QRCodes, when set, receives each pairing code as it is issued.
	QRCodes chan<- string
	// DeviceName is what shows under Linked Devices on the phone. Defaults to
	// "WhatsApp MCP". WhatsApp shows "Other device" when a client sends
	// nothing, which is what happens by default.
	DeviceName string
}

// New opens the session database and builds a client. It does not connect.
func New(ctx context.Context, opts Options) (*Client, error) {
	if opts.Store == nil {
		return nil, fmt.Errorf("store is required")
	}
	out := opts.Out
	if out == nil {
		out = os.Stderr
	}

	if err := store.EnsureSecureDir(opts.DataDir); err != nil {
		return nil, fmt.Errorf("secure data directory: %w", err)
	}

	// Identify ourselves before the client is built. WhatsApp reads these
	// during pairing and shows them under Linked Devices forever after, so
	// changing them later has no effect on an existing link.
	//
	// The icon is not ours to choose. WhatsApp picks it from PlatformType,
	// which is a fixed enum, so DESKTOP gets a monitor rather than the blank
	// mark that UNKNOWN produces.
	name := opts.DeviceName
	if name == "" {
		name = "WhatsApp MCP"
	}
	waStore.DeviceProps.Os = &name
	waStore.DeviceProps.PlatformType = waCompanionReg.DeviceProps_DESKTOP.Enum()

	// Ask for a deep history sync rather than the shallow default.
	//
	// WhatsApp decides how much backlog to push, and with the defaults it
	// sends roughly one message per chat: enough to build a chat list, far too
	// little to search. Raising these turns hundreds of one-line stubs into
	// real conversations.
	//
	// These are only read during pairing, so an existing link keeps whatever
	// it negotiated. Re-pair to apply a change.
	if hs := waStore.DeviceProps.HistorySyncConfig; hs != nil {
		hs.FullSyncDaysLimit = proto.Uint32(historySyncDays)
		hs.FullSyncSizeMbLimit = proto.Uint32(historySyncSizeMB)
		hs.RecentSyncDaysLimit = proto.Uint32(historySyncDays)
		hs.StorageQuotaMb = proto.Uint32(historySyncSizeMB)
	}

	level := "ERROR"
	if opts.Debug {
		level = "DEBUG"
	}
	logger := waLog.Stdout("whatsapp", level, true)

	// The session database holds the linked device's Signal keys. It is
	// credentials, so it lives 0700 alongside the message store.
	sessionPath := filepath.Join(opts.DataDir, "session.db")
	if err := store.EnsureSecureDatabaseFile(sessionPath); err != nil {
		return nil, fmt.Errorf("secure session store: %w", err)
	}
	container, err := sqlstore.New(ctx, "sqlite", "file:"+sessionPath+"?_pragma=foreign_keys(1)", logger)
	if err != nil {
		return nil, fmt.Errorf("open session store: %w", err)
	}

	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, fmt.Errorf("load device: %w", err)
	}

	var sttQ *transcribe.Queue
	if opts.Transcriber != nil {
		sttQ = transcribe.NewQueue(opts.Transcriber, transcribe.QueueConfig{})
	}

	c := &Client{
		wm:       whatsmeow.NewClient(device, logger),
		st:       opts.Store,
		out:      out,
		stt:      opts.Transcriber,
		sttQueue: sttQ,
		dataDir:  opts.DataDir,
		qrCodes:  opts.QRCodes,
	}
	c.downloader = c.wm
	c.wm.SetMediaHTTPClient(newBoundedMediaHTTPClient(MaxMediaDownloadBytes))
	c.wm.AddEventHandler(c.handleEvent)
	return c, nil
}

// ServeConnect is used during normal server execution and refuses unpaired sessions.
func (c *Client) ServeConnect(ctx context.Context) error {
	if c.wm == nil || c.wm.Store == nil || c.wm.Store.ID == nil {
		return fmt.Errorf("whatsapp session is not paired; run 'whatsapp-mcp login' interactively to pair a device")
	}
	return c.Connect(ctx)
}

// Connect links the device, showing a QR code if this is a first pairing, and
// blocks until the connection is established or ctx is canceled.
func (c *Client) Connect(ctx context.Context) error {
	if c.wm.Store.ID != nil {
		// Already paired. Reconnect using the stored session.
		if err := c.wm.Connect(); err != nil {
			return fmt.Errorf("reconnect: %w", err)
		}
		return nil
	}

	qrChan, err := c.wm.GetQRChannel(ctx)
	if err != nil {
		return fmt.Errorf("qr channel: %w", err)
	}
	if err := c.wm.Connect(); err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	for evt := range qrChan {
		switch evt.Event {
		case "code":
			// The raw code is emitted on its own line as well as drawn, so a
			// wrapper can render the QR somewhere better than a terminal. A
			// browser popup is far easier to scan than terminal blocks.
			if c.qrCodes != nil {
				c.qrCodes <- evt.Code
			}
			fmt.Fprintln(c.out, "\nScan this with WhatsApp on your phone.")
			fmt.Fprintln(c.out, "Settings, then Linked Devices, then Link a Device.")
			fmt.Fprintf(c.out, "\nQR-CODE: %s\n\n", evt.Code)
			qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, c.out)
		case "success":
			// Pairing is not finished when the code is accepted. WhatsApp
			// completes the handshake over the live connection immediately
			// afterwards, and the session it wrote is only durable once that
			// lands. Returning here killed the process mid-handshake, so the
			// phone dropped the device and the local row went stale.
			fmt.Fprintln(c.out, "\nCode accepted. Finishing the handshake, this takes a few seconds.")
			select {
			case <-time.After(15 * time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
			if c.wm.Store.ID == nil {
				return fmt.Errorf("handshake did not complete, run the command again")
			}
			fmt.Fprintf(c.out, "Linked as %s.\nIt will now appear under Linked Devices on your phone.\n", c.wm.Store.ID.String())
			return nil
		case "timeout":
			return fmt.Errorf("pairing timed out, run the command again")
		default:
			if evt.Error != nil {
				return fmt.Errorf("pairing failed: %w", evt.Error)
			}
		}
	}
	return ctx.Err()
}

// Disconnect closes the connection and shuts down the inference worker.
func (c *Client) Disconnect() {
	if c.sttQueue != nil {
		c.sttQueue.Close()
	}
	if c.wm != nil {
		c.wm.Disconnect()
	}
}

// Status reports connection health, for the session health tool.
type Status struct {
	Paired    bool   `json:"paired"`
	Connected bool   `json:"connected"`
	Syncing   bool   `json:"syncing"`
	JID       string `json:"jid,omitempty"`
	PushName  string `json:"push_name,omitempty"`
	// Transcription names the active provider, or is empty when off.
	Transcription string `json:"transcription,omitempty"`
	// AutoReply names the command answering one chat, or is empty when off.
	AutoReply string `json:"auto_reply,omitempty"`
}

// Status returns the current state of the link.
func (c *Client) Status() Status {
	c.mu.RLock()
	defer c.mu.RUnlock()

	s := Status{Connected: c.connected, Syncing: c.syncing}
	if c.stt != nil {
		s.Transcription = c.stt.Name()
	}
	if id := c.wm.Store.ID; id != nil {
		s.Paired = true
		s.JID = id.String()
		s.PushName = c.wm.Store.PushName
	}
	return s
}

// handleEvent is the single entry point for everything WhatsApp sends us.
func (c *Client) handleEvent(evt any) {
	// Events arrive on whatsmeow's own goroutines with no context, so give
	// each handler a bounded one rather than using context.Background.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	switch v := evt.(type) {
	case *events.Connected:
		c.setConnected(true)
		// The address book is only readable once connected, and it is what
		// gives one to one chats their names.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if n, err := c.SyncContacts(ctx); err == nil && n > 0 {
				fmt.Fprintf(c.out, "Synced %d contacts.\n", n)
			}
		}()
	case *events.Disconnected:
		c.setConnected(false)
	case *events.LoggedOut:
		c.setConnected(false)
		fmt.Fprintln(c.out, "Logged out of WhatsApp. Pair again to reconnect.")
	case *events.Message:
		c.onMessage(ctx, v)
	case *events.HistorySync:
		c.onHistorySync(ctx, v)
	case *events.PushName:
		_ = c.st.UpsertContact(ctx, store.Contact{
			JID:      v.JID.ToNonAD().String(),
			PushName: v.NewPushName,
		})
	case *events.Contact:
		_ = c.st.UpsertContact(ctx, store.Contact{
			JID:      v.JID.ToNonAD().String(),
			FullName: v.Action.GetFullName(),
		})
	}
}

func (c *Client) setConnected(v bool) {
	c.mu.Lock()
	c.connected = v
	c.mu.Unlock()
}

func (c *Client) setSyncing(v bool) {
	c.mu.Lock()
	c.syncing = v
	c.mu.Unlock()
}

// isViewOnceOrEphemeral detects explicit view-once or disappearing ephemeral payloads,
// recursing through edited and device-sent wrappers, and inspecting ephemeral context metadata.
//
// Limitations: Only inspects declared protobuf wrappers and ephemeral fields; does not decrypt
// out-of-band ephemeral server policy changes when not present on the message payload.
func isViewOnceOrEphemeral(msg *waE2E.Message) bool {
	return isViewOnceOrEphemeralRecurse(msg, 0)
}

func isViewOnceOrEphemeralRecurse(msg *waE2E.Message, depth int) bool {
	if msg == nil || depth > 10 {
		return false
	}
	if msg.GetViewOnceMessage() != nil ||
		msg.GetViewOnceMessageV2() != nil ||
		msg.GetViewOnceMessageV2Extension() != nil ||
		msg.GetEphemeralMessage() != nil {
		return true
	}
	if img := msg.GetImageMessage(); img != nil && (img.GetViewOnce() || checkContextEphemeral(img.GetContextInfo())) {
		return true
	}
	if vid := msg.GetVideoMessage(); vid != nil && (vid.GetViewOnce() || checkContextEphemeral(vid.GetContextInfo())) {
		return true
	}
	if aud := msg.GetAudioMessage(); aud != nil && (aud.GetViewOnce() || checkContextEphemeral(aud.GetContextInfo())) {
		return true
	}
	if doc := msg.GetDocumentMessage(); doc != nil && checkContextEphemeral(doc.GetContextInfo()) {
		return true
	}
	if ext := msg.GetExtendedTextMessage(); ext != nil && checkContextEphemeral(ext.GetContextInfo()) {
		return true
	}

	// Recurse through wrappers
	if e := msg.GetEditedMessage(); e != nil && e.GetMessage() != nil {
		if isViewOnceOrEphemeralRecurse(e.GetMessage(), depth+1) {
			return true
		}
	}
	if d := msg.GetDeviceSentMessage(); d != nil && d.GetMessage() != nil {
		if isViewOnceOrEphemeralRecurse(d.GetMessage(), depth+1) {
			return true
		}
	}
	return false
}

func checkContextEphemeral(ci *waE2E.ContextInfo) bool {
	if ci == nil {
		return false
	}
	return ci.GetExpiration() > 0 || ci.GetEphemeralSettingTimestamp() > 0
}

func (c *Client) getDownloadSem() chan struct{} {
	c.initMu.Lock()
	defer c.initMu.Unlock()
	if c.downloadSem == nil {
		c.downloadSem = make(chan struct{}, MaxConcurrentMediaDownloads)
	}
	return c.downloadSem
}

func (c *Client) acquireDownloadSlot() (func(), error) {
	sem := c.getDownloadSem()
	select {
	case sem <- struct{}{}:
		var once sync.Once
		return func() {
			once.Do(func() {
				<-sem
			})
		}, nil
	default:
		return nil, ErrDownloadOverload
	}
}

func (c *Client) reserveMediaSpace(needed int64) (func(success bool, actualBytes int64), error) {
	c.quotaMu.Lock()
	defer c.quotaMu.Unlock()

	if !c.cacheScanned {
		mediaRoot := filepath.Clean(filepath.Join(c.dataDir, "media"))
		var total int64
		if entries, err := os.ReadDir(mediaRoot); err == nil {
			for _, entry := range entries {
				if !entry.IsDir() {
					if info, err := entry.Info(); err == nil && info.Mode().IsRegular() {
						total += info.Size()
					}
				}
			}
		}
		c.usedCacheBytes = total
		c.cacheScanned = true
	}

	maxQuota := c.maxCacheBytes
	if maxQuota <= 0 {
		maxQuota = MaxMediaCacheBytes
	}

	if c.usedCacheBytes+c.reservedCacheBytes+needed > maxQuota {
		return nil, ErrMediaCacheFull
	}

	c.reservedCacheBytes += needed
	return func(success bool, actualBytes int64) {
		c.quotaMu.Lock()
		defer c.quotaMu.Unlock()
		c.reservedCacheBytes -= needed
		if success && actualBytes > 0 {
			c.usedCacheBytes += actualBytes
		}
	}, nil
}

func (c *Client) removeMediaByMessageID(chatJID, messageID string) {
	mediaRoot := filepath.Clean(filepath.Join(c.dataDir, "media"))
	h := sha256.Sum256([]byte(chatJID + "\x00" + messageID))
	prefix := hex.EncodeToString(h[:])
	entries, err := os.ReadDir(mediaRoot)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) {
			c.safeRemoveMedia(filepath.Join(mediaRoot, entry.Name()))
		}
	}
}

func (c *Client) safeRemoveMedia(path string) {
	if path == "" {
		return
	}
	mediaRoot := filepath.Clean(filepath.Join(c.dataDir, "media"))
	cleanPath := filepath.Clean(path)

	// Validate mediaRoot itself is not a symlink
	if rootFi, err := os.Lstat(mediaRoot); err != nil || rootFi.Mode()&os.ModeSymlink != 0 {
		return
	}

	rel, err := filepath.Rel(mediaRoot, cleanPath)
	if err != nil || strings.HasPrefix(rel, "..") || strings.HasPrefix(rel, "/") || rel == "." {
		return // Outside mediaRoot
	}

	// Verify no directory component in rel is a symlink
	curr := mediaRoot
	parts := strings.Split(rel, string(os.PathSeparator))
	for i := 0; i < len(parts)-1; i++ {
		curr = filepath.Join(curr, parts[i])
		fi, err := os.Lstat(curr)
		if err != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
			return // Parent component is a symlink or invalid
		}
	}

	if fi, err := os.Lstat(cleanPath); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return // Prohibit unlinking symlink targets
		}
		if !fi.Mode().IsRegular() {
			return
		}
		size := fi.Size()
		if err := os.Remove(cleanPath); err == nil {
			c.quotaMu.Lock()
			c.usedCacheBytes -= size
			if c.usedCacheBytes < 0 {
				c.usedCacheBytes = 0
			}
			c.quotaMu.Unlock()
		}
	}
}

// onMessage persists a live incoming or outgoing message.
func (c *Client) onMessage(ctx context.Context, evt *events.Message) {
	chatJID := evt.Info.Chat.ToNonAD().String()
	if isNotAConversation(chatJID) {
		return
	}

	// Process revoke target by deleting original message, FTS entry, and media
	if pm := evt.Message.GetProtocolMessage(); pm != nil && pm.GetType() == waE2E.ProtocolMessage_REVOKE {
		targetID := ""
		if key := pm.GetKey(); key != nil {
			targetID = key.GetID()
		}
		if targetID != "" {
			c.mediaPubRevokeMu.Lock()
			mediaPath, err := c.st.DeleteMessage(ctx, chatJID, targetID)
			if err == nil && mediaPath != "" {
				c.safeRemoveMedia(mediaPath)
			}
			c.removeMediaByMessageID(chatJID, targetID)
			c.mediaPubRevokeMu.Unlock()
		}
		return
	}

	// Strictly suppress view-once and ephemeral payloads from ingestion
	if isViewOnceOrEphemeral(evt.Message) {
		return
	}

	sm := store.StoredMessage{Message: store.Message{
		ID:        evt.Info.ID,
		ChatJID:   chatJID,
		SenderJID: evt.Info.Sender.ToNonAD().String(),
		Content:   ExtractText(evt.Message),
		Timestamp: evt.Info.Timestamp,
		IsFromMe:  evt.Info.IsFromMe,
		MsgType:   messageType(evt.Message),
	}}
	fillMedia(&sm, evt.Message)

	if err := c.st.UpsertMessage(ctx, sm); err != nil {
		return
	}

	// Only an INBOUND message may name a chat.
	//
	// evt.Info.PushName is the sender's name, which on a message you sent is
	// your own. Writing it unconditionally renamed every conversation after
	// your own name the moment you replied, so a thread with one person
	// silently became a thread apparently with yourself. The bug was invisible
	// on chats that had a contact record to fall back on and obvious on the
	// ones that did not.
	//
	// An empty name never overwrites a stored one, so leaving it blank here
	// keeps whatever the chat already had.
	name := ""
	if !evt.Info.IsFromMe && !evt.Info.IsGroup {
		name = evt.Info.PushName
	}
	_ = c.st.UpsertChat(ctx, store.Chat{
		JID:      chatJID,
		Name:     name,
		IsGroup:  evt.Info.IsGroup,
		LastSeen: evt.Info.Timestamp,
	})

	isVoice := evt.Message.GetAudioMessage() != nil

	// Synchronously enqueue voice note without spawning goroutines or downloading
	if isVoice && c.sttQueue != nil {
		c.enqueueVoiceNote(evt)
	}

	if evt.Info.PushName != "" && !evt.Info.IsFromMe {
		_ = c.st.UpsertContact(ctx, store.Contact{
			JID:      evt.Info.Sender.ToNonAD().String(),
			PushName: evt.Info.PushName,
		})
	}
}

// ExtractText pulls readable text out of any message shape.
//
// WhatsApp carries text in a different field for every message kind, and a
// caption counts as text. Missing one of these is how history ends up full of
// blank rows.
func ExtractText(msg *waE2E.Message) string {
	if msg == nil || isViewOnceOrEphemeral(msg) {
		return ""
	}
	switch {
	case msg.GetConversation() != "":
		return msg.GetConversation()
	case msg.GetExtendedTextMessage().GetText() != "":
		return msg.GetExtendedTextMessage().GetText()
	case msg.GetImageMessage().GetCaption() != "":
		return msg.GetImageMessage().GetCaption()
	case msg.GetVideoMessage().GetCaption() != "":
		return msg.GetVideoMessage().GetCaption()
	case msg.GetDocumentMessage().GetCaption() != "":
		return msg.GetDocumentMessage().GetCaption()
	case msg.GetDocumentMessage().GetFileName() != "":
		return msg.GetDocumentMessage().GetFileName()
	}

	// Wrapped payloads. An edited message hides the real body a level down.
	// View-once and ephemeral payloads are strictly omitted.
	if e := msg.GetEditedMessage(); e != nil {
		return ExtractText(e.GetMessage())
	}
	if d := msg.GetDeviceSentMessage(); d != nil {
		return ExtractText(d.GetMessage())
	}
	if r := msg.GetReactionMessage(); r != nil {
		return r.GetText()
	}

	// A media message with no caption has no text at all, and storing it as an
	// empty string makes it invisible to a reader: the model sees a message
	// that appears to say nothing rather than a photo that carried no words.
	// A short placeholder says what arrived without inventing content.
	switch {
	case msg.GetImageMessage() != nil:
		return "[image]"
	case msg.GetVideoMessage() != nil:
		return "[video]"
	case msg.GetAudioMessage() != nil:
		if msg.GetAudioMessage().GetPTT() {
			return "[voice note]"
		}
		return "[audio]"
	case msg.GetStickerMessage() != nil:
		return "[sticker]"
	case msg.GetDocumentMessage() != nil:
		return "[document]"
	case msg.GetLocationMessage() != nil:
		return "[location]"
	case msg.GetLiveLocationMessage() != nil:
		return "[live location]"
	case msg.GetContactMessage() != nil:
		return "[contact card]"
	case msg.GetContactsArrayMessage() != nil:
		return "[contact cards]"
	case msg.GetPollCreationMessageV3() != nil:
		return "[poll]"
	case msg.GetPollUpdateMessage() != nil:
		return "[poll vote]"
	case msg.GetProtocolMessage() != nil:
		if msg.GetProtocolMessage().GetType() == waE2E.ProtocolMessage_REVOKE {
			return "[message deleted]"
		}
		return ""
	case msg.GetGroupInviteMessage() != nil:
		return "[group invite]"
	case msg.GetProductMessage() != nil:
		return "[product]"
	case msg.GetOrderMessage() != nil:
		return "[order]"
	case msg.GetListMessage() != nil:
		return msg.GetListMessage().GetDescription()
	case msg.GetButtonsMessage() != nil:
		return msg.GetButtonsMessage().GetContentText()
	case msg.GetTemplateMessage() != nil:
		return "[template message]"
	case msg.GetInteractiveMessage() != nil:
		return "[interactive message]"
	case msg.GetPtvMessage() != nil:
		return "[video note]"
	}

	// Nothing recognized. Storing an empty string here is silent data loss:
	// the reader sees a message that appears to say nothing, with no way to
	// tell that something was dropped. Naming the payload is honest about it
	// and makes the gap findable rather than invisible.
	if name := payloadName(msg); name != "" {
		return "[" + name + "]"
	}
	return "[unsupported message]"
}

// payloadName reports which field of the protobuf actually carried something,
// so an unhandled type says what it was instead of nothing.
func payloadName(msg *waE2E.Message) string {
	r := msg.ProtoReflect()
	var found string
	r.Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		n := string(fd.Name())
		if n == "messageContextInfo" || n == "deviceSentMessage" {
			return true
		}
		found = n
		return false
	})
	return found
}

func messageType(msg *waE2E.Message) string {
	switch {
	case msg.GetImageMessage() != nil:
		return "image"
	case msg.GetVideoMessage() != nil:
		return "video"
	case msg.GetAudioMessage() != nil:
		return "audio"
	case msg.GetDocumentMessage() != nil:
		return "document"
	case msg.GetStickerMessage() != nil:
		return "sticker"
	case msg.GetLocationMessage() != nil:
		return "location"
	case msg.GetContactMessage() != nil:
		return "contact"
	case msg.GetReactionMessage() != nil:
		return "reaction"
	}
	return "text"
}

// fillMedia records what is needed to fetch an attachment later, without
// downloading it now.
func fillMedia(sm *store.StoredMessage, msg *waE2E.Message) {
	if isViewOnceOrEphemeral(msg) {
		return
	}
	switch {
	case msg.GetImageMessage() != nil:
		m := msg.GetImageMessage()
		sm.MediaType, sm.MediaURL, sm.MediaKey = "image", m.GetURL(), m.GetMediaKey()
		sm.FileSHA256, sm.FileEncSHA, sm.FileLength = m.GetFileSHA256(), m.GetFileEncSHA256(), m.GetFileLength()
	case msg.GetVideoMessage() != nil:
		m := msg.GetVideoMessage()
		sm.MediaType, sm.MediaURL, sm.MediaKey = "video", m.GetURL(), m.GetMediaKey()
		sm.FileSHA256, sm.FileEncSHA, sm.FileLength = m.GetFileSHA256(), m.GetFileEncSHA256(), m.GetFileLength()
	case msg.GetAudioMessage() != nil:
		m := msg.GetAudioMessage()
		sm.MediaType, sm.MediaURL, sm.MediaKey = "audio", m.GetURL(), m.GetMediaKey()
		sm.FileSHA256, sm.FileEncSHA, sm.FileLength = m.GetFileSHA256(), m.GetFileEncSHA256(), m.GetFileLength()
	case msg.GetDocumentMessage() != nil:
		m := msg.GetDocumentMessage()
		sm.MediaType, sm.MediaURL, sm.MediaKey = "document", m.GetURL(), m.GetMediaKey()
		sm.FileSHA256, sm.FileEncSHA, sm.FileLength = m.GetFileSHA256(), m.GetFileEncSHA256(), m.GetFileLength()
		sm.Filename = m.GetFileName()
	}
}

// onHistorySync persists a batch of backfilled conversations.
//
// Chats are written before their messages. The order is not guaranteed by
// WhatsApp, and the store deliberately does not enforce the reference, so a
// message that still arrives first is kept rather than dropped.
func (c *Client) onHistorySync(ctx context.Context, evt *events.HistorySync) {
	c.setSyncing(true)
	defer c.setSyncing(false)

	for _, conv := range evt.Data.GetConversations() {
		chatJID, err := types.ParseJID(conv.GetID())
		if err != nil {
			continue
		}
		jid := chatJID.ToNonAD().String()
		if isNotAConversation(jid) {
			continue
		}

		var newest time.Time
		for _, hm := range conv.GetMessages() {
			wm := hm.GetMessage()
			if wm == nil {
				continue
			}
			msg := wm.GetMessage()
			if msg == nil {
				continue
			}

			// Handle history revoke controls as well as live
			if pm := msg.GetProtocolMessage(); pm != nil && pm.GetType() == waE2E.ProtocolMessage_REVOKE {
				targetID := ""
				if key := pm.GetKey(); key != nil {
					targetID = key.GetID()
				}
				if targetID != "" {
					c.mediaPubRevokeMu.Lock()
					mediaPath, err := c.st.DeleteMessage(ctx, jid, targetID)
					if err == nil && mediaPath != "" {
						c.safeRemoveMedia(mediaPath)
					}
					c.removeMediaByMessageID(jid, targetID)
					c.mediaPubRevokeMu.Unlock()
				}
				continue
			}

			if isViewOnceOrEphemeral(msg) {
				continue
			}
			info := wm.GetKey()
			ts := time.Unix(int64(wm.GetMessageTimestamp()), 0)
			if ts.After(newest) {
				newest = ts
			}

			sender := info.GetParticipant()
			if sender == "" && !info.GetFromMe() {
				sender = jid
			}

			sm := store.StoredMessage{Message: store.Message{
				ID:        info.GetID(),
				ChatJID:   jid,
				SenderJID: sender,
				Content:   ExtractText(msg),
				Timestamp: ts,
				IsFromMe:  info.GetFromMe(),
				MsgType:   messageType(msg),
			}}
			fillMedia(&sm, msg)
			_ = c.st.UpsertMessage(ctx, sm)
		}

		_ = c.st.UpsertChat(ctx, store.Chat{
			JID:      jid,
			Name:     conv.GetName(),
			IsGroup:  strings.HasSuffix(jid, "@g.us"),
			LastSeen: newest,
		})
	}
}

// SendText sends a plain text message and returns the new message ID.
// In this hardened read-only fork, it unconditionally rejects sends.
func (c *Client) SendText(ctx context.Context, chatJID, text string) (string, error) {
	return "", ErrReadOnly
}

// enqueueVoiceNote synchronously enqueues a voice note for download and transcription.
// Single worker performs download, inference, and persistence sequentially with backpressure,
// avoiding RAM and download storms.
func (c *Client) enqueueVoiceNote(evt *events.Message) {
	if c.sttQueue == nil {
		return
	}

	audio := evt.Message.GetAudioMessage()
	if audio == nil {
		return
	}

	declaredSize := audio.GetFileLength()
	if declaredSize > transcribe.DefaultMaxAudioBytes {
		fmt.Fprintf(c.out, "transcription: voice note %s declared size %d exceeds limit\n", evt.Info.ID, declaredSize)
		return
	}

	chatJID := evt.Info.Chat.ToNonAD().String()
	msgID := evt.Info.ID

	job := &transcribe.Job{
		ChatJID:      chatJID,
		MessageID:    msgID,
		Filename:     "voice-note.ogg",
		DeclaredSize: declaredSize,
		Download: func(ctx context.Context) ([]byte, error) {
			releaseSlot, err := c.acquireDownloadSlot()
			if err != nil {
				return nil, err
			}
			defer releaseSlot()

			if c.downloader != nil {
				return c.downloader.Download(ctx, audio)
			}
			if c.wm != nil {
				return c.wm.Download(ctx, audio)
			}
			return nil, errors.New("downloader is nil")
		},
		OnSuccess: func(ctx context.Context, text string) error {
			if strings.TrimSpace(text) == "" {
				return nil
			}
			return c.st.UpdateTranscript(ctx, chatJID, msgID, text)
		},
	}

	if err := c.sttQueue.Submit(context.Background(), job); err != nil {
		fmt.Fprintf(c.out, "transcription queue submit for %s: %v\n", msgID, err)
	}
}

// SyncContacts copies WhatsApp's own contact list into the store.
//
// This is the fix for chats showing no name. History sync delivers messages
// and chat ids, but for a one to one chat it sends no name, because the name
// is not a property of the conversation. It lives in the address book, which
// whatsmeow keeps in its own store and which nothing here was reading.
//
// Without this, a freshly paired account shows hundreds of unnamed chats even
// though the names were sitting on disk the whole time.
func (c *Client) SyncContacts(ctx context.Context) (int, error) {
	all, err := c.wm.Store.Contacts.GetAllContacts(ctx)
	if err != nil {
		return 0, fmt.Errorf("read contact store: %w", err)
	}

	n := 0
	for jid, info := range all {
		if !info.Found {
			continue
		}
		contact := store.Contact{
			PushName:     info.PushName,
			BusinessName: info.BusinessName,
			FullName:     firstNonEmpty(info.FullName, info.FirstName),
		}

		contact.JID = jid.ToNonAD().String()
		if err := c.st.UpsertContact(ctx, contact); err != nil {
			continue
		}
		n++

		// Deliberately NOT duplicating this row under a LID.
		//
		// An earlier version did, using GetLIDForPN, and it wrote the wrong
		// person's name against a LID: two contacts resolved to the same
		// identifier and one overwrote the other, so a real conversation was
		// labeled with somebody else's name. Mislabelling who you are talking
		// to is worse than showing no name at all.
		//
		// LID chats get their name from history sync, which carries the
		// correct one, and from push names captured on arrival.
	}
	return n, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// isNotAConversation filters out the pseudo chats WhatsApp delivers alongside
// real ones.
//
// status@broadcast is the status feed. Stored as a chat, every status anyone
// posts reads like a message they sent you, which is misleading rather than
// merely untidy.
func isNotAConversation(jid string) bool {
	switch jid {
	case "status@broadcast", "", "0@s.whatsapp.net":
		return true
	}
	return strings.HasSuffix(jid, "@newsletter")
}
