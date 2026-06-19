// migrate converts media stored in the old flat-directory layout into the
// age-filestore (agefs) format used by the current mediabin daemon.
//
// Old layout:
//
//	<objects-dir>/<hash[0:2]>/<hash[2:4]>/<hash>/
//	    video.info.json
//	    video.<ext>
//	    thumbnail.<ext>   (zero or more)
//
// New layout (agefs):
//
//	<data-dir>/<hash[0:2]>/<hash[2:4]>.zip.age
//	    containing <hash>/meta.json
//	             <hash>/principle.<ext>
//	             <hash>/thumbnail.<ext>
//
// The script is designed to be safe and resumable:
//   - A JSON state file records which hashes have been migrated (and verified).
//   - The state file is written atomically after each successful migration.
//   - Source directories are never deleted unless --delete-old is passed.
//   - --verify computes SHA-256 checksums of every file before writing and
//     compares them against what was read back from the store after writing.
//     Only then is the hash marked "verified" in the state file.
//   - Interrupted runs can be resumed: already-done hashes are skipped.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"
	agefs "github.com/MaxDillon/age-filestore/store"
	"golang.org/x/term"
)

// stateSchemaVersion is bumped if the state file format ever changes
// incompatibly, so old state files are rejected rather than silently
// misinterpreted.
const stateSchemaVersion = 1

// entryStatus values stored in the state file.
const (
	statusMigrated = "migrated" // Put succeeded; no read-back verification done.
	statusVerified = "verified" // Put succeeded AND checksums matched on read-back.
)

// migrationEntry records the outcome of a single hash's migration.
type migrationEntry struct {
	Status     string    `json:"status"`
	MigratedAt time.Time `json:"migrated_at"`
	SourcePath string    `json:"source_path"`
}

// migrationState is the top-level structure persisted to the state file.
type migrationState struct {
	SchemaVersion int                       `json:"schema_version"`
	Entries       map[string]migrationEntry `json:"entries"`
}

func loadState(path string) (*migrationState, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &migrationState{
			SchemaVersion: stateSchemaVersion,
			Entries:       make(map[string]migrationEntry),
		}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state file: %w", err)
	}
	var s migrationState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse state file: %w", err)
	}
	if s.SchemaVersion != stateSchemaVersion {
		return nil, fmt.Errorf("state file has schema version %d, expected %d — cannot continue",
			s.SchemaVersion, stateSchemaVersion)
	}
	if s.Entries == nil {
		s.Entries = make(map[string]migrationEntry)
	}
	return &s, nil
}

// saveState writes the state file atomically via a temp-file rename.
// This ensures a crash between writes never leaves a partial file.
func saveState(path string, s *migrationState) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".migrate-state-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("fsync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}
	ok = true
	return nil
}

// isVideoExt matches the same extensions as storeInAgeFS in mediabin-daemon.go.
func isVideoExt(ext string) bool {
	switch ext {
	case ".mp4", ".mkv", ".webm", ".avi", ".mov", ".m4v":
		return true
	}
	return false
}

// archiveNameFor maps a source filename to its name inside the agefs archive.
// Returns "" for the principle video file (agefs auto-detects MIME and names
// it "principle.<ext>").
func archiveNameFor(srcName string) string {
	switch {
	case srcName == "video.info.json":
		return "meta.json"
	case isVideoExt(strings.ToLower(filepath.Ext(srcName))):
		return "" // principle — let agefs.Put determine the extension
	default:
		return "thumbnail" + strings.ToLower(filepath.Ext(srcName))
	}
}

