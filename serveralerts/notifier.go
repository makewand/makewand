package serveralerts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/makewand/makewand/serverteam"
	"github.com/makewand/makewand/serverusage"
)

type stateEntry struct {
	Severity string `json:"severity"`
	Month    string `json:"month"`
}

// Notification is the JSON payload delivered to webhook receivers.
type Notification struct {
	Timestamp time.Time              `json:"timestamp"`
	Source    string                 `json:"source"`
	Alert     serverteam.BudgetAlert `json:"alert"`
}

// WebhookNotifier emits budget alerts when a project or organization crosses a
// new alert severity threshold during the current month.
type WebhookNotifier struct {
	webhookURL string
	statePath  string
	usage      serverusage.Reader
	teams      serverteam.Store
	client     *http.Client

	mu    sync.Mutex
	state map[string]stateEntry
}

// OpenWebhookNotifier creates a budget alert notifier. Empty webhook URLs
// return nil so callers can wire this in conditionally.
func OpenWebhookNotifier(webhookURL, statePath string, usageReader serverusage.Reader, teamStore serverteam.Store) (*WebhookNotifier, error) {
	webhookURL = strings.TrimSpace(webhookURL)
	if webhookURL == "" {
		return nil, nil
	}
	if usageReader == nil || teamStore == nil {
		return nil, fmt.Errorf("usage reader and team store are required for alert webhooks")
	}
	n := &WebhookNotifier{
		webhookURL: webhookURL,
		statePath:  strings.TrimSpace(statePath),
		usage:      usageReader,
		teams:      teamStore,
		client:     &http.Client{Timeout: 10 * time.Second},
		state:      make(map[string]stateEntry),
	}
	if err := n.loadState(); err != nil {
		return nil, err
	}
	return n, nil
}

// Durable reports that this logger does NOT persist usage entries — it only
// observes them to fire alerts. Strict accounting must not treat a successful
// Log here as a recorded usage entry.
func (n *WebhookNotifier) Durable() bool { return false }

// Log implements serverusage.Logger. It observes budget thresholds and fires
// webhook notifications; it does not persist the entry, so it never reports a
// storage error (returns nil). Notification failures are handled internally.
func (n *WebhookNotifier) Log(entry serverusage.Entry) error {
	if n == nil || n.usage == nil || n.teams == nil {
		return nil
	}
	now := entry.Timestamp.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if entry.ProjectID != "" {
		n.observeProject(entry.ProjectID, now)
	}
	if entry.OrganizationID != "" {
		n.observeOrganization(entry.OrganizationID, now)
	}
	return nil
}

func (n *WebhookNotifier) observeProject(projectID string, now time.Time) {
	project, err := n.teams.GetProject(strings.TrimSpace(projectID))
	if err != nil || project == nil || project.MonthlyBudgetUSD <= 0 || !project.IsActive {
		return
	}
	entries, err := n.usage.Load(serverusage.CurrentMonthFilter(serverusage.Filter{ProjectID: project.ID}, now))
	if err != nil {
		return
	}
	bucket := serverteam.BuildBillingBucket(
		project.ID,
		project.Name,
		project.MonthlyBudgetUSD,
		serverusage.SummarizeEntries(entries).TotalCostUSD,
		len(entries),
	)
	n.notifyIfNew("project", bucket, now)
}

func (n *WebhookNotifier) observeOrganization(orgID string, now time.Time) {
	org, err := n.teams.GetOrganization(strings.TrimSpace(orgID))
	if err != nil || org == nil || org.MonthlyBudgetUSD <= 0 || !org.IsActive {
		return
	}
	entries, err := n.usage.Load(serverusage.CurrentMonthFilter(serverusage.Filter{OrgID: org.ID}, now))
	if err != nil {
		return
	}
	bucket := serverteam.BuildBillingBucket(
		org.ID,
		org.Name,
		org.MonthlyBudgetUSD,
		serverusage.SummarizeEntries(entries).TotalCostUSD,
		len(entries),
	)
	n.notifyIfNew("organization", bucket, now)
}

func (n *WebhookNotifier) notifyIfNew(scopeType string, bucket serverteam.BillingBucket, now time.Time) {
	alert, ok := serverteam.BuildBudgetAlert(scopeType, bucket)
	if !ok {
		return
	}
	key := scopeType + ":" + bucket.ID
	month := serverusage.MonthStart(now).Format("2006-01")

	n.mu.Lock()
	current := n.state[key]
	if current.Month == month && current.Severity == alert.Severity {
		n.mu.Unlock()
		return
	}
	n.mu.Unlock()

	payload := Notification{
		Timestamp: now,
		Source:    "makewand",
		Alert:     alert,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	req, err := http.NewRequest(http.MethodPost, n.webhookURL, bytes.NewReader(data))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.state[key] = stateEntry{Severity: alert.Severity, Month: month}
	_ = n.saveStateLocked()
}

func (n *WebhookNotifier) loadState() error {
	if n == nil || n.statePath == "" {
		return nil
	}
	data, err := os.ReadFile(n.statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	return json.Unmarshal(data, &n.state)
}

func (n *WebhookNotifier) saveStateLocked() error {
	if n == nil || n.statePath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(n.statePath), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(n.state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(n.statePath, data, 0o600)
}
