package execution

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestR6LedgerCompactsLongRunningHistory(t *testing.T) {
	path := layeredFixture(t)
	base := time.Now()
	for day := 0; day < 14; day++ {
		err := withLockedFile(t.Context(), path, nil, func(p *accountingPath) error {
			data, err := readLedger(p, 100)
			if err != nil {
				return err
			}
			now := base.Add(time.Duration(day) * 24 * time.Hour)
			// The compactor follows the ledger's monotonic accounting clock.
			keys, err := scopeKeys(data, Config{TaskID: fmt.Sprintf("day-%d", day), MaxCalls: 100}, now)
			if err != nil {
				return err
			}
			scopeSpend(data, keys, 1)
			data["attempts"] = append(data["attempts"].([]any), map[string]any{
				"id": fmt.Sprintf("attempt-%d", day), "status": "completed", "scopes": keys,
				"started_at": float64(now.Unix()), "finished_at": float64(now.Unix()),
			})
			return saveLedger(p, data)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	data := readTestLedger(t, path)
	if len(data["attempts"].([]any)) > 3 {
		t.Fatal("settled history grows forever")
	}
	scopes := data["scopes"].(map[string]any)
	if len(scopes["tasks"].(map[string]any)) > 3 {
		t.Fatal("task counters grow forever")
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() > 6000 {
		t.Fatalf("ledger size unbounded: %v %v", info, err)
	}
	encoded, _ := json.Marshal(data)
	var roundtrip map[string]any
	if err := json.Unmarshal(encoded, &roundtrip); err != nil {
		t.Fatal(err)
	}
}

func TestR6ConcurrencyLimitsIncreaseAfterQuiescence(t *testing.T) {
	path := layeredFixture(t)
	t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "1")
	t.Setenv("MAKEWAND_POOL_CONCURRENCY", "1")
	ctx := ContextWithConfig(t.Context(), Config{LedgerPath: path, MaxCalls: 100, TaskID: "raise"})
	first, err := Reserve(ctx, Metadata{Engine: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Complete(Outcome{Status: Passed, Known: true}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "2")
	t.Setenv("MAKEWAND_POOL_CONCURRENCY", "2")
	t.Setenv("MAKEWAND_LEASE_WAIT_SECONDS", "0")
	second, err := Reserve(ctx, Metadata{Engine: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := second.Complete(Outcome{Status: Passed, Known: true}); err != nil {
			t.Fatal(err)
		}
	}()
	third, err := Reserve(ctx, Metadata{Engine: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := third.Complete(Outcome{Status: Passed, Known: true}); err != nil {
			t.Fatal(err)
		}
	}()
}

func TestR6CompactionProtocolSharedWithPython(t *testing.T) {
	path := layeredFixture(t)
	// Python produces unpruned daily history. Go must validate and compact it;
	// Python then reads and rewrites that state without losing current counts.
	seed := `import json,time
from pathlib import Path
base=time.time()+20*86400
day='2099-01-14'
s={'tasks':{},'projects':{},'machine':{},'clock':base,'day':day}
d={'schema':1,'maximum':100,'scopes':s,'attempts':[]}
for n in range(14):
 key=f'2099-01-{n+1:02}'
 refs=[['tasks',f'task-{n}'],['projects','project:'+key],['machine',key]]
 for group,k in refs: s[group][k]={'maximum':100,'used':1,'last_used':base-(13-n)*86400}
 d['attempts'].append({'id':f'call-{n}','status':'completed','finished_at':base-(13-n)*86400,'scopes':refs})
Path(sys.argv[2]).write_text(json.dumps(d))
`
	if out, err := pythonCommand(t, seed, path).CombinedOutput(); err != nil {
		t.Fatalf("seed: %v %s", err, out)
	}
	if err := withLockedFile(t.Context(), path, nil, func(p *accountingPath) error {
		data, err := readLedger(p, 100)
		if err != nil {
			return err
		}
		return saveLedger(p, data)
	}); err != nil {
		t.Fatal(err)
	}
	verify := `from makewand import call_budget as b
with b._ledger(sys.argv[2],100) as (p,d):
 assert len(d['attempts']) <= 3
 assert len(d['scopes']['tasks']) <= 3
 assert d['scopes']['machine']['2099-01-14']['used']==1
 b._save(p,d)
`
	if out, err := pythonCommand(t, verify, path).CombinedOutput(); err != nil {
		t.Fatalf("Python rejected Go compacted state: %v %s", err, out)
	}
	if err := withLockedFile(t.Context(), path, nil, func(p *accountingPath) error { _, err := readLedger(p, 100); return err }); err != nil {
		t.Fatal(err)
	}
}

func TestR6UpgradeDatesOldGoCompletions(t *testing.T) {
	now := float64(time.Now().Unix())
	data := map[string]any{"attempts": []any{
		map[string]any{"id": "old", "status": "completed", "started_at": now - 4*86400, "seconds": float64(1)},
		map[string]any{"id": "long", "status": "completed", "started_at": now - 4*86400, "seconds": float64(4 * 86400)},
		map[string]any{"id": "unknown-finish", "status": "released", "started_at": now - 4*86400},
	}}
	compactLedger(data)
	entries := data["attempts"].([]any)
	used, _ := jsonInteger(data["archived_used"])
	if len(entries) != 2 || used != 1 || entries[0].(map[string]any)["id"] != "long" {
		t.Fatalf("old completion dating/refund: %v", data)
	}
}
