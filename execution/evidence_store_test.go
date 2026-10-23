package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type retentionVectors struct {
	RetentionMinutes int            `json:"retention_minutes"`
	KeepPerAccount   int            `json:"keep_per_account"`
	BaseRecord       map[string]any `json:"base_record"`
	Files            []struct {
		Name       string         `json:"name"`
		AgeMinutes int            `json:"age_minutes"`
		Lock       bool           `json:"lock"`
		Kept       bool           `json:"kept"`
		Indexed    bool           `json:"indexed"`
		Record     map[string]any `json:"record"`
		Raw        *string        `json:"raw"`
	} `json:"files"`
	Cap struct {
		Record           map[string]any `json:"record"`
		Settled          int            `json:"settled"`
		Open             int            `json:"open"`
		NewestAgeMinutes int            `json:"newest_age_minutes"`
		Pruned           []string       `json:"pruned"`
	} `json:"cap"`
}

type retentionFile struct {
	name                string
	age                 time.Duration
	lock, kept, indexed bool
	content             []byte
}

// retentionStore expands the vectors exactly as the Python test does.
func retentionStore(t *testing.T) (retentionVectors, []retentionFile) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "tests", "testdata", "evidence_retention_cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v retentionVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	record := func(name string, layers ...map[string]any) []byte {
		value := map[string]any{"attempt_id": strings.TrimSuffix(name, ".json")}
		for _, layer := range append([]map[string]any{v.BaseRecord}, layers...) {
			for key, item := range layer {
				value[key] = item
			}
		}
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	var files []retentionFile
	for _, f := range v.Files {
		content := []byte(nil)
		if f.Raw != nil {
			content = []byte(*f.Raw)
		} else {
			content = record(f.Name, f.Record)
		}
		files = append(files, retentionFile{f.Name, time.Duration(f.AgeMinutes) * time.Minute, f.Lock, f.Kept, f.Indexed, content})
	}
	for i := range v.Cap.Settled {
		name := fmt.Sprintf("cap-settled-%03d.json", i)
		kept := !slices.Contains(v.Cap.Pruned, name)
		age := time.Duration(v.Cap.NewestAgeMinutes+v.Cap.Settled-1-i) * time.Minute
		files = append(files, retentionFile{name, age, true, kept, kept, record(name, v.Cap.Record, map[string]any{"settled": true, "result_status": "PASSED", "outcome_known": true})})
	}
	for i := range v.Cap.Open {
		name := fmt.Sprintf("cap-open-%d.json", i)
		age := time.Duration(v.Cap.NewestAgeMinutes+v.Cap.Settled+i) * time.Minute
		files = append(files, retentionFile{name, age, false, true, true, record(name, v.Cap.Record, map[string]any{"settled": false})})
	}
	return v, files
}

func writeAged(t *testing.T, path string, content []byte, at time.Time) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func readIndexLines(t *testing.T, dir string) (evidenceIndexHeader, []evidenceIndexEntry) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, evidenceIndexName))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSuffix(data, []byte{'\n'}), []byte{'\n'})
	var header evidenceIndexHeader
	if err := json.Unmarshal(lines[0], &header); err != nil {
		t.Fatalf("index header: %s: %v", lines[0], err)
	}
	var entries []evidenceIndexEntry
	for _, line := range lines[1:] {
		var entry evidenceIndexEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("index line %s: %v", line, err)
		}
		entries = append(entries, entry)
	}
	return header, entries
}

