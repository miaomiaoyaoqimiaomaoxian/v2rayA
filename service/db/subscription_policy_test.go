package db

import (
	"database/sql"
	"testing"

	"github.com/tidwall/gjson"
)

func TestSubscriptionPolicyMigrationPreservesExistingValues(t *testing.T) {
	database, err := sql.Open(sqliteDriverName, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	if err := InitSchema(database); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO subscriptions (address, auto_update_mode, auto_update_interval_hour) VALUES
		('one', 'none', 0), ('two', 'auto_update_at_intervals', 24)`); err != nil {
		t.Fatal(err)
	}
	if err := MigrateSchema(database); err != nil {
		t.Fatal(err)
	}
	if err := MigrateSchema(database); err != nil {
		t.Fatal(err)
	}
	var disabled, enabled int
	if err := database.QueryRow(`SELECT COUNT(*) FROM subscriptions WHERE auto_update_mode='none'`).Scan(&disabled); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM subscriptions WHERE auto_update_mode='auto_update_at_intervals' AND auto_update_interval_hour=24`).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if disabled != 1 || enabled != 1 {
		t.Fatalf("policies changed: disabled=%d enabled=%d", disabled, enabled)
	}
}

func TestSubscriptionPolicyMigrationCopiesLegacyGlobalValue(t *testing.T) {
	database, err := sql.Open(sqliteDriverName, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	if _, err := database.Exec(`
		CREATE TABLE system_config (key TEXT PRIMARY KEY, value TEXT NOT NULL);
		CREATE TABLE subscriptions (id INTEGER PRIMARY KEY, address TEXT, remarks TEXT, auto_select INTEGER, sort INTEGER);
		INSERT INTO system_config VALUES ('system:setting', '{"subscriptionAutoUpdateMode":"auto_update_at_intervals","subscriptionAutoUpdateIntervalHour":12}');
		INSERT INTO subscriptions (address, remarks, auto_select, sort) VALUES ('one', '', 0, 0);
	`); err != nil {
		t.Fatal(err)
	}
	if err := migrateSubscriptionPolicy(database); err != nil {
		t.Fatal(err)
	}
	var mode string
	var interval int
	if err := database.QueryRow(`SELECT auto_update_mode, auto_update_interval_hour FROM subscriptions`).Scan(&mode, &interval); err != nil {
		t.Fatal(err)
	}
	if mode != "auto_update_at_intervals" || interval != 12 {
		t.Fatalf("legacy policy = (%q, %d)", mode, interval)
	}
}

func TestSetSubscriptionKeepsPerSourcePolicy(t *testing.T) {
	database, err := sql.Open(sqliteDriverName, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	if err := InitSchema(database); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO subscriptions (address, sort) VALUES ('one', 0)`); err != nil {
		t.Fatal(err)
	}
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := setSubscription(tx, 0, gjson.Parse(`{"address":"one","servers":[],"autoUpdateMode":"auto_update_at_intervals","autoUpdateIntervalHour":24}`)); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var mode string
	var interval int
	if err := database.QueryRow(`SELECT auto_update_mode, auto_update_interval_hour FROM subscriptions`).Scan(&mode, &interval); err != nil {
		t.Fatal(err)
	}
	if mode != "auto_update_at_intervals" || interval != 24 {
		t.Fatalf("saved policy = (%q, %d)", mode, interval)
	}
}
