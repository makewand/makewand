package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Evidence retention and the read index, mirrored by makewand/execution_evidence.py.
//
// Records stay one file per attempt (<attempt_id>.json), the layout every
// runner version reads. index.jsonl lists saved records in write order, one
// JSON line per save after a header line, so readers stop after the newest
// records they need instead of listing and statting the whole directory.
// Compaction prunes settled records once they are older than the ledger's
// retention, or once newer settled records of the same engine, account and
// pool exceed evidenceKeepPerAccount, and rewrites the index.
const (
	evidenceIndexName      = "index.jsonl"
	evidenceKeepPerAccount = 256
	evidenceCompactEvery   = time.Hour
	evidenceIndexWindow    = 1 << 20
	evidenceRecordMax      = 1 << 20
	evidenceIndexBudget    = time.Second
	evidenceTempPrefix     = ".call-budget-"
)

// EvidenceFilter selects records by exact identity. An empty field matches any value.
type EvidenceFilter struct {
	Engine, AccountRef, PoolRef string
}

func (f EvidenceFilter) admits(engine, account, pool string) bool {
	return (f.Engine == "" || f.Engine == engine) &&
		(f.AccountRef == "" || f.AccountRef == account) &&
		(f.PoolRef == "" || f.PoolRef == pool)
}

type evidenceIndexEntry struct {
	AttemptID  string `json:"attempt_id"`
	Engine     string `json:"engine"`
	AccountRef string `json:"account_ref"`
	PoolRef    string `json:"pool_ref"`
}

type evidenceIndexHeader struct {
	Schema      int   `json:"schema"`
	CompactedAt int64 `json:"compacted_at_ms"`
}

// evidenceFileID reports whether id names a record file: an execution
// identifier that is one portable path component (no Windows stream colon).
func evidenceFileID(id string) bool {
	return ValidIdentifier(id) && id != "." && id != ".." && !strings.Contains(id, ":")
}

// appendEvidenceIndex records a save. It is best effort: the record is
// authoritative, and the next compaction indexes a record whose line was lost.
func appendEvidenceIndex(dir string, directory os.FileInfo, e *ProviderEvidence) {
	if !evidenceFileID(e.AttemptID) {
		return
	}
	line, err := json.Marshal(evidenceIndexEntry{e.AttemptID, e.Engine, e.AccountRef, e.PoolRef})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), evidenceIndexBudget)
	defer cancel()
	_ = withLockedFile(ctx, filepath.Join(dir, evidenceIndexName), directory, func(p *accountingPath) error {
		file, err := openExecutionFile(p.root, p.name, os.O_CREATE|os.O_APPEND|os.O_WRONLY)
		if err != nil {
			return err
		}
		_, writeErr := file.Write(append(line, '\n'))
		return errors.Join(writeErr, file.Close())
	})
}

// RecentEvidence returns up to limit records that match filter, newest first.
// It follows the index and opens only the records it returns or rejects. A
// directory without an index (no runner of this release has written to it)
// is scanned as before: the newest limit records by modification time,
// filtered afterwards.
func RecentEvidence(filter EvidenceFilter, limit int) []ProviderEvidence {
	dir, err := EvidenceDirectory()
	if err != nil || limit <= 0 {
		return nil
	}
	if canonical, err := canonicalAccountingPath(dir); err == nil {
		dir = canonical
	}
	p, err := openAccountingPath(filepath.Join(dir, evidenceIndexName), false)
	if err != nil {
		return nil
	}
	defer p.close()
	entries, err := readEvidenceIndex(p.root)
	if errors.Is(err, os.ErrNotExist) {
		return scanEvidence(p.root, filter, limit)
	}
	if err != nil {
		return nil
	}
	var records []ProviderEvidence
	for entry := range entries {
		if len(records) == limit {
			break
		}
		if !filter.admits(entry.Engine, entry.AccountRef, entry.PoolRef) {
			continue
		}
		record, err := readEvidenceAt(p.root, entry.AttemptID+".json")
		if err == nil && filter.admits(record.Engine, record.AccountRef, record.PoolRef) {
			records = append(records, record)
		}
	}
	return records
}

