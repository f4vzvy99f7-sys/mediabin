package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestLedgerBasic(t *testing.T) {
	passphrase := "test-password-123"
	path := t.TempDir() + "/ledger.jsonl.age"

	ledger := &Ledger{passphrase: passphrase}
	ledger.Add(MediaEntry{
		ID:        "1",
		Title:     "Test Video",
		OriginUrl: "https://example.com/video1",
		VideoUrl:  "https://example.com/video1.mp4",
		Status:    "pending",
		Tags:      []string{"test", "video"},
	})

	if err := ledger.SyncToFile(path); err != nil {
		t.Fatalf("failed to sync to file: %v", err)
	}

	loaded, err := LoadFromFile(path, passphrase)
	if err != nil {
		t.Fatalf("failed to load from file: %v", err)
	}

	entry, ok := loaded.Get("1")
	if !ok {
		t.Fatal("failed to get entry by id")
	}
	if entry.Title != "Test Video" {
		t.Errorf("expected title 'Test Video', got %q", entry.Title)
	}
	if entry.OriginUrl != "https://example.com/video1" {
		t.Errorf("expected origin url, got %q", entry.OriginUrl)
	}
	if len(entry.Tags) != 2 {
		t.Errorf("expected 2 tags, got %d", len(entry.Tags))
	}
	if entry.TimestampCreated.IsZero() {
		t.Error("expected timestamp created to be set")
	}
}

func TestLedgerMultipleEntries(t *testing.T) {
	passphrase := "test-password-456"
	path := t.TempDir() + "/ledger2.jsonl.age"

	ledger := &Ledger{passphrase: passphrase}
	ledger.Add(MediaEntry{ID: "1", Title: "First"})
	ledger.Add(MediaEntry{ID: "2", Title: "Second"})
	ledger.Add(MediaEntry{ID: "3", Title: "Third"})

	if err := ledger.SyncToFile(path); err != nil {
		t.Fatalf("failed to sync to file: %v", err)
	}

	loaded, err := LoadFromFile(path, passphrase)
	if err != nil {
		t.Fatalf("failed to load from file: %v", err)
	}

	entries := loaded.List()
	if len(entries) != 3 {
		t.Errorf("expected 3 entries, got %d", len(entries))
	}
	if entries[0].ID != "1" || entries[1].ID != "2" || entries[2].ID != "3" {
		t.Error("entries not in expected order")
	}
}

func TestLedgerUpdate(t *testing.T) {
	passphrase := "test-password-789"
	path := t.TempDir() + "/ledger3.jsonl.age"

	ledger := &Ledger{passphrase: passphrase}
	ledger.Add(MediaEntry{ID: "1", Title: "Original", Status: "pending"})

	entry, _ := ledger.Get("1")
	entry.Status = "complete"
	ledger.Update(entry)

	if err := ledger.SyncToFile(path); err != nil {
		t.Fatalf("failed to sync to file: %v", err)
	}

	loaded, err := LoadFromFile(path, passphrase)
	if err != nil {
		t.Fatalf("failed to load from file: %v", err)
	}

	updated, _ := loaded.Get("1")
	if updated.Status != "complete" {
		t.Errorf("expected status 'complete', got %q", updated.Status)
	}
}

func TestLedgerDelete(t *testing.T) {
	passphrase := "test-password-delete"
	path := t.TempDir() + "/ledger4.jsonl.age"

	ledger := &Ledger{passphrase: passphrase}
	ledger.Add(MediaEntry{ID: "1", Title: "Keep"})
	ledger.Add(MediaEntry{ID: "2", Title: "Delete"})
	ledger.Add(MediaEntry{ID: "3", Title: "Also Keep"})

	ledger.Delete("2")

	if err := ledger.SyncToFile(path); err != nil {
		t.Fatalf("failed to sync to file: %v", err)
	}

	loaded, err := LoadFromFile(path, passphrase)
	if err != nil {
		t.Fatalf("failed to load from file: %v", err)
	}

	entries := loaded.List()
	if len(entries) != 2 {
		t.Errorf("expected 2 entries, got %d", len(entries))
	}
	if _, ok := loaded.Get("2"); ok {
		t.Error("deleted entry should not exist")
	}
}

func TestLedgerEncryption(t *testing.T) {
	passphrase := "encryption-test-pass"
	path := t.TempDir() + "/ledger5.jsonl.age"

	ledger := &Ledger{passphrase: passphrase}
	ledger.Add(MediaEntry{ID: "1", Title: "Secret Data"})

	if err := ledger.SyncToFile(path); err != nil {
		t.Fatalf("failed to sync to file: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read file: %v", err)
	}

	if string(data) == "Secret Data" || string(data) == "{\"id\":\"1\"..." {
		t.Error("file appears to be unencrypted - contains plaintext")
	}

	loaded, err := LoadFromFile(path, passphrase)
	if err != nil {
		t.Fatalf("failed to load with correct password: %v", err)
	}

	entry, _ := loaded.Get("1")
	if entry.Title != "Secret Data" {
		t.Errorf("expected 'Secret Data', got %q", entry.Title)
	}
}

