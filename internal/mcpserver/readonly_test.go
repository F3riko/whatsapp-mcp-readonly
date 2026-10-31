package mcpserver

import (
	"context"
	"testing"
)

func TestReadOnlyToolRegistration(t *testing.T) {
	cs, _, _ := session(t, false)

	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}

	byName := map[string]bool{}
	for _, tool := range res.Tools {
		byName[tool.Name] = true
	}

	// Required read tools
	expectedReadTools := []string{
		"search_messages",
		"list_messages",
		"list_chats",
		"search_contacts",
		"session_status",
		"download_media",
	}
	for _, want := range expectedReadTools {
		if !byName[want] {
			t.Errorf("expected read tool %s is not advertised", want)
		}
	}

	// Forbidden write/send tools
	prohibitedSendTools := []string{
		"send_message",
		"send_file",
		"send_voice_note",
		"send_audio",
		"speak_message",
	}
	for _, bad := range prohibitedSendTools {
		if byName[bad] {
			t.Errorf("prohibited send tool %s must not be registered in read-only fork", bad)
		}
	}

	if len(res.Tools) != len(expectedReadTools) {
		t.Errorf("expected exactly %d tools, got %d", len(expectedReadTools), len(res.Tools))
	}
}
