package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"filippo.io/age"
	"golang.org/x/sys/unix"
)

type MediaEntry struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	OriginUrl    string `json:"origin_url"`
	VideoUrl     string `json:"video_url"`
	ThumbnailUrl string `json:"thumbnail_url"`

	TimestampCreated   time.Time  `json:"timestamp_created"`
	TimestampUpdated   time.Time  `json:"timestamp_updated"`
	TimestampInstalled *time.Time `json:"timestamp_installed,omitempty"`

	ObjectPath string   `json:"object_path"`
	Status     string   `json:"status"`
	Tags       []string `json:"tags"`
}

type Ledger struct {
	orderedIds  []string
	entries     map[string]MediaEntry
	passphrase  string
	file        *os.File
	rawMetadata json.RawMessage // preserved from Python-created ledgers; written back verbatim if non-empty
	syncMu      sync.Mutex      // serializes SyncToFile calls; shutdown blocks until any in-flight sync finishes
}

func (l *Ledger) Lock(path string) error {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return fmt.Errorf("failed to open file for locking: %w", err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		file.Close()
		return fmt.Errorf("failed to acquire flock: %w", err)
	}
	l.file = file
	return nil
}

func (l *Ledger) Unlock() error {
	if l.file == nil {
		return nil
	}
	if err := unix.Flock(int(l.file.Fd()), unix.LOCK_UN); err != nil {
		return fmt.Errorf("failed to release flock: %w", err)
	}
	if err := l.file.Close(); err != nil {
		return fmt.Errorf("failed to close file: %w", err)
	}
	l.file = nil
	return nil
}

func (l *Ledger) SyncToFile(path string) error {
	l.syncMu.Lock()
	defer l.syncMu.Unlock()

	if l.passphrase == "" {
		return fmt.Errorf("passphrase is required")
	}
	if l.entries == nil {
		l.entries = make(map[string]MediaEntry)
	}
	if l.orderedIds == nil {
		l.orderedIds = []string{}
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)

	// Write back any preserved metadata block from Python-created ledgers.
	if len(l.rawMetadata) > 0 {
		buf.Write(l.rawMetadata)
		buf.WriteByte('\n')
	}

	for _, id := range l.orderedIds {
		if entry, ok := l.entries[id]; ok {
			if err := enc.Encode(entry); err != nil {
				return fmt.Errorf("failed to encode entry %s: %w", id, err)
			}
		}
	}

	// Write to a temp file in the same directory, then atomically rename it
	// over the target. Readers never see a partial write; no leftover bytes
	// from a previous longer file can corrupt the format.
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".ledger-*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmp.Name()

	success := false
	defer func() {
		if !success {
			tmp.Close()
			os.Remove(tmpPath)
		}
	}()

	recipient, err := age.NewScryptRecipient(l.passphrase)
	if err != nil {
		return fmt.Errorf("failed to create recipient: %w", err)
	}

	w, err := age.Encrypt(tmp, recipient)
	if err != nil {
		return fmt.Errorf("failed to create encryptor: %w", err)
	}

	if _, err := w.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("failed to write encrypted data: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("failed to finalize encryptor: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("failed to fsync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("failed to rename temp file: %w", err)
	}

	success = true
	return nil
}

func LoadFromFile(path string, passphrase string) (*Ledger, error) {
	if passphrase == "" {
		return nil, fmt.Errorf("passphrase is required")
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Ledger{
				orderedIds: []string{},
				entries:    make(map[string]MediaEntry),
				passphrase: passphrase,
			}, nil
		}
		return nil, fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to stat file: %w", err)
	}
	if stat.Size() == 0 {
		return &Ledger{
			orderedIds: []string{},
			entries:    make(map[string]MediaEntry),
			passphrase: passphrase,
		}, nil
	}

	identity, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return nil, fmt.Errorf("failed to create identity: %w", err)
	}

	r, err := age.Decrypt(file, identity)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt: %w", err)
	}

	ledger := &Ledger{
		orderedIds: []string{},
		entries:    make(map[string]MediaEntry),
		passphrase: passphrase,
	}

	scanner := bufio.NewScanner(r)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Bytes()

		// Peek at the "type" field. Lines with "type":"metadata" are from Python-created
		// ledgers. We preserve the raw bytes and skip decoding them as a MediaEntry.
		var typed struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &typed); err != nil {
			return nil, fmt.Errorf("failed to decode line %d: %w", lineNum, err)
		}
		if typed.Type == "metadata" {
			cp := make([]byte, len(line))
			copy(cp, line)
			ledger.rawMetadata = json.RawMessage(cp)
			continue
		}

		var entry MediaEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			return nil, fmt.Errorf("failed to decode entry at line %d: %w", lineNum, err)
		}
		ledger.orderedIds = append(ledger.orderedIds, entry.ID)
		ledger.entries[entry.ID] = entry
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed to read scanner: %w", err)
	}

	return ledger, nil
}

func (l *Ledger) Add(entry MediaEntry) {
	if l.entries == nil {
		l.entries = make(map[string]MediaEntry)
	}
	if l.orderedIds == nil {
		l.orderedIds = []string{}
	}

	entry.TimestampCreated = time.Now()
	entry.TimestampUpdated = time.Now()
	l.entries[entry.ID] = entry
	l.orderedIds = append(l.orderedIds, entry.ID)
}

func (l *Ledger) Update(entry MediaEntry) {
	if l.entries == nil {
		return
	}
	if _, ok := l.entries[entry.ID]; !ok {
		return
	}
	entry.TimestampUpdated = time.Now()
	l.entries[entry.ID] = entry
}

func (l *Ledger) Get(id string) (MediaEntry, bool) {
	if l.entries == nil {
		return MediaEntry{}, false
	}
	entry, ok := l.entries[id]
	return entry, ok
}

func (l *Ledger) List() []MediaEntry {
	if l.orderedIds == nil {
		return []MediaEntry{}
	}
	result := make([]MediaEntry, 0, len(l.orderedIds))
	for _, id := range l.orderedIds {
		if entry, ok := l.entries[id]; ok {
			result = append(result, entry)
		}
	}
	return result
}

func (l *Ledger) Delete(id string) {
	if l.entries == nil {
		return
	}
	delete(l.entries, id)
	newOrderedIds := make([]string, 0)
	for _, oid := range l.orderedIds {
		if oid != id {
			newOrderedIds = append(newOrderedIds, oid)
		}
	}
	l.orderedIds = newOrderedIds
}
