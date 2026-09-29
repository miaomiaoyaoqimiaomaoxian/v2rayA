package main

import (
	"testing"
	"time"

	"github.com/v2rayA/v2rayA/db/configure"
)

func TestSubscriptionShouldUpdate(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		mode     configure.AutoUpdateMode
		interval int
		last     string
		start    bool
		want     bool
	}{
		{"disabled at start", configure.NotAutoUpdate, 0, "", true, false},
		{"disabled on timer", configure.NotAutoUpdate, 0, "", false, false},
		{"start mode at start", configure.AutoUpdate, 0, "", true, true},
		{"start mode on timer", configure.AutoUpdate, 0, "", false, false},
		{"invalid interval", configure.AutoUpdateAtIntervals, 0, "", true, false},
		{"first interval", configure.AutoUpdateAtIntervals, 24, "", true, true},
		{"not due", configure.AutoUpdateAtIntervals, 24, now.Add(-23 * time.Hour).Format(time.RFC3339), false, false},
		{"due", configure.AutoUpdateAtIntervals, 24, now.Add(-24 * time.Hour).Format(time.RFC3339), false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sub := configure.SubscriptionRaw{AutoUpdateMode: tc.mode, AutoUpdateIntervalHour: tc.interval, LastUpdateAttemptAt: tc.last}
			if got := subscriptionShouldUpdate(sub, now, tc.start); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
