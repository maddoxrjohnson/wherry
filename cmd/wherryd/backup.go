package main

import (
	"context"
	"errors"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/maddoxrjohnson/wherry/internal/config"
	"github.com/maddoxrjohnson/wherry/internal/store"
)

// backup is "wherryd backup <dest-file>": it snapshots the database of the
// configured data directory, with or without a daemon running.
func backup(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: wherryd backup <dest-file>")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return store.Backup(ctx, filepath.Join(cfg.DataDir, "wherry.db"), args[0])
}
