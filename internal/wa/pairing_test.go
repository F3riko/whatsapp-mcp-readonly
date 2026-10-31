package wa

import (
	"context"
	"strings"
	"testing"
)

func TestNormalServeRefusesUnpairedSession(t *testing.T) {
	c := &Client{} // completely unpaired / uninitialized session
	ctx := context.Background()

	err := c.ServeConnect(ctx)
	if err == nil {
		t.Fatal("expected ServeConnect to refuse unpaired session, got nil")
	}
	if !strings.Contains(err.Error(), "not paired") && !strings.Contains(err.Error(), "login") {
		t.Errorf("expected error mentioning not paired or run login, got: %v", err)
	}
}
