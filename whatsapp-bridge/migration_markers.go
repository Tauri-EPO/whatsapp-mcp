package main

import "database/sql"

// Each data rewrite owns a durable name; user_version from old releases is
// deliberately untouched, so an incomplete or newer rewrite cannot hide another.
func migrationApplied(db *sql.DB, name string) (bool, error) {
	if _, err := db.Exec("CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY)"); err != nil {
		return false, err
	}
	var applied bool
	err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE name = ?)", name).Scan(&applied)
	return applied, err
}

func recordMigration(ex sqlExecer, name string) error {
	_, err := ex.Exec("INSERT INTO schema_migrations(name) VALUES (?)", name)
	return err
}

// applyNamedMigration commits a one-off rewrite and its marker together. The
// callback must use its transaction for every effect; an error rolls both back.
// An already applied name skips the callback, including after a concurrent start.
func applyNamedMigration(db *sql.DB, name string, body func(*sql.Tx) error) (bool, error) {
	applied, err := migrationApplied(db, name)
	if err != nil || applied {
		return false, err
	}
	tx, err := db.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE name = ?)", name).Scan(&applied); err != nil {
		return false, err
	}
	if applied {
		return false, nil
	}
	if err := body(tx); err != nil {
		return false, err
	}
	if err := recordMigration(tx, name); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
