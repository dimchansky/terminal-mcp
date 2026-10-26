package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// OpenWithMigration opens the new audit journal while holding its lock and,
// when legacy records exist, the legacy journal lock for the full lifetime of
// the new process. Older cooperating servers therefore cannot continue
// writing to the old path alongside the new one. Sources are only copied,
// never moved or deleted. The migration is resumable after a crash.
func OpenWithMigration(path, legacy string) (_ *Log, err error) {
	if legacy == "" {
		return Open(path)
	}
	destination, err := canonicalAuditPath(path)
	if err != nil {
		return nil, err
	}
	source, err := canonicalAuditPath(legacy)
	if errors.Is(err, os.ErrNotExist) {
		return Open(path)
	}
	if err != nil {
		return nil, err
	}
	if destination == source {
		return Open(path)
	}
	if filepath.Base(destination) != filepath.Base(source) {
		return nil, fmt.Errorf("legacy audit migration requires matching journal filenames")
	}
	if err := checkAuditPrivateDir(filepath.Dir(source)); err != nil {
		return nil, err
	}
	// Acquire the source lock first: old server processes already own it.
	// Never proceed while an old server might still be writing its journal.
	oldLock, err := lockAuditFile(source + ".lock")
	if err != nil {
		return nil, fmt.Errorf("legacy audit must be stopped before migration: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, oldLock.Close())
		}
	}()
	if err := checkAuditPrivateDir(filepath.Dir(destination)); err != nil {
		return nil, err
	}
	destLock, err := lockAuditFile(destination + ".lock")
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, destLock.Close())
		}
	}()
	if err = migrateAuditFamily(source, destination); err != nil {
		return nil, err
	}
	sink := &fileSink{path: destination, lock: destLock, remove: os.Remove, rename: os.Rename}
	if err = sink.open(); err != nil {
		return nil, err
	}
	l := newLog(sink)
	l.path, l.legacyLock = destination, oldLock
	return l, nil
}

func canonicalAuditPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, filepath.Base(abs)), nil
}

type migrationManifest struct {
	Source string            `json:"source"`
	Files  map[string]string `json:"files"`
}

func migrationMarker(destination string) string { return destination + ".migrated.json" }

func legacyAuditFiles(source string) ([]string, error) {
	dir, base := filepath.Dir(source), filepath.Base(source)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		// Keep every numbered archive, not just the three currently
		// configured for rotation. Exclude the persistent .lock file.
		if name != base {
			n, ok := strings.CutPrefix(name, base+".")
			if !ok {
				continue
			}
			index, err := strconv.ParseUint(n, 10, 64)
			if err != nil || index == 0 {
				continue
			}
		}
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		if err := checkAuditPrivateFile(filepath.Join(dir, name), info); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func migrateAuditFamily(source, destination string) error {
	names, err := legacyAuditFiles(source)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return nil
	}
	destDir := filepath.Dir(destination)
	marker := migrationMarker(destination)
	manifest := migrationManifest{Source: source, Files: make(map[string]string, len(names))}
	for _, name := range names {
		hash, err := hashFile(filepath.Join(filepath.Dir(source), name))
		if err != nil {
			return fmt.Errorf("hash legacy audit %q: %w", name, err)
		}
		manifest.Files[name] = hash
	}
	if _, err := os.Lstat(marker); err == nil {
		var completed migrationManifest
		if err := readPrivateMigrationMarker(marker, &completed); err != nil {
			return err
		}
		if completed.Source != source || !sameAuditHashes(completed.Files, manifest.Files) {
			return fmt.Errorf("legacy audit changed after migration; refusing split history: %s", source)
		}
		if _, err := os.Lstat(destination); err != nil {
			return fmt.Errorf("migrated active audit is missing: %w", err)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// A concurrent legacy writer cannot change the files while the old
	// .lock is held. Do not overwrite a destination that differs from it:
	// it might contain a separate history or a partial migration. In
	// particular, reject extra numbered archives that have no counterpart
	// in the legacy source; copying just the matching subset would hide a
	// previously independent destination journal.
	existingNames, err := legacyAuditFiles(destination)
	if err != nil {
		return err
	}
	for _, name := range existingNames {
		if _, present := manifest.Files[name]; !present {
			return fmt.Errorf("audit migration conflict: unexpected destination history %s", filepath.Join(destDir, name))
		}
	}
	for _, name := range names {
		origin := filepath.Join(filepath.Dir(source), name)
		target := filepath.Join(destDir, name)
		if info, err := os.Lstat(target); err == nil {
			if err := checkAuditPrivateFile(target, info); err != nil {
				return err
			}
			existing, err := hashFile(target)
			if err != nil {
				return err
			}
			if existing != manifest.Files[name] {
				return fmt.Errorf("audit migration conflict at %s; refusing overwrite", target)
			}
			continue // Safe resume of an interrupted copy.
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := copyPrivateAudit(origin, target, manifest.Files[name]); err != nil {
			return err
		}
	}
	return writeMigrationMarker(marker, manifest)
}

func sameAuditHashes(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for name, sum := range b {
		if a[name] != sum {
			return false
		}
	}
	return true
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyPrivateAudit(from, target, expectedSHA string) error {
	dir := filepath.Dir(target)
	temp, err := os.CreateTemp(dir, ".audit-migrate-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temp.Name()) }()
	if err := temp.Chmod(0600); err != nil {
		_ = temp.Close()
		return err
	}
	src, err := os.Open(from)
	if err != nil {
		_ = temp.Close()
		return err
	}
	_, err = io.Copy(temp, src)
	err = errors.Join(err, src.Close())
	if err == nil {
		err = temp.Sync()
	}
	err = errors.Join(err, temp.Close())
	if err != nil {
		return err
	}
	hash, err := hashFile(temp.Name())
	if err != nil || hash != expectedSHA {
		return fmt.Errorf("audit copy verification failed for %s: %w", from, errors.Join(err, errors.New("digest mismatch")))
	}
	// Never overwrite history written elsewhere between the existence
	// check and installation. The cooperating new audit lock is held.
	if _, err := os.Lstat(target); err == nil {
		return fmt.Errorf("audit migration target appeared during copy: %s", target)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(temp.Name(), target); err != nil {
		return err
	}
	return nil
}

func readPrivateMigrationMarker(path string, value *migrationManifest) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if err := checkAuditPrivateFile(path, info); err != nil {
		return err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(content, value)
}

func writeMigrationMarker(path string, manifest migrationManifest) error {
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".audit-manifest-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temp.Name()) }()
	if err = temp.Chmod(0600); err == nil {
		_, err = temp.Write(append(data, '\n'))
	}
	if err == nil {
		err = temp.Sync()
	}
	err = errors.Join(err, temp.Close())
	if err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("migration marker already exists: %s", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(temp.Name(), path)
}
