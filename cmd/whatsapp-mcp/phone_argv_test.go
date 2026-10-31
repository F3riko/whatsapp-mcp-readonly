package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The test binary doubles as the real program: with this variable set it runs
// main() exactly as the shipped binary would (real os.Args, os.Stdin, stderr
// and exit status).
const runMainEnv = "WHATSAPP_MCP_TEST_RUN_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(runMainEnv) == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

// runBinary runs the real main() with args and stdin and returns the exit
// code and everything it wrote, plus the (initially empty) data directory.
func runBinary(t *testing.T, stdin string, args ...string) (code int, out string, dir string) {
	t.Helper()
	dir = t.TempDir()
	full := []string{}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		full, args = append(full, args[0]), args[1:]
	}
	full = append(full, "--data-dir", dir)
	cmd := exec.Command(os.Args[0], append(full, args...)...)
	cmd.Env = append(os.Environ(), runMainEnv+"=1")
	cmd.Stdin = strings.NewReader(stdin)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, buf.String(), dir
}

func TestPhoneStdinWithAValueNeverEchoesIt(t *testing.T) {
	for _, args := range [][]string{
		{"login", "--phone-stdin=" + synthPhone},
		{"login", "-phone-stdin=" + synthPhone},
		{"login", "--phone-stdin=4155550100"},
		{"--phone-stdin=" + synthPhone},
		{"serve", "-phone-stdin=" + synthPhone},
	} {
		code, out, dir := runBinary(t, synthPhone+"\n", args...)
		if code == 0 {
			t.Errorf("args %v: exit status 0", args)
		}
		if strings.Contains(out, "4155550100") || strings.Contains(out, "invalid boolean value") {
			t.Errorf("args %v: output echoes the value:\n%s", args, out)
		}
		if !strings.Contains(out, "--phone-stdin takes no value") {
			t.Errorf("args %v: want the fixed rejection, got:\n%s", args, out)
		}
		if strings.Contains(out, "Usage:") {
			t.Errorf("args %v: the parser dumped its usage text", args)
		}
		assertUntouched(t, dir)
	}
}

// Values the flag package would accept as booleans are only exercised with the
// recorder: they must never be able to reach a real login.
func TestPhoneStdinBooleanValuesAreRejectedToo(t *testing.T) {
	for _, arg := range []string{"--phone-stdin=true", "-phone-stdin=true", "--phone-stdin=false", "-phone-stdin=1", "--phone-stdin="} {
		dir, in, rec, err := runLogin(t, synthPhone+"\n", "login", arg)
		if err == nil || !strings.Contains(err.Error(), "--phone-stdin takes no value") {
			t.Errorf("%s: error = %v, want the fixed rejection", arg, err)
		}
		if len(rec.phones) != 0 {
			t.Errorf("%s: the login action ran", arg)
		}
		if in.Len() != len(synthPhone)+1 {
			t.Errorf("%s: stdin was read", arg)
		}
		assertUntouched(t, dir)
	}
}

func TestBareFlagAndServeModeAreUnchangedThroughTheRealBinary(t *testing.T) {
	// The bare flag is accepted and routed: a bad number gets the validation
	// error, with no echo and no files.
	code, out, dir := runBinary(t, "not a number\n", "login", "--phone-stdin")
	if code == 0 || !strings.Contains(out, "--phone-stdin:") || strings.Contains(out, "not a number") ||
		strings.Contains(out, "takes no value") {
		t.Errorf("bare flag: code %d, output:\n%s", code, out)
	}
	assertUntouched(t, dir)

	// Other flags and modes keep their meaning.
	code, out, dir = runBinary(t, "", "--allow-send")
	if code == 0 || !strings.Contains(out, "read-only fork") {
		t.Errorf("--allow-send: code %d, output:\n%s", code, out)
	}
	assertUntouched(t, dir)

	code, out, dir = runBinary(t, synthPhone+"\n", "serve", "--phone-stdin")
	if code == 0 || !strings.Contains(out, "login") {
		t.Errorf("serve --phone-stdin: code %d, output:\n%s", code, out)
	}
	assertUntouched(t, dir)

	code, out, _ = runBinary(t, "", "version")
	if code != 0 || !strings.Contains(out, "whatsapp-mcp ") {
		t.Errorf("version: code %d, output:\n%s", code, out)
	}
}
