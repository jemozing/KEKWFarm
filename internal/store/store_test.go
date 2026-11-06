package store

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestFlagQueries(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "flags.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	if err := s.Add(ctx, []Item{{Flag: "FLAG%_1", Sploit: "a", Team: "two"}, {Flag: "FLAG2", Sploit: "a", Team: "two"}}, 100); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(ctx, []Item{{Flag: "FLAG%_1", Sploit: "b", Team: "one"}, {Flag: "FLAG3", Sploit: "b", Team: "one"}}, 101); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.Claim(ctx, 102, 110, 2)
	if err != nil || !slices.Equal(claimed, []Item{{Flag: "FLAG%_1", Time: 100}, {Flag: "FLAG2", Time: 100}}) {
		t.Fatalf("claimed %v, %v", claimed, err)
	}
	prefix := "ПРИНЯТ " + strings.Repeat("Я", 57)
	if err := s.Resolve(ctx, []Item{
		{Flag: "FLAG%_1", Status: "ACCEPTED", Response: prefix + " one"},
		{Flag: "FLAG2", Status: "ACCEPTED", Response: prefix + " two"},
	}, 110, 105); err != nil {
		t.Fatal(err)
	}

	cutoff := int64(100)
	items, total, err := s.List(ctx, Filter{Sploit: "a", Team: "two", Status: "ACCEPTED", Flag: "%_1", Response: "принят", Since: &cutoff, Until: &cutoff}, 10, 0)
	if err != nil || total != 1 || len(items) != 1 || items[0].Flag != "FLAG%_1" || items[0].Time != 100 {
		t.Fatalf("list returned %v, total %d, error %v", items, total, err)
	}
	items, total, err = s.List(ctx, Filter{}, 1, 0)
	if err != nil || total != 3 || len(items) != 1 || items[0].Flag != "FLAG3" {
		t.Fatalf("first page returned %v, total %d, error %v", items, total, err)
	}
	values, err := s.Distinct(ctx)
	if err != nil || !slices.Equal(values["status"], []string{"ACCEPTED", "QUEUED"}) || !slices.Equal(values["sploit"], []string{"a", "b"}) || !slices.Equal(values["team"], []string{"one", "two"}) {
		t.Fatalf("distinct returned %v, %v", values, err)
	}
	counts, err := s.Summary(ctx, Filter{Sploit: "a"}, true)
	if err != nil || !slices.Equal(counts, []Count{{Sploit: "a", Team: "two", Key: prefix, Total: 2}}) {
		t.Fatalf("summary returned %v, %v", counts, err)
	}
}

func TestLeaseAndMaintenance(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "flags.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("database has unexpected permissions %o", info.Mode().Perm())
	}
	other, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close() })

	if err := s.Add(ctx, []Item{{Flag: "old"}}, 100); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(ctx, []Item{{Flag: "new"}}, 101); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.Claim(ctx, 102, 110, 1)
	if err != nil || !slices.Equal(claimed, []Item{{Flag: "old", Time: 100}}) {
		t.Fatalf("first claim returned %v, %v", claimed, err)
	}
	claimed, err = other.Claim(ctx, 103, 111, 2)
	if err != nil || !slices.Equal(claimed, []Item{{Flag: "new", Time: 101}}) {
		t.Fatalf("second claim returned %v, %v", claimed, err)
	}
	if err := other.Resolve(ctx, []Item{{Flag: "new"}}, 111, 120); err != nil {
		t.Fatal(err)
	}
	if err := other.Maintain(ctx, 104, 101, 0); err != nil {
		t.Fatal(err)
	}
	claimed, err = other.Claim(ctx, 110, 121, 2)
	if err != nil || !slices.Equal(claimed, []Item{{Flag: "old", Time: 100}}) {
		t.Fatalf("reclaim returned %v, %v", claimed, err)
	}
	if err := s.Resolve(ctx, []Item{{Flag: "old", Status: "REJECTED"}}, 110, 112); err != nil {
		t.Fatal(err)
	}
	if err := other.Resolve(ctx, []Item{{Flag: "old", Status: "ACCEPTED"}}, 121, 112); err != nil {
		t.Fatal(err)
	}
	items, total, err := s.List(ctx, Filter{Status: "ACCEPTED"}, 10, 0)
	if err != nil || total != 1 || len(items) != 1 || items[0].Flag != "old" {
		t.Fatalf("accepted returned %v, total %d, error %v", items, total, err)
	}
	if err := s.Maintain(ctx, 112, 102, 101); err != nil {
		t.Fatal(err)
	}
	items, total, err = other.List(ctx, Filter{}, 10, 0)
	if err != nil || total != 1 || len(items) != 1 || items[0].Flag != "new" || items[0].Status != "SKIPPED" {
		t.Fatalf("after maintenance returned %v, total %d, error %v", items, total, err)
	}
}

func TestFreshFlagsAreClaimedBeforeRetries(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Add(ctx, []Item{{Flag: "retry"}}, 100); err != nil {
		t.Fatal(err)
	}
	if claimed, err := s.Claim(ctx, 101, 110, 1); err != nil || !slices.Equal(claimed, []Item{{Flag: "retry", Time: 100}}) {
		t.Fatalf("claim returned %v, %v", claimed, err)
	}
	if err := s.Resolve(ctx, []Item{{Flag: "retry"}}, 110, 103); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(ctx, []Item{{Flag: "fresh"}}, 105); err != nil {
		t.Fatal(err)
	}
	if claimed, err := s.Claim(ctx, 106, 115, 2); err != nil || !slices.Equal(claimed, []Item{{Flag: "fresh", Time: 105}, {Flag: "retry", Time: 100}}) {
		t.Fatalf("claim returned %v, %v", claimed, err)
	}
}
