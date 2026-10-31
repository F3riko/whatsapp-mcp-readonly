// Command whatsapp-mcp is a hardened Model Context Protocol server for your personal
// WhatsApp account.
//
// In this read-only fork, write/send tools and hosted transcription are completely disabled.
// It links to WhatsApp as a companion device, maintains a local private searchable copy of history,
// and serves read tools to an AI agent over MCP (stdio or loopback HTTP).
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/thenavidm/whatsapp-mcp/internal/mcpserver"
	"github.com/thenavidm/whatsapp-mcp/internal/safety"
	"github.com/thenavidm/whatsapp-mcp/internal/store"
	"github.com/thenavidm/whatsapp-mcp/internal/transcribe"
	"github.com/thenavidm/whatsapp-mcp/internal/wa"
)

// version is overridden at release time with -ldflags.
var version = "dev"

const usage = `whatsapp-mcp - Hardened read-only WhatsApp MCP server for AI agents

Usage:
  whatsapp-mcp [flags]          Serve read-only MCP over stdio (default)
  whatsapp-mcp login            Link device via interactive QR terminal display and exit
  whatsapp-mcp version          Print the version

General flags:
  --data-dir PATH       Where session.db and messages.db live (default: ~/.whatsapp-mcp)
  --device-name NAME    Device name under Linked Devices (default: "WhatsApp MCP")
  --debug               Verbose protocol logging on stderr

Local transcription (optional, off by default):
  --transcribe local        Enable offline local transcription of voice notes
  --transcribe-bin PATH     Pinned absolute path to local whisper adapter executable (required when --transcribe is local)
  --transcribe-model MODEL  Local model name or path (default: "tiny")
  --transcribe-lang LANG    Optional ISO-639-1 language hint (e.g. "en")

Remote access (optional, loopback only):
  --http ADDR           Serve streamable MCP over HTTP (e.g. 127.0.0.1:8765, loopback only)
  --token-file PATH     Path to file containing bearer token (preferred over env var)
  --public-host HOST    Exact trusted public HTTPS hostname behind reverse proxy (e.g. mcp.example.com)

Hardened Fork Architecture & Privacy Notes:
  - Read-only: all send, speak, upload, and auto-reply tools are structurally disabled.
  - Normal serve refuses unpaired sessions; interactive QR pairing requires 'whatsapp-mcp login'.
  - Strict loopback bind and origin protection prevent DNS rebinding and cross-origin abuse.
  - View-once and disappearing ephemeral messages are strictly excluded from history.
  - Audio and media admission limits prevent wire and disk exhaustion.

Everything is written to stderr. Stdout carries the MCP protocol and must stay
clean, so never redirect stderr into stdout.
`

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "whatsapp-mcp: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runArgs(ctx, os.Args[1:])
}

