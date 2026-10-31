package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestControllerAncestorSymlinkRejected(t *testing.T) {
	root := t.TempDir()
	actual := filepath.Join(root, "actual", "private")
	if e := os.MkdirAll(actual, 0700); e != nil {
		t.Fatal(e)
	}
	link := filepath.Join(root, "link")
	if e := os.Symlink(filepath.Join(root, "actual"), link); e != nil {
		t.Fatal(e)
	}
	st, err := Open(filepath.Join(link, "private", "messages.db"))
	if st != nil {
		st.Close()
	}
	if err == nil {
		t.Fatal("Open accepted a database path through a symlinked ancestor")
	}
}

func TestControllerConcurrentRevokeCannotResurrect(t *testing.T) {
	s, e := Open(filepath.Join(t.TempDir(), "messages.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	ctx := context.Background()
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("race-%d", i)
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		errs := make(chan error, 2)
		go func() {
			defer wg.Done()
			<-start
			errs <- s.UpsertMessage(ctx, StoredMessage{Message: Message{ID: id, ChatJID: "chat", Content: "private"}})
		}()
		go func() {
			defer wg.Done()
			<-start
			_, err := s.DeleteMessage(ctx, "chat", id)
			errs <- err
		}()
		close(start)
		wg.Wait()
		for j := 0; j < 2; j++ {
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
		}
		var count int
		if e := s.db.QueryRow("SELECT COUNT(*) FROM messages WHERE chat_jid=? AND id=?", "chat", id).Scan(&count); e != nil {
			t.Fatal(e)
		}
		if count != 0 {
			t.Fatalf("revoked message %s resurrected after concurrent ingest; rows=%d", id, count)
		}
	}
}
