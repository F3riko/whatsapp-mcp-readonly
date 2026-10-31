package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// Synthetic values only.
const synthPhone = "+14155550100"

func TestReadPhoneStdinAcceptsOneStrictNumberWithOptionalLineEnding(t *testing.T) {
	for _, in := range []string{synthPhone, synthPhone + "\n", synthPhone + "\r\n"} {
		got, err := readPhoneStdin(context.Background(), strings.NewReader(in))
		if err != nil {
			t.Errorf("rejected valid input of length %d: %v", len(in), err)
			continue
		}
		if got != synthPhone {
			t.Errorf("returned a different value than the input number (length %d vs %d)", len(got), len(synthPhone))
		}
	}
}

func TestReadPhoneStdinRejectsBadInputWithoutEchoingIt(t *testing.T) {
	bad := []string{
		"",
		"\n",
		"+",
		"14155550100\n",                 // no +
		"04155550100\n",                 // local
		"+1 415 555 0100\n",             // punctuation
		"+1-415-555-0100\n",             //
		" " + synthPhone + "\n",         // space is not trimmed
		synthPhone + " \n",              //
		synthPhone + "\n\n",             // more than one line ending
		synthPhone + "\n+14155550101\n", // a second value
		synthPhone + "\r\r\n",           //
		strings.Repeat("9", 100),        // oversized
		"+" + strings.Repeat("1", 200),  // oversized
	}
	for _, in := range bad {
		got, err := readPhoneStdin(context.Background(), strings.NewReader(in))
		if err == nil {
			t.Errorf("accepted bad input of length %d (returned length %d)", len(in), len(got))
			continue
		}
		if got != "" {
			t.Errorf("returned a value with an error for input of length %d", len(in))
		}
		if trimmed := strings.TrimSpace(in); len(trimmed) > 3 && strings.Contains(err.Error(), trimmed) {
			t.Errorf("the error echoes the input: %v", err)
		}
	}
}

// endless yields an unbounded stream and counts how much was taken from it.
type endless struct{ n int }

func (e *endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = '1'
	}
	e.n += len(p)
	return len(p), nil
}

func TestReadPhoneStdinReadsAtMostASmallBound(t *testing.T) {
	src := &endless{}
	if _, err := readPhoneStdin(context.Background(), src); err == nil {
		t.Fatal("accepted an endless stream")
	}
	if src.n > 4096 {
		t.Errorf("read %d bytes from an endless stream; the bound must be small", src.n)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("read failed for " + synthPhone) }

func TestReadPhoneStdinReadErrorIsGeneric(t *testing.T) {
	_, err := readPhoneStdin(context.Background(), failingReader{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), synthPhone) {
		t.Errorf("the error carries the reader's text: %v", err)
	}
}

func TestReadPhoneStdinStopsWhenContextIsCancelled(t *testing.T) {
	pr, pw := io.Pipe() // never written to: a terminal nobody types into
	defer pw.Close()
	defer pr.Close()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		_, err := readPhoneStdin(ctx, pr)
		done <- err
	}()
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("readPhoneStdin ignored cancellation")
	}
}
