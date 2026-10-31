package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/thenavidm/whatsapp-mcp/internal/wa"
)

const (
	// maxPhoneInput bounds what is read from stdin. The longest valid input is
	// "+" and 15 digits and a "\r\n", 18 bytes.
	maxPhoneInput = 32
	// phoneInputTimeout bounds how long login waits for the number to arrive.
	phoneInputTimeout = 60 * time.Second
)

// errPhoneRead is deliberately generic: neither it nor the validation error
// ever repeats what was read.
var errPhoneRead = errors.New("could not read a phone number from stdin")

// readPhoneStdin reads the whole of r (to EOF, at most maxPhoneInput bytes) and
// returns it as one strict E.164 number. The only tolerated decoration is a
// single trailing line ending; anything else, including spaces and a second
// line, is rejected rather than repaired. Rejections never echo the input.
func readPhoneStdin(ctx context.Context, r io.Reader) (string, error) {
	type result struct {
		b   []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		b, err := io.ReadAll(io.LimitReader(r, maxPhoneInput+1))
		ch <- result{b, err}
	}()

	var res result
	select {
	case res = <-ch:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if res.err != nil {
		return "", errPhoneRead
	}
	if len(res.b) > maxPhoneInput {
		return "", wa.ErrInvalidPhone
	}

	in := string(res.b)
	if strings.HasSuffix(in, "\r\n") {
		in = strings.TrimSuffix(in, "\r\n")
	} else {
		in = strings.TrimSuffix(in, "\n")
	}
	if _, err := wa.ParseE164(in); err != nil {
		return "", err
	}
	return in, nil
}
