// Package controlplane is a small REST API over a backend.Backend:
// idempotent creates (keyed by request_id), quota checks, snapshot
// schedules executed by an async job queue with retries.
package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ayushgawai/snapvol-csi/pkg/backend"
	"github.com/ayushgawai/snapvol-csi/pkg/obs"
)

type Job struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // create-snapshot
	VolumeID string `json:"volume_id"`
	Name     string `json:"name"`
	Attempts int    `json:"attempts"`
	Status   string `json:"status"` // queued|running|done|failed
	Err      string `json:"error,omitempty"`
}

type Schedule struct {
	ID              string `json:"id"`
	VolumeID        string `json:"volume_id"`
	NamePrefix      string `json:"name_prefix"`
	IntervalSeconds int64  `json:"interval_seconds"`
}

type Server struct {
	b         backend.Backend
	quotaByte int64
	quotaVols int
	mu        sync.Mutex
	idem      map[string][]byte // request_id -> response body
	jobs      map[string]*Job
	schedules map[string]*Schedule
	queue     chan *Job
	log       *slog.Logger
	now       func() time.Time
}

func New(b backend.Backend, quotaBytes int64, quotaVolumes int) *Server {
	if quotaBytes <= 0 {
		quotaBytes = 1 << 40
	}
	if quotaVolumes <= 0 {
		quotaVolumes = 1000
	}
	s := &Server{b: b, quotaByte: quotaBytes, quotaVols: quotaVolumes,
		idem: map[string][]byte{}, jobs: map[string]*Job{}, schedules: map[string]*Schedule{},
		queue: make(chan *Job, 1024), log: slog.Default(), now: time.Now}
	go s.worker()
	go s.scheduler()
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/volumes", s.createVolume)
	mux.HandleFunc("GET /v1/volumes", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, s.b.ListVolumes()) })
	mux.HandleFunc("GET /v1/volumes/{id}", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.b.GetVolume(r.PathValue("id"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, v)
	})
	mux.HandleFunc("DELETE /v1/volumes/{id}", func(w http.ResponseWriter, r *http.Request) {
		writeErrOr(w, s.b.DeleteVolume(r.PathValue("id")))
	})
	mux.HandleFunc("POST /v1/volumes/{id}/expand", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			SizeBytes int64 `json:"size_bytes"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		v, err := s.b.ExpandVolume(r.PathValue("id"), in.SizeBytes)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, v)
	})
	mux.HandleFunc("POST /v1/snapshots", s.createSnapshot)
	mux.HandleFunc("GET /v1/snapshots", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, s.b.ListSnapshots()) })
	mux.HandleFunc("DELETE /v1/snapshots/{id}", func(w http.ResponseWriter, r *http.Request) {
		writeErrOr(w, s.b.DeleteSnapshot(r.PathValue("id")))
	})
	mux.HandleFunc("POST /v1/schedules", s.createSchedule)
	mux.HandleFunc("GET /v1/schedules", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		out := []*Schedule{}
		for _, sc := range s.schedules {
			out = append(out, sc)
		}
		writeJSON(w, 200, out)
	})
	mux.HandleFunc("GET /v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		out := []*Job{}
		for _, j := range s.jobs {
			out = append(out, j)
		}
		writeJSON(w, 200, out)
	})
	mux.HandleFunc("GET /v1/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		j, ok := s.jobs[r.PathValue("id")]
		if !ok {
			writeJSON(w, 404, map[string]string{"error": "job not found"})
			return
		}
		writeJSON(w, 200, j)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if _, err := s.b.GetVolume("__probe__"); err != nil && !errors.Is(err, backend.ErrNotFound) {
			w.WriteHeader(503)
			return
		}
		_, _ = w.Write([]byte("ready"))
	})
	mux.Handle("GET /metrics", promhttp.Handler())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		mux.ServeHTTP(w, r)
		s.log.Info("http", "method", r.Method, "path", r.URL.Path, "dur_ms", time.Since(start).Milliseconds())
	})
}

// idempotent: if request_id seen, replay stored body without re-executing.
func (s *Server) idempotent(w http.ResponseWriter, requestID string, status int, fn func() (any, error)) {
	if requestID != "" {
		s.mu.Lock()
		body, ok := s.idem[requestID]
		s.mu.Unlock()
		if ok {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Idempotent-Replay", "true")
			w.WriteHeader(status)
			_, _ = w.Write(body)
			return
		}
	}
	v, err := fn()
	if err != nil {
		writeErr(w, err)
		return
	}
	body, _ := json.Marshal(v)
	if requestID != "" {
		s.mu.Lock()
		s.idem[requestID] = body
		s.mu.Unlock()
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (s *Server) createVolume(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RequestID  string `json:"request_id"`
		Name       string `json:"name"`
		SizeBytes  int64  `json:"size_bytes"`
		SnapshotID string `json:"snapshot_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Name == "" {
		writeJSON(w, 400, map[string]string{"error": "name required"})
		return
	}
	var err error
	defer obs.Observe("api.CreateVolume", &err)()
	s.idempotent(w, in.RequestID, 201, func() (any, error) {
		if in.SizeBytes <= 0 {
			in.SizeBytes = 1 << 30
		}
		if len(s.b.ListVolumes()) >= s.quotaVols || s.b.TotalBytes()+in.SizeBytes > s.quotaByte {
			err = backend.ErrQuotaExceeded
			return nil, err
		}
		v, e := s.b.CreateVolume(in.Name, in.SizeBytes, in.SnapshotID)
		err = e
		return v, e
	})
}

