package transcribe

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostedProvidersRejectedEvenWithKey(t *testing.T) {
	for _, p := range []string{"groq", "openai", "elevenlabs"} {
		_, err := New(Config{Provider: p, APIKey: "dummy-key-present"})
		if err == nil {
			t.Fatalf("provider %s should be rejected in hardened fork even with an API key", p)
		}
		if !strings.Contains(err.Error(), "read-only") &&
			!strings.Contains(err.Error(), "rejected") &&
			!strings.Contains(err.Error(), "only 'local'") &&
			!strings.Contains(err.Error(), "hosted providers are disabled") {
			t.Errorf("provider %s error should indicate hosted providers are rejected, got: %v", p, err)
		}
	}
}

func TestExplicitBinarySelectionRequiresValidPath(t *testing.T) {
	// 1. Missing binary must be rejected (no ambient PATH or WHISPER_BIN)
	_, err := New(Config{Provider: "local", Binary: ""})
	if err == nil {
		t.Fatal("expected error for missing --transcribe-bin")
	}
	if !strings.Contains(err.Error(), "--transcribe-bin") {
		t.Errorf("expected error mentioning --transcribe-bin, got: %v", err)
	}

	// 2. Relative path must be rejected
	_, err = New(Config{Provider: "local", Binary: "relative/path/whisper"})
	if err == nil {
		t.Fatal("expected error for relative binary path")
	}
	if !strings.Contains(err.Error(), "absolute path") {
		t.Errorf("expected error mentioning absolute path, got: %v", err)
	}

	// 3. Nonexistent absolute path must be rejected
	_, err = New(Config{Provider: "local", Binary: "/nonexistent/path/to/whisper_adapter"})
	if err == nil {
		t.Fatal("expected error for nonexistent explicit adapter binary")
	}
	if !strings.Contains(err.Error(), "not found") && !strings.Contains(err.Error(), "no such file") {
		t.Errorf("unexpected error for nonexistent binary: %v", err)
	}
}

func TestLimitBufferReturnsOriginalLenOnTruncate(t *testing.T) {
	lb := &limitBuffer{limit: 10}
	input := []byte("0123456789extra_bytes")
	n, err := lb.Write(input)
	if err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if n != len(input) {
		t.Fatalf("expected Write to return %d to prevent io.ErrShortWrite, got %d", len(input), n)
	}
	if lb.String() != "0123456789" {
		t.Fatalf("expected truncated content '0123456789', got %q", lb.String())
	}
}

func TestSubprocessOversizedTranscriptRejected(t *testing.T) {
	tmpDir := t.TempDir()
	mockBin := filepath.Join(tmpDir, "giant_adapter.sh")
	// Writes 1.5MB of text
	script := `#!/bin/sh
out_dir=""
while [ $# -gt 0 ]; do
    if [ "$1" = "--output_dir" ]; then
        out_dir="$2"
        shift 2
    else
        shift
    fi
done
if [ -n "$out_dir" ]; then
    dd if=/dev/zero bs=1024 count=1500 2>/dev/null | tr '\0' 'A' > "$out_dir/audio.txt"
fi
exit 0
`
	if err := os.WriteFile(mockBin, []byte(script), 0o700); err != nil {
		t.Fatalf("write mock: %v", err)
	}

	tr, err := New(Config{
		Provider: "local",
		Binary:   mockBin,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = tr.Transcribe(context.Background(), []byte("audio"), "audio.ogg")
	if err == nil {
		t.Fatal("expected error for oversized transcript, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds maximum allowed size") {
		t.Errorf("expected error mentioning maximum allowed size, got: %v", err)
	}
}

func TestSubprocessSanitizedEnvAndBoundedOutput(t *testing.T) {
	// Create a temporary mock adapter script in Python or bash
	tmpDir := t.TempDir()
	mockBin := filepath.Join(tmpDir, "mock_adapter.sh")
	script := `#!/bin/sh
# Check if forbidden env vars leaked
if [ -n "$SECRET_API_KEY" ]; then
    echo "SECRET_API_KEY LEAKED" >&2
    exit 2
fi
# Find --output_dir
out_dir=""
while [ $# -gt 0 ]; do
    if [ "$1" = "--output_dir" ]; then
        out_dir="$2"
        shift 2
    else
        shift
    fi
done
if [ -n "$out_dir" ]; then
    echo "mock transcript text" > "$out_dir/audio.txt"
fi
exit 0
`
	if err := os.WriteFile(mockBin, []byte(script), 0o700); err != nil {
		t.Fatalf("write mock: %v", err)
	}

	// Set a secret environment variable in parent process
	t.Setenv("SECRET_API_KEY", "super-secret-token")
	t.Setenv("OPENAI_API_KEY", "sk-proj-secret")

	tr, err := New(Config{
		Provider: "local",
		Binary:   mockBin,
	})
	if err != nil {
		t.Fatalf("New local transcriber: %v", err)
	}

	ctx := context.Background()
	text, err := tr.Transcribe(ctx, []byte("fake-audio"), "audio.ogg")
	if err != nil {
		t.Fatalf("Transcribe failed: %v", err)
	}
	if text != "mock transcript text" {
		t.Errorf("expected 'mock transcript text', got %q", text)
	}
}
