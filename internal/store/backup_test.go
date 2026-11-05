package store

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestBackupCopiesRowsStillInTheWAL(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "live.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.CreateApp(ctx, "hello", "hello"); err != nil {
		t.Fatal(err)
	}
	// Left open and unchanged, so the row has not been checkpointed. A copy of
	// the main file alone would miss it.

	dest := filepath.Join(t.TempDir(), "copy.db")
	if err := Backup(ctx, path, dest); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %o, want 600", mode)
	}

	copied, err := Open(ctx, dest)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = copied.Close() }()
	apps, err := copied.ListApps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 || apps[0].Name != "hello" {
		t.Errorf("apps in the copy = %+v, want hello", apps)
	}
}

func TestBackupRefusesAnExistingFile(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "live.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	dest := filepath.Join(t.TempDir(), "copy.db")
	for _, content := range []string{"keep me", ""} {
		if err := os.WriteFile(dest, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := Backup(ctx, path, dest); !errors.Is(err, ErrExists) {
			t.Fatalf("backup over %q: err = %v, want ErrExists", content, err)
		}
		got, err := os.ReadFile(dest)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != content {
			t.Errorf("existing file now holds %q, want %q", got, content)
		}
	}
}

func TestBackupDoesNotCreateAMissingDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing.db")
	dest := filepath.Join(dir, "copy.db")
	if err := Backup(context.Background(), path, dest); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist", err)
	}
	for _, p := range []string{path, dest} {
		if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s was created", p)
		}
	}
}

func TestBackupRemovesTheFileWhenItFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "not-a.db")
	if err := os.WriteFile(path, []byte("this is not a database file, at any length"), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "copy.db")
	if err := Backup(context.Background(), path, dest); err == nil {
		t.Fatal("backup of a non-database succeeded")
	}
	if _, err := os.Stat(dest); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("failed backup left %s", dest)
	}
}