// GEC-1021-4 / PYORCH-1021-3: both runtimes prune the same store identically.
func TestEvidenceRetentionSharedVectors(t *testing.T) {
	v, files := retentionStore(t)
	if time.Duration(v.RetentionMinutes)*time.Minute != ledgerRetention || v.KeepPerAccount != evidenceKeepPerAccount {
		t.Fatal("the shared vectors and the Go retention constants disagree")
	}
	dir := t.TempDir()
	now := time.Now()
	for _, f := range files {
		writeAged(t, filepath.Join(dir, f.name), f.content, now.Add(-f.age))
		if f.lock {
			writeAged(t, filepath.Join(dir, f.name+".lock"), nil, now.Add(-f.age))
		}
	}
	if err := compactEvidence(dir, now, true); err != nil {
		t.Fatal(err)
	}
	var indexed []retentionFile
	for _, f := range files {
		_, err := os.Lstat(filepath.Join(dir, f.name))
		if exists := err == nil; exists != f.kept {
			t.Errorf("%s: present=%v, want kept=%v", f.name, exists, f.kept)
		}
		if f.lock {
			if _, err := os.Lstat(filepath.Join(dir, f.name+".lock")); (err == nil) != f.kept {
				t.Errorf("%s.lock: present=%v, want it to follow its record (kept=%v)", f.name, err == nil, f.kept)
			}
		}
		if f.kept && f.indexed {
			indexed = append(indexed, f)
		}
	}
	sort.Slice(indexed, func(i, j int) bool {
		return indexed[i].age > indexed[j].age || indexed[i].age == indexed[j].age && indexed[i].name < indexed[j].name
	})
	header, entries := readIndexLines(t, dir)
	if header.Schema != 1 || header.CompactedAt != now.UnixMilli() {
		t.Fatalf("index header %+v, want compaction at %d", header, now.UnixMilli())
	}
	var got, want []string
	for _, entry := range entries {
		got = append(got, entry.AttemptID)
		if entry.AttemptID == "other-engine-fresh" && (entry.Engine != "agy" || entry.AccountRef != "" || entry.PoolRef != "") {
			t.Errorf("index entry lost its identity: %+v", entry)
		}
	}
	for _, f := range indexed {
		want = append(want, strings.TrimSuffix(f.name, ".json"))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("index order:\n got %v\nwant %v", got, want)
	}
}

