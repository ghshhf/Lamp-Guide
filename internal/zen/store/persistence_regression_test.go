package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
	"zenui/internal/zen/lane"
)

func reviewStore(t *testing.T) *Store {
	t.Helper()
	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func TestRegressionStatsSnapshotIsolation(t *testing.T) {
	s := reviewStore(t)
	rec := lane.CallRecord{At: time.Now().UnixMilli(), Model: "review-model", Agent: "review-agent", Output: 1, Ok: true}
	s.Record(rec)
	days, _ := s.SnapshotStats()
	d := days[time.UnixMilli(rec.At).Format("2006-01-02")]
	s.Record(rec)
	if d.Models[rec.Model] != 1 || d.ModelReqs[rec.Model] != 1 || d.Agents[rec.Agent] != 1 {
		t.Fatalf("snapshot changed after Record: models=%d modelReqs=%d agents=%d; want all 1", d.Models[rec.Model], d.ModelReqs[rec.Model], d.Agents[rec.Agent])
	}
}
// The failure fixtures used to occupy the fixed "stats.json.tmp" name. Now
// that writes use a unique CreateTemp file, the reliable way to force a write
// failure is to make the *destination* a directory: the atomic rename then
// fails, which is exactly the case where dirty must survive.
func TestRegressionStatsFlushRetainsDirtyOnFailure(t *testing.T) {
	s := reviewStore(t)
	s.Record(lane.CallRecord{At: time.Now().UnixMilli(), Model: "review-model", Output: 1, Ok: true})
	statsPath := filepath.Join(s.Home, "stats.json")
	if err := os.Remove(statsPath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Mkdir(statsPath, 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushStats(); err == nil {
		t.Fatal("expected write failure")
	}
	if !s.stats.dirty {
		t.Fatal("failed write cleared dirty flag; next flush will skip retry")
	}
}
func TestRegressionPerfFlushRetainsDirtyOnFailure(t *testing.T) {
	s := reviewStore(t)
	s.AddTTFTSample("review-model", 100)
	perfPath := filepath.Join(s.Home, "perf.json")
	if err := os.Remove(perfPath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Mkdir(perfPath, 0700); err != nil {
		t.Fatal(err)
	}
	s.FlushPerf()
	if !s.perfDirty {
		t.Fatal("failed write cleared perfDirty flag; next flush will skip retry")
	}
}

// A half-written config.json must abort startup with the original bytes still
// on disk. The old Open() replaced it with defaults, silently dropping the
// main key, provider keys, channel tokens and subscription URLs.
func TestCorruptConfigIsNeverOverwritten(t *testing.T) {
	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	home := Home()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "config.json")
	broken := []byte(`{"mainKey":"ofm-secret","providers":`)
	if err := os.WriteFile(path, broken, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(); err == nil {
		t.Fatal("damaged config.json must abort startup instead of being replaced")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(broken) {
		t.Fatalf("damaged config was overwritten: %q", got)
	}
}
