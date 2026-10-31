package wa

import (
	"strings"
	"testing"
)

func TestParseE164AcceptsOnlyStrictInternationalNumbers(t *testing.T) {
	valid := map[string]string{
		"+14155550100":     "14155550100",
		"+447700900123":    "447700900123",
		"+1234567":         "1234567",         // shortest the library accepts
		"+123456789012345": "123456789012345", // E.164 maximum, 15 digits
	}
	for in, want := range valid {
		got, err := ParseE164(in)
		if err != nil {
			t.Errorf("ParseE164 rejected a valid number: %v", err)
			continue
		}
		if got != want {
			t.Errorf("ParseE164 returned %q, want %q", got, want)
		}
	}
}

func TestParseE164RejectsEverythingElseWithoutEchoingIt(t *testing.T) {
	invalid := []string{
		"",
		"+",
		"14155550100",       // no leading +
		"0014155550100",     // 00 international prefix is not E.164
		"04155550100",       // local format
		"+04155550100",      // country codes never start with 0
		" +14155550100",     // surrounding space is not silently repaired
		"+14155550100 ",     //
		"+14155550100\n",    // line endings are the reader's job
		"+1 415 555 0100",   // punctuation
		"+1-415-555-0100",   //
		"+1(415)5550100",    //
		"++14155550100",     //
		"tel:+14155550100",  //
		"+141555501xx",      //
		"+123456",           // too short
		"+1234567890123456", // 16 digits, too long
		"+１４１５５５５０１００",      // full-width digits
		"+14155550100\x00",  // NUL
	}
	for _, in := range invalid {
		got, err := ParseE164(in)
		if err == nil {
			t.Errorf("ParseE164 accepted malformed input of length %d (returned %q)", len(in), got)
			continue
		}
		if got != "" {
			t.Errorf("ParseE164 returned a value alongside an error for input of length %d", len(in))
		}
		if len(in) > 3 && strings.Contains(err.Error(), strings.TrimSpace(in)) {
			t.Errorf("ParseE164 error echoes the input: %v", err)
		}
	}
}