// readEvidenceIndex reads the last evidenceIndexWindow bytes of the index and
// yields its entries newest first, one per record, decoding lines only as the
// caller consumes them. A torn line, the header and foreign lines are skipped.
func readEvidenceIndex(root *os.Root) (iter.Seq[evidenceIndexEntry], error) {
	file, err := openExecutionFile(root, evidenceIndexName, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	start := max(0, info.Size()-evidenceIndexWindow)
	data, err := io.ReadAll(io.NewSectionReader(file, start, info.Size()-start))
	if err != nil {
		return nil, err
	}
	if start > 0 {
		// The window began inside a line.
		_, data, _ = bytes.Cut(data, []byte{'\n'})
	}
	lines := bytes.Split(data, []byte{'\n'})
	return func(yield func(evidenceIndexEntry) bool) {
		seen := make(map[string]bool)
		for i := len(lines) - 1; i >= 0; i-- {
			var entry evidenceIndexEntry
			if json.Unmarshal(lines[i], &entry) != nil || !evidenceFileID(entry.AttemptID) || seen[entry.AttemptID] {
				continue
			}
			seen[entry.AttemptID] = true
			if !yield(entry) {
				return
			}
		}
	}, nil
}

// scanEvidence reads a directory that has no index yet.
func scanEvidence(root *os.Root, filter EvidenceFilter, limit int) []ProviderEvidence {
	listing, err := listEvidenceDirectory(root)
	if err != nil {
		return nil
	}
	type evidenceFile struct {
		name string
		at   time.Time
	}
	var files []evidenceFile
	for _, name := range listing {
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		if info, err := root.Lstat(name); err == nil && info.Mode().IsRegular() {
			files = append(files, evidenceFile{name, info.ModTime()})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].at.After(files[j].at) })
	var records []ProviderEvidence
	for _, file := range files[:min(len(files), limit)] {
		record, err := readEvidenceAt(root, file.name)
		if err == nil && filter.admits(record.Engine, record.AccountRef, record.PoolRef) {
			records = append(records, record)
		}
	}
	return records
}

func listEvidenceDirectory(root *os.Root) ([]string, error) {
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	return dir.Readdirnames(-1)
}

