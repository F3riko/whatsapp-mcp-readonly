package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestMainRejectsSendAgentVoiceAndHostedFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"allow-send", []string{"--allow-send"}},
		{"send-to", []string{"--send-to", "someone@s.whatsapp.net"}},
		{"agent-chat", []string{"--agent-chat", "someone@s.whatsapp.net"}},
		{"agent-command", []string{"--agent-command", "echo"}},
		{"agent-args", []string{"--agent-args", "-p"}},
		{"agent-dir", []string{"--agent-dir", "/tmp"}},
		{"voice", []string{"--voice", "elevenlabs"}},
		{"voice-id", []string{"--voice-id", "some-voice"}},
		{"transcribe-openai", []string{"--transcribe", "openai"}},
		{"transcribe-groq", []string{"--transcribe", "groq"}},
		{"transcribe-elevenlabs", []string{"--transcribe", "elevenlabs"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()

			tmpDir := t.TempDir()
			fullArgs := append([]string{"--data-dir", tmpDir}, tc.args...)
			err := runArgs(ctx, fullArgs)
			if err == nil {
				t.Fatalf("expected error for flag %s, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), "read-only") &&
				!strings.Contains(err.Error(), "not supported") &&
				!strings.Contains(err.Error(), "rejected") &&
				!strings.Contains(err.Error(), "only 'local'") {
				t.Errorf("expected rejection error mentioning read-only or not supported for %s, got: %v", tc.name, err)
			}
		})
	}
}

func TestTokenArgvRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	tmpDir := t.TempDir()
	err := runArgs(ctx, []string{"--data-dir", tmpDir, "--token", "secret123"})
	if err == nil {
		t.Fatal("expected error when passing --token on CLI")
	}
	if !strings.Contains(err.Error(), "token") && !strings.Contains(err.Error(), "rejected") {
		t.Errorf("unexpected error for --token: %v", err)
	}
}

func TestLocalSTTRequiresExplicitBinary(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	tmpDir := t.TempDir()
	err := runArgs(ctx, []string{"--data-dir", tmpDir, "--transcribe", "local"})
	if err == nil {
		t.Fatal("expected error when passing --transcribe local without --transcribe-bin")
	}
	if !strings.Contains(err.Error(), "--transcribe-bin") {
		t.Errorf("expected error mentioning --transcribe-bin, got: %v", err)
	}
}
