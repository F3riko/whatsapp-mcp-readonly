package transcribe

import (
	"strings"
	"testing"
)

func TestOffByDefault(t *testing.T) {
	for _, p := range []string{"", "off", "none"} {
		got, err := New(Config{Provider: p})
		if err != nil {
			t.Fatalf("provider %q: %v", p, err)
		}
		if got != nil {
			t.Errorf("provider %q should disable transcription, got %v", p, got.Name())
		}
	}
}

func TestUnknownProviderRejection(t *testing.T) {
	_, err := New(Config{Provider: "assemblyai"})
	if err == nil {
		t.Fatal("want an error for an unknown provider")
	}
	if !strings.Contains(err.Error(), "local") {
		t.Errorf("the error should list 'local' as valid, got %q", err)
	}
}

func TestHostedProvidersAreRejected(t *testing.T) {
	for _, p := range []string{"groq", "openai", "elevenlabs"} {
		if _, err := New(Config{Provider: p, APIKey: "test-key"}); err == nil {
			t.Errorf("%s should be rejected in read-only hardened fork", p)
		}
	}
}
