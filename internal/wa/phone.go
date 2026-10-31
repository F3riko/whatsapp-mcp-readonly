package wa

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.mau.fi/whatsmeow"
)

// ErrInvalidPhone is returned for anything that is not a strict E.164 number.
// It deliberately says nothing about the rejected value.
var ErrInvalidPhone = errors.New("phone number must be international E.164: '+', a non-zero digit, then 6 to 14 more digits, with nothing else")

// Failures that cross the provider boundary are reported by these fixed values
// and never by the underlying error: whatsmeow errors can carry the number or
// a pairing code, and this output goes to a terminal that may be recorded.
var (
	ErrAlreadyLinked  = errors.New("this data directory is already linked to a WhatsApp account; refusing to link another or unlink it")
	ErrPairCodeFailed = errors.New("WhatsApp did not issue a pairing code; check the number and run login again")
	ErrPairingFailed  = errors.New("phone pairing failed; run login again")
	ErrPairingTimeout = errors.New("pairing timed out, run the command again")
)

const (
	minPhoneDigits = 7  // the shortest number whatsmeow accepts
	maxPhoneDigits = 15 // the E.164 maximum
)

// ParseE164 validates a strict international number ("+" and 7 to 15 ASCII
// digits, the first non-zero) and returns the digits alone, which is the form
// whatsmeow's PairPhone wants.
//
// whatsmeow strips every non-digit from what it is given, so a malformed
// number would be silently "repaired" into a different one. Validate first,
// reject rather than repair, and never infer a country.
func ParseE164(s string) (string, error) {
	if len(s) < 1+minPhoneDigits || len(s) > 1+maxPhoneDigits || s[0] != '+' || s[1] == '0' {
		return "", ErrInvalidPhone
	}
	digits := s[1:]
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return "", ErrInvalidPhone
		}
	}
	return digits, nil
}

// What WhatsApp is told about the linking client in phone mode. The server
// validates the display name and only accepts "Browser (OS)" for a real
// browser, so these are fixed rather than taken from --device-name.
const (
	phoneClientType    = whatsmeow.PairClientChrome
	phoneClientDisplay = "Chrome (Linux)"
)

// LinkWithPhone links this device by phone number instead of a QR code and
// blocks until it is linked, fails, or ctx is canceled.
//
// whatsmeow requires the QR channel to be requested before Connect, and a pair
// code to be requested only once the connection is up, which is signalled by
// the first item on that channel. Exactly one code is requested; every later
// QR item is drained and ignored, so no QR payload is ever drawn or relayed and
// no second code is generated behind the owner's back.
func (c *Client) LinkWithPhone(ctx context.Context, e164 string) (err error) {
	digits, err := ParseE164(e164)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Never unlink, repair or add a second account to an existing link.
	if c.pair.LinkedID() != nil {
		return ErrAlreadyLinked
	}
	// Whatever goes wrong from here on, do not leave a connection open.
	defer func() {
		if err != nil {
			c.pair.Disconnect()
		}
	}()

	qrChan, err := c.pair.GetQRChannel(ctx)
	if err != nil {
		return fmt.Errorf("qr channel: %w", err)
	}
	if err := c.pair.Connect(); err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	requested := false
	for {
		var evt whatsmeow.QRChannelItem
		select {
		case <-ctx.Done():
			return ctx.Err()
		case e, ok := <-qrChan:
			if !ok {
				// The channel only closes after a final event, so closing
				// without one means the link did not complete.
				if err := ctx.Err(); err != nil {
					return err
				}
				return ErrPairingFailed
			}
			evt = e
		}

		switch evt.Event {
		case whatsmeow.QRChannelEventCode:
			if requested {
				continue
			}
			requested = true
			// select picks at random when cancellation and this item are both
			// ready; do not send a code request on a context that is already dead.
			if err := ctx.Err(); err != nil {
				return err
			}
			linkCode, err := c.pair.PairPhone(ctx, digits, true, phoneClientType, phoneClientDisplay)
			if err := ctx.Err(); err != nil {
				return err
			}
			if err != nil || linkCode == "" {
				// The provider error may carry the number or the code, so it
				// is dropped rather than wrapped.
				return ErrPairCodeFailed
			}
			fmt.Fprintln(c.out, "\nOn your phone open WhatsApp, then Settings, Linked Devices, Link a Device,")
			fmt.Fprintln(c.out, "then choose 'Link with phone number instead' and enter this code.")
			fmt.Fprintf(c.out, "\nPAIR-CODE: %s\n\n", linkCode)
		case whatsmeow.QRChannelSuccess.Event:
			if err := c.finishPairing(ctx); err != nil {
				return err
			}
			fmt.Fprintln(c.out, "Linked.\nIt will now appear under Linked Devices on your phone.")
			return nil
		case whatsmeow.QRChannelTimeout.Event:
			return ErrPairingTimeout
		case whatsmeow.QRChannelEventError:
			// evt.Error is dropped for the same reason as a PairPhone error.
			return ErrPairingFailed
		default:
			// The library's terminal states (unexpected state, outdated
			// client, scanned without multi-device). Other events, such as
			// passkey prompts, are not ours to act on and are ignored.
			if strings.HasPrefix(evt.Event, "err-") {
				return ErrPairingFailed
			}
		}
	}
}