// readEvidenceAt reads one record from the pinned evidence directory.
func readEvidenceAt(root *os.Root, name string) (ProviderEvidence, error) {
	var value ProviderEvidence
	file, err := openExecutionFile(root, name, os.O_RDONLY)
	if err != nil {
		return value, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() > evidenceRecordMax {
		return value, fmt.Errorf("invalid evidence file")
	}
	err = json.NewDecoder(io.LimitReader(file, evidenceRecordMax+1)).Decode(&value)
	if err == nil && (value.Schema != 1 || !ValidIdentifier(value.AttemptID) || !ValidIdentifier(value.Engine)) {
		err = fmt.Errorf("invalid provider evidence")
	}
	return value, err
}

type evidenceIndexState struct {
	info      os.FileInfo // nil when the index does not exist
	size      int64
	compacted int64 // header time; 0 when the first line is no header
}

func readEvidenceIndexState(root *os.Root) (evidenceIndexState, error) {
	var state evidenceIndexState
	file, err := openExecutionFile(root, evidenceIndexName, os.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	defer file.Close()
	if state.info, err = file.Stat(); err != nil {
		return state, err
	}
	state.size = state.info.Size()
	head, err := io.ReadAll(io.LimitReader(file, 512))
	if err != nil {
		return state, err
	}
	head, _, _ = bytes.Cut(head, []byte{'\n'})
	var header evidenceIndexHeader
	if json.Unmarshal(head, &header) == nil && header.Schema == 1 && header.CompactedAt > 0 {
		state.compacted = header.CompactedAt
	}
	return state, nil
}

// due reports whether compaction should run: the index is missing or has no
// header, or its last compaction is an interval old (or ahead of the clock).
func (s evidenceIndexState) due(now time.Time) bool {
	age := now.UnixMilli() - s.compacted
	return s.info == nil || s.compacted == 0 || age >= evidenceCompactEvery.Milliseconds() || age <= -evidenceCompactEvery.Milliseconds()
}

type evidenceScanRecord struct {
	name    string
	info    os.FileInfo
	group   [3]string // engine, account_ref, pool_ref
	settled bool      // prunable; every other record is kept
	indexed bool      // a schema 1 record with a valid file name
}

// classifyEvidence reads a record's retention state. Only schema 1 objects are
// managed. A record without the settled field was written before it existed:
// its runner could not mark settlement, so only an outcome other than UNKNOWN
// proves its attempt ended, and UNKNOWN records of that age are kept (they may
// be interrupted attempts that a ledger still holds open).
func classifyEvidence(data []byte) (group [3]string, settled, managed bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return group, false, false
	}
	var schema int
	if json.Unmarshal(fields["schema"], &schema) != nil || schema != 1 {
		return group, false, false
	}
	for i, key := range []string{"engine", "account_ref", "pool_ref"} {
		_ = json.Unmarshal(fields[key], &group[i])
	}
	if raw, ok := fields["settled"]; ok {
		return group, json.Unmarshal(raw, &settled) == nil && settled, true
	}
	var status string
	var known bool
	terminal := json.Unmarshal(fields["result_status"], &status) == nil && status != "" && status != string(Unknown)
	return group, terminal || json.Unmarshal(fields["outcome_known"], &known) == nil && known, true
}

// compactEvidence prunes settled records and rewrites the index when the
// index is due, or always with force. It never deletes an open record, a
// record whose sidecar is locked, or one changed since it was scanned. The
// scan and deletions run without the index lock; the rewrite takes it and
// keeps the lines appended meanwhile.
func compactEvidence(dir string, now time.Time, force bool) error {
	indexPath, directory, err := resolvePathIdentity(filepath.Join(dir, evidenceIndexName), nil)
	if err != nil {
		return err
	}
	p, err := openAccountingPath(indexPath, false)
	if err != nil {
		return err
	}
	defer p.close()
	if info, err := p.root.Stat("."); err != nil || !os.SameFile(info, directory) {
		return fmt.Errorf("execution evidence directory changed")
	}
	start, err := readEvidenceIndexState(p.root)
	if err != nil || !force && !start.due(now) {
		return err
	}
	records, deleted, err := pruneEvidence(p.root, now)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), evidenceIndexBudget)
	defer cancel()
	return withLockedFile(ctx, indexPath, directory, func(ip *accountingPath) error {
		current, err := readEvidenceIndexState(ip.root)
		if err != nil || !force && !current.due(now) {
			return err // or another runner compacted meanwhile
		}
		var appended []byte
		if current.info != nil {
			offset := int64(0)
			if start.info != nil {
				if !os.SameFile(start.info, current.info) {
					return nil // another runner replaced it
				}
				offset = start.size
			}
			file, err := openExecutionFile(ip.root, ip.name, os.O_RDONLY)
			if err != nil {
				return err
			}
			appended, err = io.ReadAll(io.NewSectionReader(file, offset, max(0, current.size-offset)))
			_ = file.Close()
			if err != nil {
				return err
			}
		}
		return replaceAccountingFile(ip, evidenceIndexImage(records, deleted, appended, now))
	})
}

// pruneEvidence scans the directory, deletes settled records older than the
// ledger retention or beyond the newest evidenceKeepPerAccount of their group,
// and removes sidecars and temporary files that outlived the retention without
// a record. It returns the scanned records, oldest first, and the deleted names.
func pruneEvidence(root *os.Root, now time.Time) ([]evidenceScanRecord, map[string]bool, error) {
	listing, err := listEvidenceDirectory(root)
	if err != nil {
		return nil, nil, err
	}
	cutoff := now.Add(-ledgerRetention)
	present := make(map[string]bool)
	var records []evidenceScanRecord
	for _, name := range listing {
		present[name] = true
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		info, err := root.Lstat(name)
		if err != nil || !info.Mode().IsRegular() || accountingReparse(info) || info.Size() > evidenceRecordMax {
			continue
		}
		data, err := readEvidenceBytes(root, name)
		if err != nil {
			continue
		}
		record := evidenceScanRecord{name: name, info: info}
		var managed bool
		record.group, record.settled, managed = classifyEvidence(data)
		if !managed {
			continue
		}
		record.indexed = evidenceFileID(strings.TrimSuffix(name, ".json"))
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		a, b := records[i].info.ModTime().UnixNano(), records[j].info.ModTime().UnixNano()
		return a < b || a == b && records[i].name < records[j].name
	})
	newer := make(map[[3]string]int)
	deleted := make(map[string]bool)
	for i := len(records) - 1; i >= 0; i-- {
		record := records[i]
		if !record.settled {
			continue
		}
		newer[record.group]++
		if (newer[record.group] > evidenceKeepPerAccount || record.info.ModTime().Before(cutoff)) && removeEvidenceRecord(root, record) {
			deleted[record.name] = true
		}
	}
	for _, name := range listing {
		temporary := strings.HasPrefix(name, evidenceTempPrefix)
		if !temporary && (!strings.HasSuffix(name, ".json.lock") || present[strings.TrimSuffix(name, ".lock")]) {
			continue
		}
		info, err := root.Lstat(name)
		if err != nil || !info.Mode().IsRegular() || accountingReparse(info) || !info.ModTime().Before(cutoff) {
			continue
		}
		if temporary {
			// A writer removes its own temporary; this one outlived its writer.
			_ = root.Remove(name)
		} else {
			removeEvidenceSidecar(root, name)
		}
	}
	return records, deleted, nil
}

