package media

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// ErrJobsFull means queued plus running jobs are already at the cap.
// The HTTP layer maps it to 429 agent_mock_busy with Retry-After: 5.
var ErrJobsFull = errors.New("video job queue is full")

// Job is one video request. Status is pending, done, or failed.
// Progress while pending is only 0, 10, or 50. Done uses 100.
type Job struct {
	// ID is the request_id returned to the caller.
	ID string
	// Status is pending, done, or failed.
	Status string
	// Progress is the coarse percent. Pending values are 0, 10, or 50.
	Progress int
	// Model is the caller model, or grok-imagine-video when omitted.
	Model string
	// File is the stored media name once Status is done.
	File string
	// Duration is the requested duration in seconds, echoed on done.
	Duration int
	// ErrCode is the xAI error code when Status is failed.
	ErrCode string
	// ErrMsg is the failure message, including grok's own sentence.
	ErrMsg string
	// Created is when the job was reserved. TTL is measured from here.
	Created time.Time
	// Phase is queued, running, done, or failed. queued and running count
	// toward the cap. Status stays pending for both queued and running.
	Phase string
}

// Jobs is the in-memory video table. It is not kept across a process restart.
type Jobs struct {
	// Max is queued plus running before Reserve returns ErrJobsFull.
	Max int
	// TTL is how long a record is visible. Zero means it does not expire.
	TTL time.Duration
	// Clock is the time source. Nil uses time.Now.
	Clock func() time.Time

	mu sync.Mutex
	m  map[string]*Job
}

// NewJobs returns an empty table. max < 1 is treated as 1 so a bad flag cannot
// make every submit succeed. ttl is the media TTL.
func NewJobs(max int, ttl time.Duration) *Jobs {
	if max < 1 {
		max = 1
	}
	return &Jobs{Max: max, TTL: ttl, m: map[string]*Job{}}
}

// now returns Clock or time.Now.
func (j *Jobs) now() time.Time {
	if j != nil && j.Clock != nil {
		return j.Clock()
	}
	return time.Now()
}

// Reserve adds a pending job at progress 0 if the cap allows.
// model is stored for the status echo. The id is a random UUID.
// ErrJobsFull means the caller should return 429 and not stage inputs.
func (j *Jobs) Reserve(model string) (string, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	n := 0
	for _, job := range j.m {
		if job.Phase == "queued" || job.Phase == "running" {
			n++
		}
	}
	if n >= j.Max {
		return "", ErrJobsFull
	}
	id, err := newUUID()
	if err != nil {
		return "", err
	}
	j.m[id] = &Job{ID: id, Status: "pending", Progress: 0, Model: model, Created: j.now(), Phase: "queued"}
	return id, nil
}

// Drop removes a reserved job that never started, so a staging failure does not
// hold a slot. An unknown id is ignored.
func (j *Jobs) Drop(id string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	delete(j.m, id)
}

// MarkRunning moves a queued job to running. Progress stays 0 until a tool starts.
func (j *Jobs) MarkRunning(id string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if job := j.m[id]; job != nil && job.Phase == "queued" {
		job.Phase = "running"
	}
}

// SetProgress sets a pending progress of 0, 10, or 50.
// Any other value is ignored so the status cannot advertise a finer percent.
func (j *Jobs) SetProgress(id string, p int) {
	if p != 0 && p != 10 && p != 50 {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if job := j.m[id]; job != nil && job.Status == "pending" {
		job.Progress = p
	}
}

// Finish marks the job done with the stored file name and the requested duration.
// Progress becomes 100. A missing id is ignored.
func (j *Jobs) Finish(id, file string, duration int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	job := j.m[id]
	if job == nil {
		return
	}
	job.Status = "done"
	job.Phase = "done"
	job.Progress = 100
	job.File = file
	job.Duration = duration
}

// Fail marks the job failed. code is the xAI error code. msg is shown to the caller.
func (j *Jobs) Fail(id, code, msg string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	job := j.m[id]
	if job == nil {
		return
	}
	job.Status = "failed"
	job.Phase = "failed"
	job.ErrCode = code
	job.ErrMsg = msg
}

// Get returns a copy of the job. ok is false when the id is unknown or older than TTL.
func (j *Jobs) Get(id string) (Job, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	job := j.m[id]
	if job == nil || j.expired(job.Created) {
		return Job{}, false
	}
	return *job, true
}

// Counts returns queued and running jobs. Done and failed are not included.
func (j *Jobs) Counts() (queued, running int) {
	if j == nil {
		return 0, 0
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, job := range j.m {
		switch job.Phase {
		case "queued":
			queued++
		case "running":
			running++
		}
	}
	return queued, running
}

// Sweep deletes records older than TTL. Pending jobs past TTL are removed too,
// so a later GET is video_not_found. The grok process, if any, is not killed here.
func (j *Jobs) Sweep() {
	if j == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	for id, job := range j.m {
		if j.expired(job.Created) {
			delete(j.m, id)
		}
	}
}

// expired reports whether created is past TTL. A zero TTL never expires.
func (j *Jobs) expired(created time.Time) bool {
	if j.TTL <= 0 {
		return false
	}
	return j.now().After(created.Add(j.TTL))
}

// newUUID returns a random 8-4-4-4-12 hex id. It is not a grok session id.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32], nil
}
