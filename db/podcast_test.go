package db

import (
	"testing"
	"time"
)

func TestJobLockIsLocked(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		lock *JobLock
		want bool
	}{
		{name: "nil lock", lock: nil, want: false},
		{name: "zero date", lock: &JobLock{Duration: 10}, want: false},
		{name: "zero duration", lock: &JobLock{Date: now}, want: false},
		{name: "active lock", lock: &JobLock{Date: now.Add(-time.Minute), Duration: 2}, want: true},
		{name: "expired lock", lock: &JobLock{Date: now.Add(-2 * time.Minute), Duration: 1}, want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.lock.IsLocked(); got != test.want {
				t.Fatalf("IsLocked() = %v, want %v", got, test.want)
			}
		})
	}
}
