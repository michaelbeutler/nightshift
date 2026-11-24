package queue

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestStoreRoundTrip(t *testing.T) {
	t.Parallel()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 25, 2, 7, 13, 0, time.FixedZone("CET", 3600))
	var ids []string
	for i, kind := range []Kind{KindPR, KindCommit, KindPush} {
		id, err := s.NewID()
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		j := &Job{ID: id, Kind: kind, Status: StatusPending, Repo: "/r", Branch: "b", At: at.Add(time.Duration(2-i) * time.Minute)}
		if kind == KindCommit {
			j.Commit = &CommitSpec{Message: "feat: x\n\nbody\n"}
		}
		if err := s.Save(j); err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.All()
	if err != nil || len(all) != 3 {
		t.Fatalf("All = %d jobs, %v", len(all), err)
	}
	if all[0].Kind != KindPush || all[2].Kind != KindPR {
		t.Errorf("All is not ordered by time: %s %s %s", all[0].Kind, all[1].Kind, all[2].Kind)
	}
	if got := all[1].Title(); got != "feat: x" {
		t.Errorf("Title = %q", got)
	}
	if !all[0].At.Equal(at) {
		t.Errorf("At = %v, want %v", all[0].At, at)
	}
	j, err := s.Find(ids[1][:5])
	if err != nil || j.ID != ids[1] {
		t.Errorf("Find(prefix) = %v, %v", j, err)
	}
	if _, err := s.Find("zzzzzzzz"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Find(unknown) error = %v", err)
	}
	if _, err := s.Find("../../etc/passwd"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Find(path) error = %v", err)
	}
	if err := s.Remove(ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(ids[0]); !errors.Is(err, ErrNotFound) {
		t.Errorf("Load(removed) error = %v", err)
	}
	// Windows has no Unix permission bits (Go reports 0666); there the job
	// files are private because the state directory is in the user's profile.
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(filepath.Join(s.Dir(), "jobs", ids[1]+".json")); err != nil || fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("job files must be private: %v %v", fi, err)
		}
	}
}

func TestFindAmbiguous(t *testing.T) {
	t.Parallel()
	s, _ := Open(t.TempDir())
	for _, id := range []string{"abc111", "abc222"} {
		if err := s.Save(&Job{ID: id, Kind: KindPush}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Find("abc"); !errors.Is(err, ErrAmbiguous) {
		t.Errorf("Find(shared prefix) error = %v, want ErrAmbiguous", err)
	}
}

func TestRunnerLockIsExclusive(t *testing.T) {
	t.Parallel()
	s, _ := Open(t.TempDir())
	unlock, err := s.TryLockRunner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.TryLockRunner(t.Context()); !errors.Is(err, ErrLocked) {
		t.Errorf("second TryLockRunner error = %v, want ErrLocked", err)
	}
	unlock()
	again, err := s.TryLockRunner(t.Context())
	if err != nil {
		t.Fatalf("TryLockRunner after unlock: %v", err)
	}
	again()
}