func runArgs(ctx context.Context, rawArgs []string) error {
	defaultDir := ""
	if home, err := os.UserHomeDir(); err == nil {
		defaultDir = filepath.Join(home, ".whatsapp-mcp")
	}

	fs := flag.NewFlagSet("whatsapp-mcp", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, usage)
	}

	dataDir := fs.String("data-dir", defaultDir, "where session and history live")
	allowSend := fs.Bool("allow-send", false, "permit sending messages (prohibited in read-only fork)")
	sendTo := fs.String("send-to", "", "comma separated chat ids (prohibited in read-only fork)")
	rateLimit := fs.Int("rate-limit", 10, "maximum sends per minute")
	debug := fs.Bool("debug", false, "verbose protocol logging")
	deviceName := fs.String("device-name", "WhatsApp MCP", "name shown under Linked Devices on your phone")
	httpAddr := fs.String("http", "", "serve MCP over HTTP at this address instead of stdio")
	httpToken := fs.String("token", "", "bearer token (rejected: use --token-file or WHATSAPP_MCP_TOKEN)")
	tokenFile := fs.String("token-file", "", "path to file containing bearer token")
	publicHost := fs.String("public-host", "", "hostname a reverse proxy reaches this on, for example whatsapp.example.com")
	sttProvider := fs.String("transcribe", "", "local (offline transcription of voice notes)")
	sttBin := fs.String("transcribe-bin", "", "pinned absolute executable path to local transcription adapter")
	sttModel := fs.String("transcribe-model", "", "override the provider default model")
	sttLang := fs.String("transcribe-lang", "", "ISO-639-1 language hint")
	voiceProvider := fs.String("voice", "", "prohibited in read-only fork")
	voiceID := fs.String("voice-id", "", "prohibited in read-only fork")
	agentChat := fs.String("agent-chat", "", "prohibited in read-only fork")
	agentCmd := fs.String("agent-command", "", "prohibited in read-only fork")
	agentArgs := fs.String("agent-args", "", "prohibited in read-only fork")
	agentDir := fs.String("agent-dir", "", "prohibited in read-only fork")

	args := rawArgs
	command := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command = args[0]
		args = args[1:]
	}

	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}

	switch command {
	case "version":
		fmt.Printf("whatsapp-mcp %s (hardened read-only fork)\n", version)
		return nil
	case "", "serve", "login":
	default:
		return fmt.Errorf("unknown command %q", command)
	}

	// Reject prohibited flags in hardened read-only fork
	if *allowSend {
		return fmt.Errorf("read-only fork: sending is permanently disabled; --allow-send is rejected")
	}
	if *sendTo != "" {
		return fmt.Errorf("read-only fork: sending is permanently disabled; --send-to is rejected")
	}
	if *voiceProvider != "" {
		return fmt.Errorf("read-only fork: voice synthesis and sending are disabled; --voice is rejected")
	}
	if *voiceID != "" {
		return fmt.Errorf("read-only fork: voice synthesis is disabled; --voice-id is rejected")
	}
	if *agentChat != "" || *agentCmd != "" || *agentArgs != "" || *agentDir != "" {
		return fmt.Errorf("read-only fork: automated agent replies are disabled; agent flags are rejected")
	}
	if *sttProvider != "" {
		p := strings.ToLower(strings.TrimSpace(*sttProvider))
		if p != "local" && p != "off" && p != "" {
			return fmt.Errorf("read-only fork: only 'local' transcription is supported; hosted provider %q is rejected", *sttProvider)
		}
		if p == "local" && strings.TrimSpace(*sttBin) == "" {
			return fmt.Errorf("read-only fork: --transcribe local requires an explicit pinned executable path via --transcribe-bin")
		}
	}
	if *httpToken != "" {
		return fmt.Errorf("read-only fork: passing token via --token is rejected (exposes secrets in process table); use --token-file or WHATSAPP_MCP_TOKEN")
	}

	var resolvedToken string
	if *tokenFile != "" {
		b, err := os.ReadFile(*tokenFile)
		if err != nil {
			return fmt.Errorf("read token-file %q: %w", *tokenFile, err)
		}
		resolvedToken = strings.TrimSpace(string(b))
	} else if envTok := os.Getenv("WHATSAPP_MCP_TOKEN"); envTok != "" {
		resolvedToken = strings.TrimSpace(envTok)
	}

	if *httpAddr != "" && resolvedToken == "" {
		return fmt.Errorf("read-only fork: serving HTTP requires a token provided via --token-file or WHATSAPP_MCP_TOKEN")
	}

	if *dataDir == "" {
		return fmt.Errorf("could not determine a home directory, pass --data-dir")
	}

	st, err := store.Open(filepath.Join(*dataDir, "messages.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	auditLog := filepath.Join(*dataDir, "audit.log")
	guard, err := safety.New(safety.Config{
		AllowSend: false,
		Allowlist: splitList(*sendTo),
		PerMinute: *rateLimit,
		AuditPath: auditLog,
	})
	if err != nil {
		return err
	}
	defer guard.Close()

	stt, err := transcribe.New(transcribe.Config{
		Provider: *sttProvider,
		Binary:   *sttBin,
		Model:    *sttModel,
		Language: *sttLang,
	})
	if err != nil {
		return err
	}
	if stt != nil {
		fmt.Fprintf(os.Stderr, "Transcribing voice notes with %s.\n", stt.Name())
	}

	client, err := wa.New(ctx, wa.Options{
		DataDir:     *dataDir,
		Store:       st,
		Out:         os.Stderr,
		Debug:       *debug,
		Transcriber: stt,
		DeviceName:  *deviceName,
	})
	if err != nil {
		return err
	}
	defer client.Disconnect()

	if command == "login" {
		if err := client.Connect(ctx); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "Linked. You can now add this server to your AI client.")
		return nil
	}

	// Normal serve mode: refuse unpaired sessions instead of streaming QR code
	if err := client.ServeConnect(ctx); err != nil {
		return err
	}

	fmt.Fprintln(os.Stderr, "Running hardened read-only fork. Sending is disabled.")

	mcpserver.Version = version
	srv := mcpserver.New(mcpserver.Deps{Store: st, Client: client, Guard: guard})

	if *httpAddr != "" {
		return serveHTTP(ctx, srv, *httpAddr, resolvedToken, *publicHost)
	}

	fmt.Fprintln(os.Stderr, "MCP server ready on stdio.")
	return srv.Run(ctx, &mcp.StdioTransport{})
}

// serveHTTP runs the streamable HTTP transport and shuts down cleanly on
// signal, so the WhatsApp session is not left half open.
func serveHTTP(ctx context.Context, srv *mcp.Server, addr, token, publicHost string) error {
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("serving HTTP requires a token provided via --token-file or WHATSAPP_MCP_TOKEN")
	}

	httpSrv, err := mcpserver.HTTPServer(srv, addr, token, publicHost)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "MCP server ready on http://%s\n", addr)

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdown)
	}
}

func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