func TestLedgerWrongPassword(t *testing.T) {
	passphrase := "correct-password"
	path := t.TempDir() + "/ledger6.jsonl.age"

	ledger := &Ledger{passphrase: passphrase}
	ledger.Add(MediaEntry{ID: "1", Title: "Test"})
	ledger.SyncToFile(path)

	_, err := LoadFromFile(path, "wrong-password")
	if err == nil {
		t.Error("expected error with wrong password")
	}

	_, err = LoadFromFile(path, passphrase)
	if err != nil {
		t.Errorf("should work with correct password: %v", err)
	}
}

func TestLedgerNewFile(t *testing.T) {
	passphrase := "new-file-pass"
	path := t.TempDir() + "/nonexistent.jsonl.age"

	ledger, err := LoadFromFile(path, passphrase)
	if err != nil {
		t.Fatalf("failed to load nonexistent file: %v", err)
	}

	if len(ledger.List()) != 0 {
		t.Error("expected empty ledger for new file")
	}

	ledger.Add(MediaEntry{ID: "1", Title: "First Entry"})
	if err := ledger.SyncToFile(path); err != nil {
		t.Fatalf("failed to sync new file: %v", err)
	}

	loaded, err := LoadFromFile(path, passphrase)
	if err != nil {
		t.Fatalf("failed to load new file: %v", err)
	}
	if len(loaded.List()) != 1 {
		t.Errorf("expected 1 entry, got %d", len(loaded.List()))
	}
}

func TestLedgerPreservesTimestamps(t *testing.T) {
	passphrase := "timestamp-test"
	path := t.TempDir() + "/ledger7.jsonl.age"

	ledger := &Ledger{passphrase: passphrase}
	beforeAdd := time.Now()
	ledger.Add(MediaEntry{ID: "1", Title: "Test"})
	afterAdd := time.Now()

	entry, _ := ledger.Get("1")
	if entry.TimestampCreated.Before(beforeAdd) || entry.TimestampCreated.After(afterAdd) {
		t.Error("timestamp created not set correctly on add")
	}

	if err := ledger.SyncToFile(path); err != nil {
		t.Fatalf("failed to sync: %v", err)
	}

	loaded, err := LoadFromFile(path, passphrase)
	if err != nil {
		t.Fatalf("failed to load: %v", err)
	}

	loadedEntry, _ := loaded.Get("1")
	if loadedEntry.TimestampCreated.IsZero() {
		t.Error("timestamp should be preserved after load")
	}
}

// TestLedgerMetadataPassthrough verifies that a Python-style metadata line
// is preserved verbatim across a load+sync cycle.
func TestLedgerMetadataPassthrough(t *testing.T) {
	passphrase := "meta-pass"
	path := t.TempDir() + "/ledger-meta.jsonl.age"

	// Build a ledger that already has a Python-style metadata block.
	metaJSON := `{"type":"metadata","datadir":"/some/path","num_concurrent_downloads":3}`

	ledger := &Ledger{
		passphrase:  passphrase,
		orderedIds:  []string{},
		entries:     make(map[string]MediaEntry),
		rawMetadata: json.RawMessage(metaJSON),
	}
	ledger.Add(MediaEntry{ID: "abc", Title: "Has Meta"})

	if err := ledger.SyncToFile(path); err != nil {
		t.Fatalf("failed to sync: %v", err)
	}

	loaded, err := LoadFromFile(path, passphrase)
	if err != nil {
		t.Fatalf("failed to load: %v", err)
	}

	if string(loaded.rawMetadata) != metaJSON {
		t.Errorf("rawMetadata not preserved; got %q, want %q", string(loaded.rawMetadata), metaJSON)
	}

	// Media entries should still be accessible.
	if _, ok := loaded.Get("abc"); !ok {
		t.Error("media entry not found after metadata passthrough")
	}
}

// TestLedgerMetadataOmittedWhenEmpty confirms no metadata line is written
// when the ledger was created without one.
func TestLedgerMetadataOmittedWhenEmpty(t *testing.T) {
	passphrase := "no-meta-pass"
	path := t.TempDir() + "/ledger-no-meta.jsonl.age"

	ledger := &Ledger{passphrase: passphrase}
	ledger.Add(MediaEntry{ID: "x", Title: "No Meta"})

	if err := ledger.SyncToFile(path); err != nil {
		t.Fatalf("sync failed: %v", err)
	}

	loaded, err := LoadFromFile(path, passphrase)
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}

	if len(loaded.rawMetadata) != 0 {
		t.Errorf("expected no rawMetadata, got %q", string(loaded.rawMetadata))
	}
}