// sha256OfReader consumes r and returns its hex-encoded SHA-256 digest.
func sha256OfReader(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sha256OfSeeker computes SHA-256 over r then rewinds r to the start.
func sha256OfSeeker(r io.ReadSeeker) (string, error) {
	sum, err := sha256OfReader(r)
	if err != nil {
		return "", err
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("seek after checksum: %w", err)
	}
	return sum, nil
}

// fileRecord holds an open source file together with its migration metadata.
type fileRecord struct {
	f       *os.File
	srcName string
	name    string // archive entry name; "" means principle
	sha256  string // populated when --verify is set
}

func main() {
	objectsDirFlag := flag.String("objects-dir", "", "path to old objects directory (required)")
	dataDirFlag := flag.String("data-dir", "", "path to new agefs data directory (required)")
	stateFileFlag := flag.String("state-file", "", "path to migration state file (default: <data-dir>/.migration-state.json)")
	passphraseEnvFlag := flag.String("passphrase-env", "DB_PASSWD", "name of env var containing the passphrase (prompts if unset)")
	dryRunFlag := flag.Bool("dry-run", false, "report what would be migrated without making any changes")
	verifyFlag := flag.Bool("verify", false, "after each Put, read every file back and compare SHA-256 checksums")
	deleteOldFlag := flag.Bool("delete-old", false, "remove the source directory after a successful (and verified, if --verify) migration")
	flag.Parse()

	if *objectsDirFlag == "" || *dataDirFlag == "" {
		fmt.Fprintln(os.Stderr, "usage: migrate --objects-dir <path> --data-dir <path> [options]")
		flag.PrintDefaults()
		os.Exit(1)
	}

	passphrase := os.Getenv(*passphraseEnvFlag)
	if passphrase == "" {
		fmt.Fprint(os.Stderr, "Enter passphrase: ")
		pw, err := term.ReadPassword(int(os.Stdin.Fd()))
		if err != nil {
			// Not a terminal — try plain readline.
			fmt.Fscan(os.Stdin, &passphrase)
		} else {
			passphrase = string(pw)
		}
		fmt.Fprintln(os.Stderr)
	}
	if passphrase == "" {
		fmt.Fprintln(os.Stderr, "error: passphrase is required")
		os.Exit(1)
	}

	stateFilePath := *stateFileFlag
	if stateFilePath == "" {
		stateFilePath = filepath.Join(*dataDirFlag, ".migration-state.json")
	}

	state, err := loadState(stateFilePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// ── Discovery ────────────────────────────────────────────────────────────
	// Walk <objects-dir>/<2-char>/<2-char>/<32-char-hash>/ structure.
	var hashDirs []string
	topEntries, err := os.ReadDir(*objectsDirFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: read objects dir: %v\n", err)
		os.Exit(1)
	}
	for _, top := range topEntries {
		if !top.IsDir() || len(top.Name()) != 2 {
			continue
		}
		secondPath := filepath.Join(*objectsDirFlag, top.Name())
		secondEntries, err := os.ReadDir(secondPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: read dir %s: %v\n", secondPath, err)
			continue
		}
		for _, second := range secondEntries {
			if !second.IsDir() || len(second.Name()) != 2 {
				continue
			}
			hashLevelPath := filepath.Join(secondPath, second.Name())
			hashEntries, err := os.ReadDir(hashLevelPath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: read dir %s: %v\n", hashLevelPath, err)
				continue
			}
			for _, he := range hashEntries {
				if !he.IsDir() {
					continue
				}
				hash := he.Name()
				if err := agefs.ValidateHash(hash); err != nil {
					fmt.Fprintf(os.Stderr, "warning: skipping unexpected entry %s: %v\n", hash, err)
					continue
				}
				hashDirs = append(hashDirs, filepath.Join(hashLevelPath, hash))
			}
		}
	}

	total := len(hashDirs)
	pending := 0
	for _, dir := range hashDirs {
		hash := filepath.Base(dir)
		e, done := state.Entries[hash]
		if !done || (*verifyFlag && e.Status != statusVerified) {
			pending++
		}
	}
	fmt.Printf("Found %d hash directories  |  already done: %d  |  pending: %d\n",
		total, total-pending, pending)

	if *dryRunFlag {
		fmt.Println("Dry run — no changes made.")
		return
	}
	if pending == 0 {
		fmt.Println("Nothing to do.")
		return
	}

	// ── Migration ────────────────────────────────────────────────────────────
	migrated := 0
	errCount := 0
	for i, dir := range hashDirs {
		hash := filepath.Base(dir)

		// Skip if already at the required status.
		if e, done := state.Entries[hash]; done {
			if !*verifyFlag || e.Status == statusVerified {
				continue
			}
		}

		fmt.Printf("[%d/%d] %s ... ", i+1, total, hash)

		if err := migrateOne(hash, dir, *dataDirFlag, passphrase, *verifyFlag, state); err != nil {
			fmt.Printf("FAILED: %v\n", err)
			errCount++
			continue
		}

		if err := saveState(stateFilePath, state); err != nil {
			// Non-fatal: the data is safe; only the state file couldn't be updated.
			fmt.Printf("WARNING: state not saved: %v\n", err)
		}

		if *deleteOldFlag {
			if err := os.RemoveAll(dir); err != nil {
				fmt.Printf("WARNING: could not remove source dir: %v\n", err)
			}
		}

		migrated++
		if *verifyFlag {
			fmt.Println("ok (verified)")
		} else {
			fmt.Println("ok")
		}
	}

	fmt.Printf("\nFinished.  Migrated: %d  Errors: %d\n", migrated, errCount)
	if errCount > 0 {
		os.Exit(1)
	}
}

// migrateOne reads all files from srcDir, writes them into the agefs store
// under hash, optionally verifies checksums, and records the result in state.
func migrateOne(hash, srcDir, dataDir, passphrase string, verify bool, state *migrationState) error {
	des, err := os.ReadDir(srcDir)
	if err != nil {
		return fmt.Errorf("read dir: %w", err)
	}

	var records []fileRecord
	for _, de := range des {
		if de.IsDir() {
			continue
		}
		f, err := os.Open(filepath.Join(srcDir, de.Name()))
		if err != nil {
			closeAll(records)
			return fmt.Errorf("open %s: %w", de.Name(), err)
		}
		records = append(records, fileRecord{
			f:       f,
			srcName: de.Name(),
			name:    archiveNameFor(de.Name()),
		})
	}
	defer closeAll(records)

	if len(records) == 0 {
		return fmt.Errorf("no files found in source directory")
	}

	// Compute source checksums before the write so we can verify after.
	// sha256OfSeeker rewinds each file, so the readers are ready for Put.
	if verify {
		for i := range records {
			sum, err := sha256OfSeeker(records[i].f)
			if err != nil {
				return fmt.Errorf("checksum %s: %w", records[i].srcName, err)
			}
			records[i].sha256 = sum
		}
	}

	putFiles := make([]agefs.PutFile, len(records))
	for i, r := range records {
		putFiles[i] = agefs.PutFile{Name: r.name, Reader: r.f}
	}
	if err := agefs.Put(dataDir, hash, passphrase, putFiles); err != nil {
		return fmt.Errorf("agefs.Put: %w", err)
	}

	if verify {
		if err := verifyMigration(hash, dataDir, passphrase, records); err != nil {
			return fmt.Errorf("verification: %w", err)
		}
		state.Entries[hash] = migrationEntry{
			Status:     statusVerified,
			MigratedAt: time.Now(),
			SourcePath: srcDir,
		}
	} else {
		state.Entries[hash] = migrationEntry{
			Status:     statusMigrated,
			MigratedAt: time.Now(),
			SourcePath: srcDir,
		}
	}

	return nil
}

// verifyMigration opens the agefs store, reads back each migrated file, and
// compares its SHA-256 digest to the pre-computed source digest.
func verifyMigration(hash, dataDir, passphrase string, records []fileRecord) error {
	identity, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return fmt.Errorf("create identity: %w", err)
	}
	st, err := agefs.New(agefs.Config{
		Root:       dataDir,
		Identities: []age.Identity{identity},
	})
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	// Stat resolves the principle filename (e.g. "principle.mp4").
	he, err := st.Stat(hash)
	if err != nil {
		return fmt.Errorf("stat hash: %w", err)
	}

	for _, r := range records {
		archName := r.name
		if archName == "" {
			// Principle: use the name the store chose.
			if he.Principle == nil {
				return fmt.Errorf("expected a principle file for %s but none found in archive", r.srcName)
			}
			archName = he.Principle.Name
		}

		rc, _, err := st.Open(hash, archName)
		if err != nil {
			return fmt.Errorf("open %s: %w", archName, err)
		}
		gotSum, readErr := sha256OfReader(rc)
		rc.Close()
		if readErr != nil {
			return fmt.Errorf("read %s: %w", archName, readErr)
		}

		if gotSum != r.sha256 {
			return fmt.Errorf("SHA-256 mismatch for %s (source %s, stored %s)", r.srcName, r.sha256, gotSum)
		}
	}

	return nil
}

// closeAll closes all open file handles, ignoring errors.
func closeAll(records []fileRecord) {
	for _, r := range records {
		r.f.Close()
	}
}
