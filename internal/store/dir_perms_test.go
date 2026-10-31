package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDataDirectorySymlinkRejected(t *testing.T) {
	tmp := t.TempDir()
	realDir := filepath.Join(tmp, "real_data")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatalf("mkdir real_data: %v", err)
	}

	symDir := filepath.Join(tmp, "symlink_data")
	if err := os.Symlink(realDir, symDir); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	dbPath := filepath.Join(symDir, "messages.db")
	_, err := Open(dbPath)
	if err == nil {
		t.Fatal("expected Open to reject symlinked data directory, got nil")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("expected error mentioning symlink, got: %v", err)
	}
}

func TestDatabaseFilePermissionsHardened(t *testing.T) {
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "secure_data")
	dbPath := filepath.Join(dataDir, "messages.db")

	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	st.Close()

	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat db: %v", err)
	}
	// Verify file mode is 0600 (not readable/writable by group or others)
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("expected database permissions 0600, got: %o", perm)
	}
}

func TestControllerDatabaseSymlinkRejected(t *testing.T) {
	root := t.TempDir()
	victim := filepath.Join(root, "victim", "messages.db")
	v, e := Open(victim)
	if e != nil {
		t.Fatal(e)
	}
	v.Close()
	d := filepath.Join(root, "service")
	if e = os.Mkdir(d, 0700); e != nil {
		t.Fatal(e)
	}
	link := filepath.Join(d, "messages.db")
	if e = os.Symlink(victim, link); e != nil {
		t.Fatal(e)
	}
	opened, err := Open(link)
	if opened != nil {
		opened.Close()
	}
	if err == nil {
		t.Fatal("Open accepted database symlink to another directory")
	}
}
