package serveralerts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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

	mu      sync.Mutex
	state   map[string]stateEntry
	pending map[string]bool
	queue   chan queuedNotification
	cancel  context.CancelFunc
	done    chan struct{}
	closed  bool
	options NotifierOptions
}

// OpenWebhookNotifier creates a budget alert notifier. Empty webhook URLs
// return nil so callers can wire this in conditionally.
func OpenWebhookNotifier(webhookURL, statePath string, usageReader serverusage.Reader, teamStore serverteam.Store) (*WebhookNotifier, error) {
	return OpenWebhookNotifierWithOptions(webhookURL, statePath, usageReader, teamStore, NotifierOptions{})
}

// NotifierOptions bounds asynchronous delivery and makes failures observable.
type NotifierOptions struct {
	QueueSize    int
	Timeout      time.Duration
	MaxAttempts  int
	RetryDelay   time.Duration
	ErrorHandler func(error)
}

type queuedNotification struct {
	key, pendingKey, month string
	payload                Notification
}

func OpenWebhookNotifierWithOptions(webhookURL, statePath string, usageReader serverusage.Reader, teamStore serverteam.Store, options NotifierOptions) (*WebhookNotifier, error) {
	if options.QueueSize <= 0 {
		options.QueueSize = 64
	}
	if options.Timeout <= 0 {
		options.Timeout = 10 * time.Second
	}
	if options.MaxAttempts <= 0 {
		options.MaxAttempts = 3
	}
	if options.RetryDelay <= 0 {
		options.RetryDelay = 100 * time.Millisecond
	}
	if options.ErrorHandler == nil {
		options.ErrorHandler = func(err error) { log.Printf("budget alert delivery failed: %v", err) }
	}

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
		client:     &http.Client{Timeout: options.Timeout},
		state:      make(map[string]stateEntry),
		pending:    make(map[string]bool),
		queue:      make(chan queuedNotification, options.QueueSize),
		done:       make(chan struct{}),
		options:    options,
	}
	if err := n.loadState(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	n.cancel = cancel
	go n.run(ctx)
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
	cost, count, err := serverusage.Spend(n.usage, serverusage.CurrentMonthFilter(serverusage.Filter{ProjectID: project.ID}, now))
	if err != nil {
		return
	}
	bucket := serverteam.BuildBillingBucket(
		project.ID,
		project.Name,
		project.MonthlyBudgetUSD,
		cost,
		count,
	)
	n.notifyIfNew("project", bucket, now)
}

func (n *WebhookNotifier) observeOrganization(orgID string, now time.Time) {
	org, err := n.teams.GetOrganization(strings.TrimSpace(orgID))
	if err != nil || org == nil || org.MonthlyBudgetUSD <= 0 || !org.IsActive {
		return
	}
	cost, count, err := serverusage.Spend(n.usage, serverusage.CurrentMonthFilter(serverusage.Filter{OrgID: org.ID}, now))
	if err != nil {
		return
	}
	bucket := serverteam.BuildBillingBucket(
		org.ID,
		org.Name,
		org.MonthlyBudgetUSD,
		cost,
		count,
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

	pendingKey := key + "|" + month + "|" + alert.Severity
	n.mu.Lock()
	current := n.state[key]
	if n.closed || n.pending[pendingKey] || (current.Month == month && severityRank(current.Severity) >= severityRank(alert.Severity)) {
		n.mu.Unlock()
		return
	}
	job := queuedNotification{key: key, pendingKey: pendingKey, month: month, payload: Notification{Timestamp: now, Source: "makewand", Alert: alert}}
	select {
	case n.queue <- job:
		n.pending[pendingKey] = true
		n.mu.Unlock()
	default:
		n.mu.Unlock()
		n.options.ErrorHandler(fmt.Errorf("budget alert queue full"))
	}
}

func severityRank(severity string) int {
	switch severity {
	case "warning":
		return 1
	case "high":
		return 2
	case "critical":
		return 3
	}
	return 0
}

func (n *WebhookNotifier) run(ctx context.Context) {
	defer close(n.done)
	for {
		select {
		case <-ctx.Done():
			return
		case job, ok := <-n.queue:
			if !ok {
				return
			}
			err := n.deliver(ctx, job.payload)
			n.mu.Lock()
			delete(n.pending, job.pendingKey)
			if err == nil {
				current := n.state[job.key]
				if current.Month != job.month || severityRank(current.Severity) < severityRank(job.payload.Alert.Severity) {
					n.state[job.key] = stateEntry{Severity: job.payload.Alert.Severity, Month: job.month}
					err = n.saveStateLocked()
				}
			}
			n.mu.Unlock()
			if err != nil {
				n.options.ErrorHandler(err)
			}
		}
	}
}

func (n *WebhookNotifier) deliver(ctx context.Context, payload Notification) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	for attempt := 0; attempt < n.options.MaxAttempts; attempt++ {
		req, requestErr := http.NewRequestWithContext(ctx, http.MethodPost, n.webhookURL, bytes.NewReader(data))
		if requestErr != nil {
			return requestErr
		}
		req.Header.Set("Content-Type", "application/json")
		// Receivers can use this stable key to make retries idempotent.
		req.Header.Set("Idempotency-Key", payload.Alert.ScopeType+":"+payload.Alert.ID+":"+payload.Timestamp.UTC().Format("2006-01")+":"+payload.Alert.Severity)
		resp, sendErr := n.client.Do(req)
		if sendErr == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
			sendErr = fmt.Errorf("budget alert webhook HTTP %d", resp.StatusCode)
			if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
				return sendErr
			}
		}
		err = sendErr
		if attempt+1 < n.options.MaxAttempts {
			timer := time.NewTimer(n.options.RetryDelay * time.Duration(1<<attempt))
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	return err
}

// Flush waits for accepted notifications without closing the notifier.
func (n *WebhookNotifier) Flush(ctx context.Context) error {
	if n == nil {
		return nil
	}
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		n.mu.Lock()
		empty := len(n.pending) == 0
		n.mu.Unlock()
		if empty {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Close drains the bounded queue before stores are closed. Its deadline also
// cancels in-flight HTTP requests; no worker remains after Close returns.
func (n *WebhookNotifier) Close() error {
	if n == nil {
		return nil
	}
	n.mu.Lock()
	if !n.closed {
		n.closed = true
		close(n.queue)
	}
	n.mu.Unlock()
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	select {
	case <-n.done:
		n.cancel()
		return nil
	case <-timer.C:
		n.cancel()
		<-n.done
		return errors.New("budget alert shutdown timed out")
	}
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
