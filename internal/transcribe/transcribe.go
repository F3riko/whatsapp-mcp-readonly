// Package transcribe turns voice notes into searchable text.
// In this hardened read-only fork, only local transcription is permitted.
package transcribe

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Transcriber turns audio bytes into text.
type Transcriber interface {
	// Transcribe returns the spoken text. filename carries the extension so a
	// provider can tell what container it was given.
	Transcribe(ctx context.Context, audio []byte, filename string) (string, error)
	// Name is what gets reported in session_status.
	Name() string
}

// Config selects and configures a provider.
type Config struct {
	// Provider is one of local or empty for off.
	Provider string
	// APIKey is rejected in this fork.
	APIKey string
	// Model overrides the provider default.
	Model string
	// Language is an optional ISO-639-1 hint, for example "es". Leave empty to
	// let the provider detect it.
	Language string
	// Binary is the explicit pinned absolute path to the local whisper executable or adapter script.
	Binary string
}

const (
	MaxTranscriptBytes = 1 << 20 // 1MB maximum transcript text
	MaxStderrBytes     = 64 * 1024
)

// New builds a Transcriber, or nil if transcription is off.
func New(cfg Config) (Transcriber, error) {
	p := strings.ToLower(strings.TrimSpace(cfg.Provider))
	switch p {
	case "", "off", "none":
		return nil, nil

	case "local", "whisper":
		bin := strings.TrimSpace(cfg.Binary)
		if bin == "" {
			return nil, fmt.Errorf("local transcription requires an explicit pinned executable path via --transcribe-bin (no ambient PATH or WHISPER_BIN fallback permitted)")
		}
		if !filepath.IsAbs(bin) {
			return nil, fmt.Errorf("local transcription binary %q must be an absolute path", bin)
		}

		info, err := os.Stat(bin)
		if err != nil {
			return nil, fmt.Errorf("local transcription binary %q not found: %w", bin, err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("local transcription binary %q is a directory", bin)
		}
		if info.Mode().Perm()&0o111 == 0 {
			return nil, fmt.Errorf("local transcription binary %q is not executable", bin)
		}

		return &localWhisper{binary: bin, model: cfg.Model, language: cfg.Language}, nil

	case "groq", "openai", "elevenlabs", "eleven":
		return nil, fmt.Errorf("hosted transcription provider %q rejected; only 'local' transcription is permitted in this hardened fork", cfg.Provider)
	}

	return nil, fmt.Errorf("unknown transcription provider %q. Only 'local' is supported in this hardened fork", cfg.Provider)
}

// ── Local whisper ──

type localWhisper struct {
	binary   string
	model    string
	language string
}

func (l *localWhisper) Name() string { return "local/" + filepath.Base(l.binary) }

type limitBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *limitBuffer) Write(p []byte) (n int, err error) {
	origLen := len(p)
	rem := b.limit - b.buf.Len()
	if rem <= 0 {
		return origLen, nil
	}
	if len(p) > rem {
		p = p[:rem]
	}
	_, err = b.buf.Write(p)
	return origLen, err
}

func (b *limitBuffer) String() string {
	return b.buf.String()
}

func (l *localWhisper) Transcribe(ctx context.Context, audio []byte, filename string) (string, error) {
	// Bounded per-call timeout
	callCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	dir, err := os.MkdirTemp("", "whatsapp-mcp-stt-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)

	ext := filepath.Ext(filename)
	if ext == "" {
		ext = ".ogg"
	}
	in := filepath.Join(dir, "audio"+ext)
	if err := os.WriteFile(in, audio, 0o600); err != nil {
		return "", err
	}

	args := []string{in, "--output_format", "txt", "--output_dir", dir}
	if l.model != "" {
		args = append(args, "--model", l.model)
	}
	if l.language != "" {
		args = append(args, "--language", l.language)
	}

	cmd := exec.CommandContext(callCtx, l.binary, args...)
	cmd.WaitDelay = 5 * time.Second
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return cmd.Process.Kill()
		}
		return nil
	}

	// Controlled clean environment: strictly sanitize to prevent API key leakage
	cleanPath := "/usr/bin:/bin:/usr/local/bin"
	if p := os.Getenv("PATH"); p != "" {
		cleanPath = p
	}
	cmd.Env = []string{
		"PATH=" + cleanPath,
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
		"TMPDIR=" + dir,
		"HF_HUB_OFFLINE=1",
		"TRANSFORMERS_OFFLINE=1",
	}

	stderr := &limitBuffer{limit: MaxStderrBytes}
	cmd.Stderr = stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s failed: %v: %s", l.binary, err, strings.TrimSpace(stderr.String()))
	}

	outPath := filepath.Join(dir, "audio.txt")
	f, err := os.Open(outPath)
	if err != nil {
		return "", fmt.Errorf("%s produced no transcript: %w", l.binary, err)
	}
	defer f.Close()

	// Bound output read and reject oversized transcript
	out, err := io.ReadAll(io.LimitReader(f, MaxTranscriptBytes+1))
	if err != nil {
		return "", fmt.Errorf("read transcript: %w", err)
	}
	if len(out) > MaxTranscriptBytes {
		return "", fmt.Errorf("transcript output exceeds maximum allowed size of %d bytes", MaxTranscriptBytes)
	}
	return strings.TrimSpace(string(out)), nil
}