func evidenceAttempt(t *testing.T, ctx context.Context, account string) *Attempt {
	t.Helper()
	a, err := Reserve(ctx, Metadata{Engine: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.UpdateEvidence(func(e *ProviderEvidence) {
		e.AccountRef, e.IdentityKnown, e.PoolRef = account, true, account+":subscription"
	}); err != nil {
		t.Fatal(err)
	}
	return a
}

func recordIDs(records []ProviderEvidence) []string {
	var ids []string
	for _, r := range records {
		ids = append(ids, r.AttemptID)
	}
	return ids
}

// GEC-1021-4 / go-server-1021-6: reads follow the index instead of listing
// and statting the directory, and select by account before the limit.
func TestEvidenceReadsFollowTheIndex(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MAKEWAND_EXECUTION_EVIDENCE_DIR", dir)
	accountA, accountB := "codex:account:"+strings.Repeat("a", 32), "codex:account:"+strings.Repeat("b", 32)
	filterA := EvidenceFilter{Engine: "codex", AccountRef: accountA, PoolRef: accountA + ":subscription"}
	legacy := []byte(fmt.Sprintf(`{"schema":1,"task_id":"t","attempt_id":"legacy","engine":"codex","account_ref":%q,"identity_known":true,"pool_ref":%q,"source":"cli_json","observed_at_ms":1,"windows":[],"result_status":"PASSED","outcome_known":true,"process_exit_code":0,"tokens":null}`, accountA, accountA+":subscription"))
	writeAged(t, filepath.Join(dir, "legacy.json"), legacy, time.Now().Add(-time.Hour))
	// An older runner left no index: the directory is scanned as before.
	if got := recordIDs(RecentEvidence(filterA, 64)); !slices.Equal(got, []string{"legacy"}) {
		t.Fatalf("unindexed store: %v", got)
	}
	var saved []string
	for i := range 5 {
		account := accountA
		if i%2 == 1 {
			account = accountB
		}
		a := evidenceAttempt(t, t.Context(), account)
		if account == accountA {
			saved = append(saved, a.ID)
		}
		// Compaction orders by modification time; coarse filesystem clocks
		// must not reorder records saved within one tick.
		at := time.Now().Add(time.Duration(i-10) * time.Second)
		if err := os.Chtimes(filepath.Join(dir, a.ID+".json"), at, at); err != nil {
			t.Fatal(err)
		}
	}
	// Newest first, only the selected account, at most the limit. Other
	// accounts' records cannot push this account's out of the window.
	if got := recordIDs(RecentEvidence(filterA, 2)); !slices.Equal(got, []string{saved[2], saved[1]}) {
		t.Fatalf("filtered newest records: %v, saved %v", got, saved)
	}
	// Once the index exists, a record written without it is not read (no
	// directory scan) until compaction indexes it.
	if got := recordIDs(RecentEvidence(filterA, 64)); slices.Contains(got, "legacy") {
		t.Fatalf("reader listed the directory: %v", got)
	}
	if err := compactEvidence(dir, time.Now(), true); err != nil {
		t.Fatal(err)
	}
	if got := recordIDs(RecentEvidence(filterA, 64)); !slices.Equal(got, []string{saved[2], saved[1], saved[0], "legacy"}) {
		t.Fatalf("compacted index: %v", got)
	}
	// A record changed by its owner moves to the front.
	if err := os.WriteFile(filepath.Join(dir, evidenceIndexName), append(readFile(t, filepath.Join(dir, evidenceIndexName)),
		[]byte(`{"attempt_id":"legacy","engine":"codex","account_ref":"`+accountA+`","pool_ref":"`+accountA+`:subscription"}`+"\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := recordIDs(RecentEvidence(filterA, 1)); !slices.Equal(got, []string{"legacy"}) {
		t.Fatalf("latest save did not win: %v", got)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestEvidenceIndexReadsOnlyItsTail(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var image bytes.Buffer
	image.WriteString(`{"attempt_id":"oldest","engine":"codex","account_ref":"","pool_ref":""}` + "\n")
	for image.Len() <= evidenceIndexWindow {
		image.WriteString(`{"attempt_id":"filler","engine":"codex","account_ref":"","pool_ref":""}` + "\n")
	}
	image.WriteString("{torn\n" + `{"attempt_id":"../escape","engine":"codex"}` + "\n" + `{"attempt_id":"newest","engine":"codex","account_ref":"","pool_ref":""}` + "\n")
	if err := os.WriteFile(filepath.Join(dir, evidenceIndexName), image.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := readEvidenceIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for entry := range entries {
		ids = append(ids, entry.AttemptID)
	}
	if !slices.Equal(ids, []string{"newest", "filler"}) {
		t.Fatalf("tail entries: %v", ids)
	}
}

func TestEvidenceIndexImageKeepsLinesAppendedDuringCompaction(t *testing.T) {
	now := time.Now()
	records := []evidenceScanRecord{
		{name: "a.json", group: [3]string{"codex", "x", "x:subscription"}, indexed: true},
		{name: "deleted.json", group: [3]string{"codex", "x", "x:subscription"}, indexed: true},
		{name: "odd name.json", indexed: false},
		{name: "b.json", group: [3]string{"agy", "", ""}, indexed: true},
	}
	appended := []byte(`{"attempt_id":"a","engine":"codex","account_ref":"y","pool_ref":"y:subscription"}` + "\n" +
		`{"attempt_id":"new","engine":"claude","account_ref":"","pool_ref":""}` + "\n" + "{torn")
	image := evidenceIndexImage(records, map[string]bool{"deleted.json": true}, appended, now)
	want := fmt.Sprintf(`{"schema":1,"compacted_at_ms":%d}`+"\n", now.UnixMilli()) +
		`{"attempt_id":"b","engine":"agy","account_ref":"","pool_ref":""}` + "\n" +
		`{"attempt_id":"a","engine":"codex","account_ref":"y","pool_ref":"y:subscription"}` + "\n" +
		`{"attempt_id":"new","engine":"claude","account_ref":"","pool_ref":""}` + "\n"
	if string(image) != want {
		t.Fatalf("index image:\n%s\nwant:\n%s", image, want)
	}
}

func agedSettledRecord(t *testing.T, dir, id string, age time.Duration) string {
	t.Helper()
	path := filepath.Join(dir, id+".json")
	writeAged(t, path, []byte(`{"schema":1,"attempt_id":"`+id+`","engine":"codex","settled":true,"result_status":"PASSED","outcome_known":true}`), time.Now().Add(-age))
	return path
}

// Compaction runs only when due, after an attempt settles, and never deletes
// a record whose sidecar another runner holds.
func TestEvidenceCompactionIsHourlyAndSkipsHeldRecords(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	old := agedSettledRecord(t, dir, "old", 49*time.Hour)
	header := fmt.Sprintf(`{"schema":1,"compacted_at_ms":%d}`+"\n", now.Add(-10*time.Minute).UnixMilli())
	if err := os.WriteFile(filepath.Join(dir, evidenceIndexName), []byte(header), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := compactEvidence(dir, now, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("compaction ran before it was due")
	}
	p, _, err := accountingPathForIdentity(old, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	held, err := openExecutionFile(p.root, p.name+".lock", os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	if locked, err := tryLock(held); err != nil || !locked {
		t.Fatalf("fixture lock: %v", err)
	}
	later := now.Add(time.Hour)
	if err := compactEvidence(dir, later, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("compaction deleted a record whose sidecar was held")
	}
	_ = unlock(held)
	_ = held.Close()
	if header, _ := readIndexLines(t, dir); header.CompactedAt != later.UnixMilli() {
		t.Fatalf("due compaction did not rewrite the index: %+v", header)
	}
	if err := compactEvidence(dir, later.Add(time.Hour), false); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{old, old + ".lock"} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s survived its retention: %v", path, err)
		}
	}
}

func evidenceRecordAt(t *testing.T, dir, id string) ProviderEvidence {
	t.Helper()
	record, err := ReadEvidence(filepath.Join(dir, id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return record
}

// Retention may delete a record only once nothing can still reference it:
// the ledger settled the attempt and released its lease, or it has no ledger.
func TestEvidenceSettlesOnlyAfterTheLedgerReleasesTheAttempt(t *testing.T) {
	t.Run("no ledger", func(t *testing.T) {
		clearAccountingEnvironment(t)
		dir := t.TempDir()
		t.Setenv("MAKEWAND_EXECUTION_EVIDENCE_DIR", dir)
		a := evidenceAttempt(t, t.Context(), fixtureAccount)
		if evidenceRecordAt(t, dir, a.ID).Settled {
			t.Fatal("a running attempt's record claimed settlement")
		}
		if err := a.Complete(Outcome{Status: Passed, Known: true}); err != nil {
			t.Fatal(err)
		}
		if !evidenceRecordAt(t, dir, a.ID).Settled {
			t.Fatal("completed attempt without a ledger stayed open")
		}
	})
	t.Run("ledger", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("MAKEWAND_EXECUTION_EVIDENCE_DIR", dir)
		ctx := ContextWithConfig(t.Context(), Config{LedgerPath: layeredFixture(t), TaskID: "settle", MaxCalls: 10})
		a := evidenceAttempt(t, ctx, fixtureAccount)
		if evidenceRecordAt(t, dir, a.ID).Settled {
			t.Fatal("a running attempt's record claimed settlement")
		}
		if err := a.Complete(Outcome{Status: Passed, Known: true}); err != nil {
			t.Fatal(err)
		}
		if !evidenceRecordAt(t, dir, a.ID).Settled {
			t.Fatal("settled attempt's record stayed open")
		}
	})
	t.Run("held lease", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("MAKEWAND_EXECUTION_EVIDENCE_DIR", dir)
		path := layeredFixture(t)
		ctx := ContextWithConfig(t.Context(), Config{LedgerPath: path, TaskID: "fence", MaxCalls: 10})
		a, err := Reserve(ctx, Metadata{Engine: "codex", Pending: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := a.UpdateEvidence(func(*ProviderEvidence) {}); err != nil {
			t.Fatal(err)
		}
		// A launch fence without a registered worker outlives completion.
		if err := a.BeginLaunch(); err != nil {
			t.Fatal(err)
		}
		if err := a.Complete(Outcome{Status: Unknown}); err != nil {
			t.Fatal(err)
		}
		if len(readTestLedger(t, path)["leases"].(map[string]any)) != 1 {
			t.Fatal("fixture lease was released")
		}
		if evidenceRecordAt(t, dir, a.ID).Settled {
			t.Fatal("record of an attempt whose lease is still held was settled")
		}
	})
	t.Run("ledger settlement fails", func(t *testing.T) {
		clearAccountingEnvironment(t)
		dir := t.TempDir()
		t.Setenv("MAKEWAND_EXECUTION_EVIDENCE_DIR", dir)
		path := filepath.Join(t.TempDir(), "calls.json")
		a := evidenceAttempt(t, ContextWithConfig(t.Context(), Config{LedgerPath: path, MaxCalls: 5}), fixtureAccount)
		if err := withLockedFile(t.Context(), path, nil, func(p *accountingPath) error {
			return writeLedgerFile(p, map[string]any{"schema": 1, "maximum": 5, "attempts": []any{}})
		}); err != nil {
			t.Fatal(err)
		}
		// complete without the background retry: the reservation disappeared.
		if err := a.complete(Outcome{Status: Passed, Known: true}); !errors.Is(err, errReservationDisappeared) {
			t.Fatalf("settlement: %v", err)
		}
		if evidenceRecordAt(t, dir, a.ID).Settled {
			t.Fatal("record of an attempt the ledger never settled was settled")
		}
	})
}

// A terminal PASSED record can be saved before the ledger releases its lease.
// Failure of the subsequent settled=true write must still reach the caller.
type failedSettledSuccessContext struct {
	context.Context
	attempt *Attempt
	done    <-chan struct{}
}

func (c failedSettledSuccessContext) Done() <-chan struct{} {
	if c.Err() != nil {
		return c.done
	}
	return nil
}

func (c failedSettledSuccessContext) Err() error {
	if e := c.attempt.evidence; e != nil && e.Settled && e.Status == Passed {
		return context.DeadlineExceeded
	}
	return nil
}

func TestEvidenceFinalSettlementFailureDowngradesWithoutRefund(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MAKEWAND_EXECUTION_EVIDENCE_DIR", dir)
	path := layeredFixture(t)
	a := evidenceAttempt(t, ContextWithConfig(t.Context(), Config{LedgerPath: path, TaskID: "final-write", MaxCalls: 5}), fixtureAccount)
	done := make(chan struct{})
	close(done)
	a.evidenceContext = failedSettledSuccessContext{Context: context.Background(), attempt: a, done: done}
	if err := a.complete(Outcome{Status: Passed, Known: true}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("final record failure was swallowed: %v", err)
	}
	data := readTestLedger(t, path)
	entries := data["attempts"].([]any)
	if len(entries) != 1 {
		t.Fatalf("settlement changed the attempt count: %v", entries)
	}
	entry := entries[0].(map[string]any)
	if entry["status"] != "completed" || entry["result_status"] != "UNKNOWN" || entry["outcome_known"] != false {
		t.Fatalf("final record failure did not preserve a charged UNKNOWN: %v", entry)
	}
	if used := data["scopes"].(map[string]any)["tasks"].(map[string]any)["final-write"].(map[string]any)["used"]; used != float64(1) {
		t.Fatalf("final record failure changed the charge: %v", used)
	}
	if e := evidenceRecordAt(t, dir, a.ID); e.Status != Unknown || e.OutcomeKnown {
		t.Fatalf("recovered storage did not publish UNKNOWN: %+v", e)
	}
}

// Both runtimes must acquire the same stable compaction lock before pruning.
// Holding it leaves the index free but prevents a second directory scan.
func TestEvidenceCompactionLockIsSharedWithPython(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MAKEWAND_EXECUTION_EVIDENCE_DIR", dir)
	old := agedSettledRecord(t, dir, "old-shared-compaction", 49*time.Hour)
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	lock, err := openExecutionFile(root, evidenceCompactionLock, os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	locked, err := tryLock(lock)
	if err != nil || !locked {
		t.Fatalf("hold compaction lock: %v %v", locked, err)
	}
	defer func() { _ = unlock(lock) }()
	if err := compactEvidence(dir, time.Now(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("Go pruned while another compactor held the lock: %v", err)
	}
	if out, err := pythonCommand(t, "from makewand import execution_evidence as e; e.compact(force=True)").CombinedOutput(); err != nil {
		t.Fatalf("Python compaction: %v %s", err, out)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("Python pruned while Go held the compaction lock: %v", err)
	}
	if err := unlock(lock); err != nil {
		t.Fatal(err)
	}
	if err := compactEvidence(dir, time.Now(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unlocked compaction did not prune: %v", err)
	}
	deferred := agedSettledRecord(t, dir, "next-hour", 49*time.Hour)
	if out, err := pythonCommand(t, "from makewand import execution_evidence as e; e.compact()").CombinedOutput(); err != nil {
		t.Fatalf("Python hourly check: %v %s", err, out)
	}
	if _, err := os.Stat(deferred); err != nil {
		t.Fatalf("another runtime pruned twice within one hour: %v", err)
	}
}

// go-server-1021-6: a first record that cannot be written before the launch
// (an unwritable directory, a full disk) is degraded telemetry. The call keeps
// its real outcome and nothing is written.
func TestEvidenceUnwritableBeforeLaunchDegradesTelemetry(t *testing.T) {
	path := layeredFixture(t)
	var warnings []error
	ctx := ContextWithConfig(t.Context(), Config{LedgerPath: path, TaskID: "degraded", MaxCalls: 10, EventError: func(err error) { warnings = append(warnings, err) }})
	a, err := Reserve(ctx, Metadata{Engine: "codex", Pending: true})
	if err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAKEWAND_EXECUTION_EVIDENCE_DIR", filepath.Join(blocker, "child"))
	if err := a.UpdateEvidence(func(*ProviderEvidence) {}); !errors.Is(err, ErrEvidenceDegraded) || errors.Is(err, ErrUnknownOutcome) {
		t.Fatalf("first record before launch: %v", err)
	}
	if len(warnings) != 1 || !errors.Is(warnings[0], ErrEvidenceDegraded) {
		t.Fatalf("degraded telemetry was not reported once: %v", warnings)
	}
	t.Setenv("MAKEWAND_EXECUTION_EVIDENCE_DIR", t.TempDir())
	if err := a.UpdateEvidence(func(e *ProviderEvidence) { e.Model = "late" }); err != nil {
		t.Fatalf("degraded attempt wrote evidence later: %v", err)
	}
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	if err := a.Complete(Outcome{Status: Passed, Known: true}); err != nil {
		t.Fatalf("degraded telemetry changed the outcome: %v", err)
	}
	entry := readTestLedger(t, path)["attempts"].([]any)[0].(map[string]any)
	if entry["status"] != "completed" || entry["result_status"] != "PASSED" || entry["outcome_known"] != true {
		t.Fatalf("settlement: %v", entry)
	}
	if records, _ := filepath.Glob(filepath.Join(os.Getenv("MAKEWAND_EXECUTION_EVIDENCE_DIR"), "*.json")); len(records) != 0 || len(warnings) != 1 {
		t.Fatalf("degraded attempt recorded evidence: %v, warnings %v", records, warnings)
	}
}

// Once the provider may have started, a failed write still leaves the outcome
// unknown (the CLI path: the reservation started at its launch boundary).
func TestEvidenceUnwritableAfterLaunchStaysUnknown(t *testing.T) {
	path := layeredFixture(t)
	ctx := ContextWithConfig(t.Context(), Config{LedgerPath: path, TaskID: "launched", MaxCalls: 10})
	a, err := Reserve(ctx, Metadata{Engine: "codex", Pending: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAKEWAND_EXECUTION_EVIDENCE_DIR", filepath.Join(blocker, "child"))
	if err := a.UpdateEvidence(func(*ProviderEvidence) {}); err == nil || errors.Is(err, ErrEvidenceDegraded) {
		t.Fatalf("write after launch: %v", err)
	}
	if err := a.Complete(Outcome{Status: Passed, Known: true}); err == nil {
		t.Fatal("evidence failure after launch accepted as success")
	}
	entry := readTestLedger(t, path)["attempts"].([]any)[0].(map[string]any)
	if entry["result_status"] != "UNKNOWN" || entry["outcome_known"] != false {
		t.Fatalf("settlement: %v", entry)
	}
}

// spentContext's deadline passes between its first and second checks: the
// budget ran out while the transaction opened its files.
type spentContext struct {
	context.Context
	checks atomic.Int32
}

func (c *spentContext) Err() error {
	if c.checks.Add(1) > 1 {
		return context.DeadlineExceeded
	}
	return nil
}

// Native Windows flake (TestAccountingEventParentReplacementStaysOptionalAndCannotRedirect):
// the start event's 100 ms budget ran out during Windows file and ACL I/O, so
// an uncontended lock was refused and the event reported a second error. A
// deadline bounds waiting for another holder only.
func TestLockedFileTakesAFreeLockAfterItsBudgetWentToIO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	ran := false
	err := withLockedFile(&spentContext{Context: context.Background()}, path, nil, func(*accountingPath) error {
		ran = true
		return nil
	})
	if err != nil || !ran {
		t.Fatalf("free lock refused after its budget went to I/O: %v", err)
	}
	expired, cancel := context.WithDeadline(context.Background(), time.Time{})
	defer cancel()
	if err := withLockedFile(expired, path, nil, func(*accountingPath) error { return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("an already expired transaction started: %v", err)
	}
}

// Pruning removes the sidecars of the records it deletes. A writer that
// waited on a removed sidecar must lock the current one instead.
func TestLockedFileReopensASidecarReplacedWhileWaiting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "record.json")
	p, _, err := accountingPathForIdentity(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	stale, err := openExecutionFile(p.root, p.name+".lock", os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path + ".lock"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".lock", nil, 0o600); err != nil {
		t.Skipf("cannot recreate a sidecar that is still open: %v", err)
	}
	if f, err := lockOpenedSidecar(t.Context(), p, stale, true); f != nil || err != nil {
		t.Fatalf("lock of a replaced sidecar was kept: %v %v", f, err)
	}
	err = withEvidenceLockedFile(t.Context(), path, nil, func(*accountingPath) error {
		current, err := openExecutionFile(p.root, p.name+".lock", os.O_RDWR)
		if err != nil {
			return err
		}
		defer current.Close()
		if locked, err := tryLock(current); err != nil || locked {
			return fmt.Errorf("transaction does not hold the current sidecar: locked=%v %v", locked, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Both runtimes append to and read the same index: Go's records reach
// Python's quota and discovery readers, Python's reach Go's, and Python's
// compaction keeps Go's records indexed.
func TestEvidenceIndexSharedWithPython(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MAKEWAND_EXECUTION_EVIDENCE_DIR", dir)
	accountA, accountB := "codex:account:"+strings.Repeat("a", 32), "codex:account:"+strings.Repeat("b", 32)
	filterA := EvidenceFilter{Engine: "codex", AccountRef: accountA, PoolRef: accountA + ":subscription"}
	goRecord := evidenceAttempt(t, t.Context(), accountA)
	_ = evidenceAttempt(t, t.Context(), accountB)
	account, _ := json.Marshal(accountA)
	output, err := pythonCommand(t, `import json
from makewand import execution_evidence as e
print(json.dumps([r["attempt_id"] for r in e.read_recent(engine="codex", account_ref=`+string(account)+`)]))
e.save(dict(schema=1, task_id="python", attempt_id="pythonsaved", engine="codex", account_ref=`+string(account)+`,
    identity_known=True, pool_ref=`+string(account)+` + ":subscription", source="cli_json", observed_at_ms=1, windows=[],
    result_status="PASSED", outcome_known=True, tokens=None, process_exit_code=0, settled=True))`).Output()
	if err != nil {
		t.Fatalf("python: %v %s", err, output)
	}
	var seen []string
	if err := json.Unmarshal(bytes.TrimSpace(output), &seen); err != nil || !slices.Equal(seen, []string{goRecord.ID}) {
		t.Fatalf("Python read Go's index as %s (%v)", output, err)
	}
	if got := recordIDs(RecentEvidence(filterA, 64)); !slices.Equal(got, []string{"pythonsaved", goRecord.ID}) {
		t.Fatalf("Go read Python's index line as %v", got)
	}
	if output, err := pythonCommand(t, `from makewand import execution_evidence as e; e.compact(force=True)`).CombinedOutput(); err != nil {
		t.Fatalf("python compaction: %v %s", err, output)
	}
	got, want := recordIDs(RecentEvidence(filterA, 64)), []string{goRecord.ID, "pythonsaved"}
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Fatalf("after Python compaction Go reads %v", got)
	}
	if header, _ := readIndexLines(t, dir); header.Schema != 1 || header.CompactedAt == 0 {
		t.Fatalf("Python compaction header: %+v", header)
	}
}
