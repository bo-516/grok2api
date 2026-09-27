package media

import (
	"testing"
	"time"
)

// TestJobsFullAndTTL rejects a third reserve at max 2 and hides a job after TTL.
func TestJobsFullAndTTL(t *testing.T) {
	now := time.Unix(0, 0)
	j := NewJobs(2, time.Minute)
	j.Clock = func() time.Time { return now }
	id1, err := j.Reserve("m")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.Reserve("m"); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Reserve("m"); err != ErrJobsFull {
		t.Fatal(err)
	}
	j.MarkRunning(id1)
	q, r := j.Counts()
	if q != 1 || r != 1 {
		t.Fatalf("queued %d running %d", q, r)
	}
	now = now.Add(61 * time.Second)
	j.Sweep()
	if _, ok := j.Get(id1); ok {
		t.Fatal("expired job still visible")
	}
}
