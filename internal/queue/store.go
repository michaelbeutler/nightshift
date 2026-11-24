package queue

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

var (
	// ErrNotFound is returned for an ID that names no job.
	ErrNotFound = errors.New("no such job")
	// ErrAmbiguous is returned for an ID prefix that matches several jobs.
	ErrAmbiguous = errors.New("job ID prefix matches several jobs")
	// ErrLocked is returned when a lock is held and the caller did not wait.
	ErrLocked = errors.New("locked by another nightshift process")
)

// idAlphabet is Crockford's base32 alphabet: no i, l, o or u to misread.
const idAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"

var validID = regexp.MustCompile(`^[0-9a-z]{1,32}$`)

// Store keeps jobs as files under a directory.
type Store struct {
	dir string
}

// Open opens (creating if needed) the store in dir.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "jobs"), 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	return &Store{dir: dir}, nil
}

// Dir is the state directory.
func (s *Store) Dir() string { return s.dir }

func (s *Store) path(id string) string { return filepath.Join(s.dir, "jobs", id+".json") }

// NewID returns an unused job ID.
func (s *Store) NewID() (string, error) {
	for range 16 {
		var b [6]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		id := make([]byte, len(b))
		for i, c := range b {
			id[i] = idAlphabet[int(c)%len(idAlphabet)]
		}
		if _, err := os.Stat(s.path(string(id))); errors.Is(err, fs.ErrNotExist) {
			return string(id), nil
		}
	}
	return "", errors.New("could not find an unused job ID")
}

// Save writes a job atomically: readers see the old or the new file, never
// half of one.
func (s *Store) Save(j *Job) error {
	if !validID.MatchString(j.ID) {
		return fmt.Errorf("invalid job ID %q", j.ID)
	}
	j.Updated = time.Now()
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Join(s.dir, "jobs"), ".tmp-"+j.ID+"-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path(j.ID))
}

// Load reads one job by its full ID.
func (s *Store) Load(id string) (*Job, error) {
	if !validID.MatchString(id) {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	data, err := os.ReadFile(s.path(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	var j Job
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, fmt.Errorf("job %s: %w", id, err)
	}
	return &j, nil
}

// All returns every job, ordered by when they run.
func (s *Store) All() ([]*Job, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "jobs"))
	if err != nil {
		return nil, err
	}
	jobs := make([]*Job, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		j, err := s.Load(strings.TrimSuffix(name, ".json"))
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	Sort(jobs)
	return jobs, nil
}

// Sort orders jobs by run time, then creation time, then ID.
func Sort(jobs []*Job) {
	slices.SortStableFunc(jobs, func(a, b *Job) int {
		if c := a.At.Compare(b.At); c != 0 {
			return c
		}
		if c := a.Created.Compare(b.Created); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
}

// Find returns the job whose ID is id or starts with it.
func (s *Store) Find(id string) (*Job, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	if !validID.MatchString(id) {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	if j, err := s.Load(id); err == nil {
		return j, nil
	}
	all, err := s.All()
	if err != nil {
		return nil, err
	}
	var match *Job
	for _, j := range all {
		if strings.HasPrefix(j.ID, id) {
			if match != nil {
				return nil, fmt.Errorf("%w: %s", ErrAmbiguous, id)
			}
			match = j
		}
	}
	if match == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return match, nil
}

// Remove deletes a job's file.
func (s *Store) Remove(id string) error {
	if !validID.MatchString(id) {
		return fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	err := os.Remove(s.path(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Lock takes the store lock, waiting for it if necessary. Hold it for every
// read-modify-write of the job set, and never across slow work such as a push.
func (s *Store) Lock(ctx context.Context) (unlock func(), err error) {
	return acquire(ctx, filepath.Join(s.dir, "store.lock"), true)
}

// TryLockRunner takes the runner lock if no other runner holds it.
func (s *Store) TryLockRunner(ctx context.Context) (unlock func(), err error) {
	return acquire(ctx, filepath.Join(s.dir, "runner.lock"), false)
}
