package doc

import (
	"context"
	"fmt"
	"strings"
)

// Checkpoint folds the write-ahead log into the main file and starts a fresh WAL
// generation, bounding WAL growth without closing the database (spec 2061 doc 18
// §15.4). It is the online counterpart to the checkpoint that Close performs. The
// mode is accepted for MongoDB and SQLite compatibility; doc's checkpoint always
// resets the WAL generation, so every mode performs the same full checkpoint.
func (db *DB) Checkpoint(ctx context.Context, mode string) error {
	if err := db.check(ctx); err != nil {
		return err
	}
	if mode != "" && !validCheckpointMode(mode) {
		return fmt.Errorf("doc: checkpoint mode wants passive, full, restart, or truncate, got %q", mode)
	}
	if err := db.eng.Checkpoint(); err != nil {
		return mapEngineErr(err)
	}
	db.logger(logComponentWAL).Info("checkpoint completed", "mode", checkpointModeOrDefault(mode))
	return nil
}

// IncrementalVacuum reclaims up to n trailing free pages to the operating system,
// shrinking the file (spec 2061 doc 18 §15.2). A value of n at or below zero
// reclaims every trailing free page. It returns the number of pages reclaimed. It
// is a no-op that returns zero when auto_vacuum is none, mirroring SQLite, where
// incremental_vacuum has nothing to do unless the incremental mode is enabled.
func (db *DB) IncrementalVacuum(ctx context.Context, n int) (int, error) {
	if err := db.check(ctx); err != nil {
		return 0, err
	}
	db.pragMu.Lock()
	mode := db.autoVacuum
	db.pragMu.Unlock()
	if mode == "" || mode == "none" {
		return 0, nil
	}
	reclaimed, err := db.eng.IncrementalVacuum(n)
	if err != nil {
		return 0, mapEngineErr(err)
	}
	if reclaimed > 0 {
		db.logger(logComponentStorage).Info("incremental vacuum reclaimed pages", "pages", reclaimed)
	}
	return reclaimed, nil
}

// autoVacuumMode returns the database's auto_vacuum mode under the lock, defaulting
// to none when unset.
func (db *DB) autoVacuumMode() string {
	db.pragMu.Lock()
	defer db.pragMu.Unlock()
	if db.autoVacuum == "" {
		return "none"
	}
	return db.autoVacuum
}

// setAutoVacuumMode records the auto_vacuum mode under the lock.
func (db *DB) setAutoVacuumMode(mode string) { // mode is pre-validated
	db.pragMu.Lock()
	db.autoVacuum = mode
	db.pragMu.Unlock()
}

// validCheckpointMode reports whether s is one of the four SQLite checkpoint modes.
func validCheckpointMode(s string) bool {
	switch strings.ToLower(s) {
	case "passive", "full", "restart", "truncate":
		return true
	}
	return false
}

// checkpointModeOrDefault returns the mode for logging, substituting passive when
// the caller passed no mode.
func checkpointModeOrDefault(mode string) string {
	if mode == "" {
		return "passive"
	}
	return strings.ToLower(mode)
}
