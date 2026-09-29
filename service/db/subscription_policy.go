package db

import (
	"database/sql"
	"fmt"

	"github.com/tidwall/gjson"
)

// migrateSubscriptionPolicy keeps settings from releases that only had a
// global subscription schedule. Existing per-subscription settings are intact.
func migrateSubscriptionPolicy(database *sql.DB) error {
	tx, err := database.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	added := false
	for _, column := range []struct{ name, definition string }{
		{"auto_update_mode", "TEXT NOT NULL DEFAULT 'none'"},
		{"auto_update_interval_hour", "INTEGER NOT NULL DEFAULT 0"},
		{"last_update_attempt_at", "DATETIME DEFAULT NULL"},
	} {
		var count int
		if err := tx.QueryRow("SELECT COUNT(*) FROM pragma_table_info('subscriptions') WHERE name = ?", column.name).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			if _, err := tx.Exec("ALTER TABLE subscriptions ADD COLUMN " + column.name + " " + column.definition); err != nil {
				return err
			}
			if column.name != "last_update_attempt_at" {
				added = true
			}
		}
	}
	if added {
		var setting string
		err := tx.QueryRow("SELECT value FROM system_config WHERE key = 'system:setting'").Scan(&setting)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		mode := gjson.Get(setting, "subscriptionAutoUpdateMode").String()
		interval := int(gjson.Get(setting, "subscriptionAutoUpdateIntervalHour").Int())
		switch mode {
		case "auto_update":
			interval = 0
		case "auto_update_at_intervals":
			if interval < 1 {
				mode, interval = "none", 0
			}
		default:
			mode, interval = "none", 0
		}
		if _, err := tx.Exec("UPDATE subscriptions SET auto_update_mode = ?, auto_update_interval_hour = ?", mode, interval); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func MarkSubscriptionUpdateAttempt(id int64) error {
	result, err := GetDB().Exec("UPDATE subscriptions SET last_update_attempt_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now') WHERE id = ?", id)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("subscription %d not found", id)
	}
	return nil
}

func GetSubscriptionIndexByID(id int64) (int, error) {
	var index int
	err := GetDB().QueryRow("SELECT sort FROM subscriptions WHERE id = ?", id).Scan(&index)
	return index, err
}
