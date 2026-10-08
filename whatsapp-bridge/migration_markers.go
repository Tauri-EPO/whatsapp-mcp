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
	_, err := ex.Exec("INSERT OR IGNORE INTO schema_migrations(name) VALUES (?)", name)
	return err
}
