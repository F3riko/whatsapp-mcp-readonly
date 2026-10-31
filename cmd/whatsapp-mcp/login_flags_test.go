package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/thenavidm/whatsapp-mcp/internal/wa"
)

// loginRecorder replaces the login action, so no test reaches WhatsApp.
type loginRecorder struct {
	phones  []string
	clients int
	err     error
}

func (l *loginRecorder) login(_ context.Context, c *wa.Client, phone string) error {
	l.phones = append(l.phones, phone)
	if c != nil {
		l.clients++
	}
	return l.err
}

func runLogin(t *testing.T, stdin string, args ...string) (dir string, in *strings.Reader, rec *loginRecorder, err error) {
	t.Helper()
	dir = t.TempDir()
	in = strings.NewReader(stdin)
	rec = &loginRecorder{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The command, when there is one, must come first, as on a real command line.
	full := []string{}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		full, args = append(full, args[0]), args[1:]
	}
	full = append(full, "--data-dir", dir)
	err = runWith(ctx, append(full, args...), runEnv{stdin: in, login: rec.login})
	return dir, in, rec, err
}

func assertUntouched(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("data directory was touched: %v", names)
	}
}

func TestPhoneStdinIsLoginOnlyAndRejectedBeforeAnySetup(t *testing.T) {
	for _, args := range [][]string{
		{"--phone-stdin"},
		{"serve", "--phone-stdin"},
		{"version", "--phone-stdin"},
		{"--http", "127.0.0.1:8765", "--phone-stdin"},
	} {
		dir, in, rec, err := runLogin(t, synthPhone+"\n", args...)
		if err == nil || !strings.Contains(err.Error(), "--phone-stdin") || !strings.Contains(err.Error(), "login") {
			t.Errorf("args %v: error = %v, want a login-only rejection", args, err)
		}
		assertUntouched(t, dir)
		if len(rec.phones) != 0 {
			t.Errorf("args %v: the login action ran", args)
		}
		if in.Len() != len(synthPhone)+1 {
			t.Errorf("args %v: stdin was read before the flag was rejected", args)
		}
	}
}

func TestPhoneStdinRefusesDebugLogging(t *testing.T) {
	dir, _, rec, err := runLogin(t, synthPhone+"\n", "login", "--phone-stdin", "--debug")
	if err == nil || !strings.Contains(err.Error(), "--debug") {
		t.Fatalf("error = %v, want --debug refused with --phone-stdin", err)
	}
	assertUntouched(t, dir)
	if len(rec.phones) != 0 {
		t.Error("the login action ran")
	}
}

func TestLoginWithInvalidPhoneInputHasNoSideEffects(t *testing.T) {
	for _, in := range []string{"", "\n", "4155550100\n", "04155550100\n", "+1 415 555 0100\n", synthPhone + "\nextra\n", strings.Repeat("7", 500)} {
		dir, _, rec, err := runLogin(t, in, "login", "--phone-stdin")
		if err == nil {
			t.Errorf("input of length %d was accepted", len(in))
			continue
		}
		if trimmed := strings.TrimSpace(in); len(trimmed) > 3 && strings.Contains(err.Error(), trimmed) {
			t.Errorf("the error echoes the input: %v", err)
		}
		assertUntouched(t, dir) // no session.db, messages.db or audit log: no DB side effects
		if len(rec.phones) != 0 {
			t.Errorf("input of length %d reached the login action", len(in))
		}
	}
}

func TestLoginWithValidPhoneInputReachesLoginExactlyOnceWithTheNumber(t *testing.T) {
	_, in, rec, err := runLogin(t, synthPhone+"\n", "login", "--phone-stdin")
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if len(rec.phones) != 1 || rec.phones[0] != synthPhone || rec.clients != 1 {
		t.Errorf("login calls = %d (clients %d), want exactly one with the validated number", len(rec.phones), rec.clients)
	}
	if in.Len() != 0 {
		t.Error("stdin was not consumed")
	}
}

func TestLoginProviderFailureIsReturned(t *testing.T) {
	dir := t.TempDir()
	rec := &loginRecorder{err: errors.New("link failed")}
	err := runWith(context.Background(), []string{"login", "--data-dir", dir, "--phone-stdin"},
		runEnv{stdin: strings.NewReader(synthPhone), login: rec.login})
	if err == nil || err.Error() != "link failed" {
		t.Errorf("error = %v, want the login action's error", err)
	}
}

func TestLoginWithoutPhoneFlagStaysQRAndIgnoresStdin(t *testing.T) {
	_, in, rec, err := runLogin(t, synthPhone+"\n", "login")
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if len(rec.phones) != 1 || rec.phones[0] != "" {
		t.Errorf("login phones = %q, want exactly one empty (QR) call", rec.phones)
	}
	if in.Len() != len(synthPhone)+1 {
		t.Error("stdin was read although --phone-stdin was not given")
	}
}

func TestPhoneNumberCannotBePassedOnTheCommandLine(t *testing.T) {
	for _, args := range [][]string{
		{"login", "--phone", synthPhone},
		{"login", "--phone=" + synthPhone},
		{"login", "--phone-number", synthPhone},
		{"login", "--phone-stdin", synthPhone},
		{"login", synthPhone},
	} {
		dir, _, rec, err := runLogin(t, synthPhone+"\n", args...)
		if err == nil {
			t.Errorf("args %v: accepted a number on the command line", args)
			continue
		}
		if strings.Contains(err.Error(), "4155550100") {
			t.Errorf("args %v: the error echoes the number: %v", args, err)
		}
		assertUntouched(t, dir)
		if len(rec.phones) != 0 {
			t.Errorf("args %v: the login action ran", args)
		}
	}
}

func TestHelpDocumentsPhoneStdin(t *testing.T) {
	for _, want := range []string{"--phone-stdin", "login", "PAIR-CODE", "E.164"} {
		if !strings.Contains(usage, want) {
			t.Errorf("usage text lacks %q", want)
		}
	}
	if strings.Contains(usage, "4155550100") {
		t.Error("usage text contains an example number")
	}
}
