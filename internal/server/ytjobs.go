package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"os"
	"sync"
	"time"

	"github.com/nytuo/diapason-uploader/internal/library"
	"github.com/nytuo/diapason-uploader/internal/ytdlp"
)

const jobTTL = time.Hour

// Job is a yt-dlp download moving through the pipeline. Its JSON form is what
// clients poll.
type Job struct {
	ID        string `json:"id"`
	VideoID   string `json:"videoId"`
	State     string `json:"state"`            // queued | running | done | error
	Error     string `json:"error,omitempty"`  // user-facing sentence
	Code      string `json:"code,omitempty"`   // machine-readable reason
	Detail    string `json:"detail,omitempty"` // technical cause, for logs
	Path      string `json:"path,omitempty"`
	Hash      string `json:"hash,omitempty"`
	Duplicate bool   `json:"duplicate,omitempty"`

	created time.Time
}

type jobStore struct {
	mu   sync.Mutex
	jobs map[string]*Job
}

func newJobStore() *jobStore { return &jobStore{jobs: map[string]*Job{}} }

func (j *Job) snapshot() Job { return *j }

func (s *jobStore) start(videoID string) (Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for id, j := range s.jobs {
		if now.Sub(j.created) > jobTTL {
			delete(s.jobs, id)
			continue
		}
		if j.VideoID == videoID && (j.State == "queued" || j.State == "running") {
			return j.snapshot(), false
		}
	}
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	j := &Job{ID: hex.EncodeToString(b), VideoID: videoID, State: "queued", created: now}
	s.jobs[j.ID] = j
	return j.snapshot(), true
}

func (s *jobStore) get(id string) (Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return Job{}, false
	}
	return j.snapshot(), true
}

func (s *jobStore) update(id string, f func(*Job)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if j, ok := s.jobs[id]; ok {
		f(j)
	}
}

// override carries optional client-supplied tags that win over yt-dlp's.
type override struct {
	Artist, Album, Title string
}

func (s *Server) runDownload(ctx context.Context, jobID, videoID string, ov override) {
	fail := func(code ytdlp.Kind, msg, detail string) {
		log.Printf("ytdlp job %s failed kind=%s detail=%q", jobID, code, detail)
		s.jobs.update(jobID, func(j *Job) {
			j.State, j.Error, j.Code, j.Detail = "error", msg, string(code), detail
		})
	}
	s.jobs.update(jobID, func(j *Job) { j.State = "running" })

	d, err := s.yt.Download(ctx, videoID)
	if err != nil {
		if e, ok := ytdlp.AsError(err); ok {
			fail(e.Kind, e.Msg, e.Detail)
		} else {
			fail(ytdlp.KindFailed, "The server couldn't download that video.", err.Error())
		}
		return
	}
	defer d.Cleanup()

	if !library.AllowedExt(d.Ext) {
		fail(ytdlp.KindFailed, "The server downloaded a format it can't store.", d.Ext)
		return
	}
	f, err := os.Open(d.Path)
	if err != nil {
		fail(ytdlp.KindFailed, "The server couldn't read the file it just downloaded.", err.Error())
		return
	}
	defer f.Close()

	meta := library.Metadata{
		Artist:      pick(ov.Artist, d.Meta.Artist),
		Album:       pick(ov.Album, d.Meta.Album, "YouTube"),
		Title:       pick(ov.Title, d.Meta.Title),
		TrackNumber: d.Meta.Track,
		Year:        d.Meta.Year,
		Ext:         d.Ext,
	}
	res, err := s.lib.Store(meta, f)
	if err != nil {
		fail(ytdlp.KindFailed, "The server downloaded the track but couldn't save it to the library (check permissions and free space).", err.Error())
		return
	}
	if !res.Duplicate {
		s.rescan.Schedule()
	}
	s.jobs.update(jobID, func(j *Job) {
		j.State, j.Path, j.Hash, j.Duplicate = "done", res.RelPath, res.Hash, res.Duplicate
	})
	log.Printf("ytdlp job %s finished (duplicate=%v)", jobID, res.Duplicate)
}

func pick(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