func readEvidenceBytes(root *os.Root, name string) ([]byte, error) {
	file, err := openExecutionFile(root, name, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, evidenceRecordMax+1))
}

// removeEvidenceRecord deletes a record and its sidecar while holding the
// sidecar's lock. Writers check the sidecar again after locking it, so
// removing a held sidecar cannot let two writers lock different files.
func removeEvidenceRecord(root *os.Root, record evidenceScanRecord) bool {
	lock, err := openExecutionFile(root, record.name+".lock", os.O_CREATE|os.O_RDWR)
	if err != nil {
		return false
	}
	defer lock.Close()
	if locked, err := tryLock(lock); err != nil || !locked {
		return false
	}
	defer func() { _ = unlock(lock) }()
	current, err := root.Lstat(record.name)
	if err != nil || !os.SameFile(current, record.info) || !current.ModTime().Equal(record.info.ModTime()) || current.Size() != record.info.Size() {
		return false
	}
	if retryAccountingIO(func() error { return root.Remove(record.name) }) != nil {
		return false
	}
	_ = retryAccountingIO(func() error { return root.Remove(record.name + ".lock") })
	return true
}

// removeEvidenceSidecar deletes a sidecar whose record is gone, unless a
// writer holds it or recreated the record meanwhile.
func removeEvidenceSidecar(root *os.Root, name string) {
	lock, err := openExecutionFile(root, name, os.O_RDWR)
	if err != nil {
		return
	}
	defer lock.Close()
	if locked, err := tryLock(lock); err != nil || !locked {
		return
	}
	defer func() { _ = unlock(lock) }()
	if _, err := root.Lstat(strings.TrimSuffix(name, ".lock")); !errors.Is(err, os.ErrNotExist) {
		return
	}
	if held, err := lock.Stat(); err == nil {
		if current, err := root.Lstat(name); err == nil && os.SameFile(held, current) {
			_ = retryAccountingIO(func() error { return root.Remove(name) })
		}
	}
}

// evidenceIndexImage is the compacted index: a header, the scanned records
// oldest first, then the lines appended during compaction, one line per
// record and without deleted ones.
func evidenceIndexImage(records []evidenceScanRecord, deleted map[string]bool, appended []byte, now time.Time) []byte {
	var lines [][]byte
	position := make(map[string]int)
	add := func(entry evidenceIndexEntry) {
		if deleted[entry.AttemptID+".json"] {
			return
		}
		line, err := json.Marshal(entry)
		if err != nil {
			return
		}
		if previous, ok := position[entry.AttemptID]; ok {
			lines[previous] = nil
		}
		position[entry.AttemptID] = len(lines)
		lines = append(lines, line)
	}
	for _, record := range records {
		if record.indexed {
			add(evidenceIndexEntry{strings.TrimSuffix(record.name, ".json"), record.group[0], record.group[1], record.group[2]})
		}
	}
	for _, raw := range bytes.Split(appended, []byte{'\n'}) {
		var entry evidenceIndexEntry
		if json.Unmarshal(raw, &entry) == nil && evidenceFileID(entry.AttemptID) {
			add(entry)
		}
	}
	header, _ := json.Marshal(evidenceIndexHeader{Schema: 1, CompactedAt: now.UnixMilli()})
	var image bytes.Buffer
	for _, line := range append([][]byte{header}, lines...) {
		if line != nil {
			image.Write(line)
			image.WriteByte('\n')
		}
	}
	return image.Bytes()
}
