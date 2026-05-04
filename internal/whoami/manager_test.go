package whoami

import (
	"testing"
	"time"
)

func TestTokenExpiringSoon(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		exp  time.Time
		want bool
	}{
		{"already expired", now.Add(-1 * time.Hour), true},
		{"expires now", now, true},
		{"expires in 1 day", now.Add(24 * time.Hour), true},
		{"expires in 6 days", now.Add(6 * 24 * time.Hour), true},
		{"expires 1ns before window boundary", now.Add(7*24*time.Hour - time.Nanosecond), true},
		{"expires exactly at window boundary", now.Add(7 * 24 * time.Hour), false},
		{"expires in 8 days", now.Add(8 * 24 * time.Hour), false},
		{"expires in 30 days", now.Add(30 * 24 * time.Hour), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tokenExpiringSoon(tc.exp, now)
			if got != tc.want {
				t.Errorf("tokenExpiringSoon(exp=%v, now=%v) = %v, want %v", tc.exp, now, got, tc.want)
			}
		})
	}
}
