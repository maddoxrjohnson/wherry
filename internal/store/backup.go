package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Backup writes a consistent snapshot of the database at path to dest, which
// must not exist. VACUUM INTO reads inside one transaction, so it is safe next
// to a running daemon, where copying the file could tear it against the WAL.
//
// It goes through openDB rather than Open so that a backup never migrates the
// database it is copying, and checks for the file first because openDB would
// create a missing one and a backup of nothing would look like a success.
func Backup(ctx context.Context, path, dest string) (err error) {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	// SQLite takes this as a filename, and absolute is never read as a URI.
	abs, err := filepath.Abs(dest)
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	// VACUUM INTO accepts an empty file, so O_EXCL can be the overwrite check,
	// and it fixes the mode: the database holds every app's config values, and
	// SQLite would otherwise create the file with whatever the umask allows.
	f, err := os.OpenFile(abs, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("backup %s: %w", dest, ErrExists)
	}
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(abs)
		}
	}()
	if err := f.Close(); err != nil {
		return fmt.Errorf("backup: %w", err)
	}

	db, err := openDB(ctx, path)
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, abs); err != nil {
		return fmt.Errorf("backup %s: %w", dest, err)
	}
	return nil
}
