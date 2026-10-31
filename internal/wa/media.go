package wa

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"go.mau.fi/whatsmeow"

	"github.com/thenavidm/whatsapp-mcp/internal/store"
)

var (
	ErrDownloadOverload = errors.New("media download admission budget exceeded: server is overloaded")
	ErrMediaCacheFull   = errors.New("media cache quota exceeded")
)

// mediaDownloadable adapts stored media metadata back into the shape whatsmeow
// needs to fetch and decrypt an attachment.
type mediaDownloadable struct {
	directPath string
	mediaKey   []byte
	fileSHA    []byte
	fileEncSHA []byte
	fileLength uint64
	mediaType  whatsmeow.MediaType
}

func (d *mediaDownloadable) GetDirectPath() string    { return d.directPath }
func (d *mediaDownloadable) GetMediaKey() []byte      { return d.mediaKey }
func (d *mediaDownloadable) GetFileSHA256() []byte    { return d.fileSHA }
func (d *mediaDownloadable) GetFileEncSHA256() []byte { return d.fileEncSHA }
func (d *mediaDownloadable) GetFileLength() uint64    { return d.fileLength }

const (
	MaxMediaDownloadBytes = 50 * 1024 * 1024 // 50MB

	// MaxConcurrentMediaDownloads is the shared admission limit across MCP media and STT downloads.
	MaxConcurrentMediaDownloads = 2

	// MaxMediaCacheBytes is the conservative default disk quota for the media cache (500MB).
	MaxMediaCacheBytes = 500 * 1024 * 1024
)

type boundedMediaTransport struct {
	base  http.RoundTripper
	limit int64
}

type boundedReadCloser struct {
	r     io.Reader
	c     io.Closer
	read  int64
	limit int64
}

func (b *boundedReadCloser) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.read += int64(n)
	if b.read > b.limit {
		return n, fmt.Errorf("media wire download exceeded limit of %d bytes", b.limit)
	}
	return n, err
}

func (b *boundedReadCloser) Close() error {
	return b.c.Close()
}

func (t *boundedMediaTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp != nil && resp.Body != nil {
		resp.Body = &boundedReadCloser{
			r:     resp.Body,
			c:     resp.Body,
			limit: t.limit,
		}
	}
	return resp, nil
}

func newBoundedMediaHTTPClient(limit int64) *http.Client {
	return &http.Client{
		Transport: &boundedMediaTransport{
			base:  http.DefaultTransport,
			limit: limit,
		},
		Timeout: 2 * time.Minute,
	}
}

func ensureSecureMediaDir(mediaRoot string) error {
	return store.EnsureSecureDir(mediaRoot)
}

func isSecureCacheHit(mediaRoot, path string) bool {
	clean := filepath.Clean(path)
	if filepath.Dir(clean) != mediaRoot {
		return false
	}
	if rootFi, err := os.Lstat(mediaRoot); err != nil || rootFi.Mode()&os.ModeSymlink != 0 || !rootFi.IsDir() {
		return false
	}
	fi, err := os.Lstat(clean)
	if err != nil {
		return false
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		return false
	}
	if stat, ok := fi.Sys().(*syscall.Stat_t); ok {
		if stat.Uid != uint32(os.Geteuid()) {
			return false
		}
	}
	return true
}

func (c *Client) resolveMediaPath(chatJID, messageID, filename, mediaType string) string {
	mediaRoot := filepath.Clean(filepath.Join(c.dataDir, "media"))
	h := sha256.Sum256([]byte(chatJID + "\x00" + messageID))
	hashHex := hex.EncodeToString(h[:])
	ext := extensionFor(mediaType)
	return filepath.Clean(filepath.Join(mediaRoot, hashHex+ext))
}

