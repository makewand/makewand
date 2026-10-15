package router

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// The Go router and makewand/health.py read one catalog of provider CLI
// messages (makewand/cli_message_catalog.json, embedded here as the copy
// cli_message_catalog.json) and must reach the same verdict on the same CLI
// output. The shared corpus holds what health.py finds in each case
// (tests/test_cli_message_parity.py checks health.py against it and
// regenerates it).
const (
	pythonCLIMessageCatalogFile = "../makewand/cli_message_catalog.json"
	goCLIMessageCatalogFile     = "cli_message_catalog.json"
	cliParityCorpusFile         = "../tests/testdata/cli_message_parity.json"
)

// cliParityText is a corpus string field: a string, or strings and
// [unit, count] pairs to concatenate.
type cliParityText string

func (t *cliParityText) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		*t = cliParityText(text)
		return nil
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(data, &parts); err != nil {
		return err
	}
	var b strings.Builder
	for _, part := range parts {
		var piece string
		if err := json.Unmarshal(part, &piece); err == nil {
			b.WriteString(piece)
			continue
		}
		var run []json.RawMessage
		var unit string
		var count int
		if json.Unmarshal(part, &run) != nil || len(run) != 2 || json.Unmarshal(run[0], &unit) != nil || json.Unmarshal(run[1], &count) != nil {
			return fmt.Errorf("corpus string part %s is neither a string nor [unit, count]", part)
		}
		b.WriteString(strings.Repeat(unit, count))
	}
	*t = cliParityText(b.String())
	return nil
}

type cliParityCase struct {
	ID     string                                         `json:"id"`
	Text   *cliParityText                                 `json:"text"`
	Stdout *cliParityText                                 `json:"stdout"`
	Stderr *cliParityText                                 `json:"stderr"`
	Prompt *cliParityText                                 `json:"prompt"`
	Want   string                                         `json:"want"`
	Prov   string                                         `json:"prov"`
	Expect map[string]map[string]map[string]cliParityText `json:"expect"`
}

// expected is what health.py finds: provider -> mode -> kind -> normalized line.
func (c cliParityCase) expected(provider, mode string) map[string]string {
	found := map[string]string{}
	for modes, lines := range c.Expect[provider] {
		if slices.Contains(strings.Split(modes, ","), mode) {
			for kind, line := range lines {
				found[kind] = string(line)
			}
		}
	}
	return found
}

type cliParityCorpus struct {
	CatalogSHA256 string          `json:"catalog_sha256"`
	Cases         []cliParityCase `json:"cases"`
}

func loadCLIParityCorpus(t *testing.T) cliParityCorpus {
	t.Helper()
	raw, err := os.ReadFile(cliParityCorpusFile)
	if err != nil {
		t.Fatal(err)
	}
	var corpus cliParityCorpus
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatal(err)
	}
	if len(corpus.Cases) == 0 {
		t.Fatal("empty CLI message parity corpus")
	}
	return corpus
}

// catalogProviders lists the CLIs of the catalog file.
func catalogProviders(t *testing.T, raw []byte) []string {
	t.Helper()
	var catalog struct {
		Providers map[string]json.RawMessage `json:"providers"`
	}
	if err := json.Unmarshal(raw, &catalog); err != nil {
		t.Fatal(err)
	}
	var providers []string
	for provider := range catalog.Providers {
		providers = append(providers, provider)
	}
	slices.Sort(providers)
	return providers
}

func TestCLIMessageCatalogMirrorsPythonCatalog(t *testing.T) {
	python, err := os.ReadFile(pythonCLIMessageCatalogFile)
	if err != nil {
		t.Fatal(err)
	}
	mirror, err := os.ReadFile(goCLIMessageCatalogFile)
	if err != nil {
		t.Fatalf("the router needs the catalog: cp makewand/cli_message_catalog.json router/ (%v)", err)
	}
	if !bytes.Equal(mirror, python) {
		t.Fatal("router/cli_message_catalog.json differs from makewand/cli_message_catalog.json: copy it, then regenerate tests/testdata/cli_message_parity.json (tests/test_cli_message_parity.py)")
	}
	sum := sha256.Sum256(mirror)
	if corpus := loadCLIParityCorpus(t); corpus.CatalogSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("tests/testdata/cli_message_parity.json was generated from another catalog: regenerate it (tests/test_cli_message_parity.py)")
	}
}

// A CLI failure seals the provider's pool (ErrorKindRateLimit) exactly when
// health.py finds a quota or rate refusal of that provider's CLI in its
// stderr, for every CLI of the catalog. (formatCLIExecutionError does not know
// the prompt, so cases that give one are left to the findCLIMessages test.)
func TestFormatCLIExecutionErrorQuotaVerdictsMatchPython(t *testing.T) {
	raw, err := os.ReadFile(goCLIMessageCatalogFile)
	if err != nil {
		raw, err = os.ReadFile(pythonCLIMessageCatalogFile)
	}
	if err != nil {
		t.Fatal(err)
	}
	providers := catalogProviders(t, raw)
	exit := errors.New("exit status 1")
	checked, sealed := 0, 0
	for _, c := range loadCLIParityCorpus(t).Cases {
		if c.Text == nil || c.Prompt != nil {
			continue
		}
		for _, provider := range providers {
			expected := c.expected(provider, "stderr")
			_, quota := expected["quota"]
			_, rate := expected["rate"]
			got := ErrorKindOf(formatCLIExecutionError(provider, string(*c.Text), exit, nil, time.Second)) == ErrorKindRateLimit
			if got != (quota || rate) {
				t.Errorf("%s: %s CLI sealed=%t, health.py finds %v in stderr %q", c.ID, provider, got, expected, *c.Text)
			}
			checked++
			if got {
				sealed++
			}
		}
	}
	if checked < 3000 || sealed < 400 {
		t.Fatalf("parity corpus too thin: %d verdicts, %d sealing", checked, sealed)
	}
}