func (s *Server) createSnapshot(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RequestID string `json:"request_id"`
		Name      string `json:"name"`
		VolumeID  string `json:"volume_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Name == "" || in.VolumeID == "" {
		writeJSON(w, 400, map[string]string{"error": "name and volume_id required"})
		return
	}
	s.idempotent(w, in.RequestID, 201, func() (any, error) { return s.b.CreateSnapshot(in.Name, in.VolumeID) })
}

func (s *Server) createSchedule(w http.ResponseWriter, r *http.Request) {
	var in Schedule
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.VolumeID == "" || in.IntervalSeconds <= 0 {
		writeJSON(w, 400, map[string]string{"error": "volume_id and interval_seconds required"})
		return
	}
	if _, err := s.b.GetVolume(in.VolumeID); err != nil {
		writeErr(w, err)
		return
	}
	in.ID = fmt.Sprintf("sched-%s-%d", in.VolumeID, s.now().UnixNano())
	if in.NamePrefix == "" {
		in.NamePrefix = "sched"
	}
	s.mu.Lock()
	s.schedules[in.ID] = &in
	s.mu.Unlock()
	writeJSON(w, 201, in)
}

func (s *Server) Enqueue(j *Job) *Job {
	s.mu.Lock()
	if j.ID == "" {
		j.ID = fmt.Sprintf("job-%d", s.now().UnixNano())
	}
	j.Status = "queued"
	s.jobs[j.ID] = j
	s.mu.Unlock()
	s.queue <- j
	return j
}

func (s *Server) worker() {
	for j := range s.queue {
		s.runJob(j)
	}
}

// runJob executes with up to 3 attempts, exponential backoff.
func (s *Server) runJob(j *Job) {
	s.mu.Lock()
	j.Status = "running"
	s.mu.Unlock()
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		s.mu.Lock()
		j.Attempts = attempt
		s.mu.Unlock()
		switch j.Type {
		case "create-snapshot":
			_, err = s.b.CreateSnapshot(j.Name, j.VolumeID)
		default:
			err = fmt.Errorf("unknown job type %q", j.Type)
		}
		if err == nil {
			break
		}
		obs.OpErrors.WithLabelValues("job." + j.Type).Inc()
		if attempt < 3 {
			time.Sleep(time.Duration(attempt*attempt) * 10 * time.Millisecond) // ponytail: tiny backoff, fine for local backend
		}
	}
	s.mu.Lock()
	if err != nil {
		j.Status, j.Err = "failed", err.Error()
	} else {
		j.Status = "done"
	}
	s.mu.Unlock()
}

func (s *Server) scheduler() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	last := map[string]time.Time{}
	for range t.C {
		s.mu.Lock()
		scheds := make([]*Schedule, 0, len(s.schedules))
		for _, sc := range s.schedules {
			scheds = append(scheds, sc)
		}
		s.mu.Unlock()
		for _, sc := range scheds {
			if s.now().Sub(last[sc.ID]) >= time.Duration(sc.IntervalSeconds)*time.Second {
				last[sc.ID] = s.now()
				s.Enqueue(&Job{Type: "create-snapshot", VolumeID: sc.VolumeID,
					Name: fmt.Sprintf("%s-%s-%d", sc.NamePrefix, sc.VolumeID, s.now().Unix())})
			}
		}
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	code := 500
	switch {
	case errors.Is(err, backend.ErrNotFound):
		code = 404
	case errors.Is(err, backend.ErrAlreadyExists):
		code = 409
	case errors.Is(err, backend.ErrQuotaExceeded):
		code = 429
	case errors.Is(err, backend.ErrNotImplemented):
		code = 501
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func writeErrOr(w http.ResponseWriter, err error) {
	if err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(204)
}