// DownloadMedia fetches and decrypts an attachment, writes it safely into the data
// directory with path containment and uniqueness, and returns the path.
func (c *Client) DownloadMedia(ctx context.Context, chatJID, messageID string) (string, error) {
	m, err := c.st.GetMedia(ctx, chatJID, messageID)
	if err != nil {
		return "", err
	}
	if m.MediaType == "" {
		return "", fmt.Errorf("message %s has no attachment", messageID)
	}

	mediaRoot := filepath.Clean(filepath.Join(c.dataDir, "media"))
	if err := ensureSecureMediaDir(mediaRoot); err != nil {
		return "", err
	}

	targetPath := c.resolveMediaPath(chatJID, messageID, m.Filename, m.MediaType)

	// Check if already downloaded and valid within mediaRoot without symlinks
	if m.MediaPath != "" && isSecureCacheHit(mediaRoot, m.MediaPath) {
		return m.MediaPath, nil
	}

	if isSecureCacheHit(mediaRoot, targetPath) {
		return targetPath, nil
	}

	// Cap declared size before download
	if m.FileLength > MaxMediaDownloadBytes {
		return "", fmt.Errorf("attachment size %d exceeds limit of %d bytes", m.FileLength, MaxMediaDownloadBytes)
	}

	bytesNeeded := int64(m.FileLength)
	if bytesNeeded <= 0 {
		bytesNeeded = MaxMediaDownloadBytes
	}

	// Reserve disk budget safely before download
	releaseQuota, err := c.reserveMediaSpace(bytesNeeded)
	if err != nil {
		return "", fmt.Errorf("media cache budget check: %w", err)
	}
	var (
		downloadSuccess bool
		actualFileSize  int64
	)
	defer func() {
		releaseQuota(downloadSuccess, actualFileSize)
	}()

	// Acquire shared admission slot before download
	releaseSlot, err := c.acquireDownloadSlot()
	if err != nil {
		return "", fmt.Errorf("media download admission: %w", err)
	}
	defer releaseSlot()

	var kind whatsmeow.MediaType
	switch m.MediaType {
	case "image":
		kind = whatsmeow.MediaImage
	case "video":
		kind = whatsmeow.MediaVideo
	case "audio":
		kind = whatsmeow.MediaAudio
	default:
		kind = whatsmeow.MediaDocument
	}

	dl := c.downloader
	if dl == nil && c.wm != nil {
		dl = c.wm
	}
	if dl == nil {
		return "", fmt.Errorf("no downloader configured")
	}

	data, err := dl.Download(ctx, &mediaDownloadable{
		directPath: directPath(m.MediaURL),
		mediaKey:   m.MediaKey,
		fileSHA:    m.FileSHA256,
		fileEncSHA: m.FileEncSHA,
		fileLength: m.FileLength,
		mediaType:  kind,
	})
	if err != nil {
		return "", fmt.Errorf("download failed: %w", err)
	}

	if int64(len(data)) > MaxMediaDownloadBytes {
		return "", fmt.Errorf("downloaded data size %d exceeds limit of %d bytes", len(data), MaxMediaDownloadBytes)
	}

	// Write to exclusive random temporary file in mediaRoot
	tmpFile, err := os.CreateTemp(mediaRoot, ".tmp-media-*")
	if err != nil {
		return "", fmt.Errorf("create temp media file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() {
		_ = os.Remove(tmpPath)
	}()

	if err := tmpFile.Chmod(0o600); err != nil {
		_ = tmpFile.Close()
		return "", fmt.Errorf("chmod temp media file: %w", err)
	}

	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		return "", fmt.Errorf("write media: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return "", fmt.Errorf("close temp media: %w", err)
	}

	// Release admission slot early before serialization lock
	releaseSlot()

	// Short per-client cache/revoke serialization around final publication + SetMediaPath
	c.mediaPubRevokeMu.Lock()
	defer c.mediaPubRevokeMu.Unlock()

	// Check IsRevoked at publication
	revoked, err := c.st.IsRevoked(ctx, chatJID, messageID)
	if err != nil {
		return "", fmt.Errorf("check revoked status: %w", err)
	}
	if revoked {
		return "", fmt.Errorf("message %s in chat %s was revoked during download", messageID, chatJID)
	}

	// Controlled atomic publication with no-overwrite semantics:
	// Revalidate existing targets as regular secure cache hits; otherwise fail;
	// no overwrite fallback or check-then-Rename.
	if err := os.Link(tmpPath, targetPath); err != nil {
		if os.IsExist(err) {
			if !isSecureCacheHit(mediaRoot, targetPath) {
				return "", fmt.Errorf("target media path %q exists and is not a valid regular cache file", targetPath)
			}
		} else {
			return "", fmt.Errorf("publish media file: %w", err)
		}
	}

	if err := c.st.SetMediaPath(ctx, chatJID, messageID, targetPath); err != nil {
		c.safeRemoveMedia(targetPath)
		return "", fmt.Errorf("record media path: %w", err)
	}

	downloadSuccess = true
	actualFileSize = int64(len(data))
	return targetPath, nil
}

// SendFile sends a local file as an image, video, document or audio message.
// In this hardened read-only fork, it unconditionally rejects sends before reading any file or touching network.
func (c *Client) SendFile(ctx context.Context, chatJID, path, caption string) (string, error) {
	return "", ErrReadOnly
}

// SendVoiceBytes sends Ogg Opus audio already in memory as a voice note.
// In this hardened read-only fork, it unconditionally rejects sends before touching network.
func (c *Client) SendVoiceBytes(ctx context.Context, chatJID string, ogg []byte) (string, error) {
	return "", ErrReadOnly
}

// SendVoiceNote sends an Ogg Opus file as a playable voice message.
// In this hardened read-only fork, it unconditionally rejects sends before reading any file or touching network.
func (c *Client) SendVoiceNote(ctx context.Context, chatJID, path string) (string, error) {
	return "", ErrReadOnly
}

// directPath strips the CDN host off a stored media URL, because whatsmeow
// wants the path portion rather than the full URL.
func directPath(url string) string {
	if i := strings.Index(url, ".net/"); i != -1 {
		if q := strings.Index(url[i+4:], "?"); q != -1 {
			return url[i+4 : i+4+q]
		}
		return url[i+4:]
	}
	return url
}

func kindFor(path string) (string, whatsmeow.MediaType) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg", ".png", ".webp", ".gif":
		return "image", whatsmeow.MediaImage
	case ".mp4", ".mov", ".avi", ".mkv", ".webm":
		return "video", whatsmeow.MediaVideo
	case ".ogg", ".mp3", ".m4a", ".wav", ".aac", ".opus":
		return "audio", whatsmeow.MediaAudio
	}
	return "document", whatsmeow.MediaDocument
}

func mimeFor(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".mp4":
		return "video/mp4"
	case ".mov":
		return "video/quicktime"
	case ".ogg", ".opus":
		return "audio/ogg; codecs=opus"
	case ".mp3":
		return "audio/mpeg"
	case ".m4a":
		return "audio/mp4"
	case ".wav":
		return "audio/wav"
	case ".pdf":
		return "application/pdf"
	}
	return "application/octet-stream"
}

func extensionFor(mediaType string) string {
	switch mediaType {
	case "image":
		return ".jpg"
	case "video":
		return ".mp4"
	case "audio":
		return ".ogg"
	}
	return ".bin"
}

// SendAudioBytes sends audio held in memory as an ordinary audio message.
// In this hardened read-only fork, it unconditionally rejects sends before touching network.
func (c *Client) SendAudioBytes(ctx context.Context, chatJID string, data []byte, filename string) (string, error) {
	return "", ErrReadOnly
}
