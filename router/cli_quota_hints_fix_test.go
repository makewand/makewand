package router

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// cliQuotaRefusals are quota, usage-limit and request-rate refusals as each
// provider CLI prints them (strings of the installed Claude Code 2.1.293,
// codex-linux-x64 0.162.1, Grok Build 1.0.50, Muse Code 1.4.6 and agy 1.3.2
// binaries), with the prefixes each puts in front of its own errors. "gemini"
// is Gemini CLI, which relays the same Google API refusals as agy.
var cliQuotaRefusals = []struct{ cli, text string }{
	{"claude", "You've hit your limit · resets 8pm (Asia/Shanghai)"},
	{"claude", "You've hit your session limit · resets 3pm (Asia/Shanghai)"},
	{"claude", "You've hit your weekly limit · resets Oct 20, 8pm (Asia/Shanghai)"},
	{"claude", "You've reached your Fable limit."},
	{"claude", "You've hit your monthly spend limit."},
	{"claude", "You've hit your channel's monthly spend limit."},
	{"claude", "You've hit your team's shared budget. Switch to another model to continue."},
	{"claude", "You're out of usage credits. Switch to another model to continue."},
	{"claude", "Your org is out of usage · add funds to continue"},
	{"claude", "Opus 4 requires usage credits. Run /usage-credits to continue."},
	{"claude", "Usage limit reached · limit resets 8pm"},
	{"claude", "Claude AI usage limit reached|1760000000"},
	{"claude", "Claude usage limit reached. Your limit resets at 3pm."},
	{"claude", "5-hour limit reached ∙ resets 3pm"},
	{"claude", "Monthly spend limit reached"},
	{"claude", `API Error: 429 {"type":"error","error":{"type":"rate_limit_error","message":"Rate limit exceeded"}}`},
	{"claude", "API Error: Request rejected (429) · this may be a temporary capacity issue."},
	{"claude", "API Error: Rate limit exceeded"},
	{"claude", "Error: rate limit exceeded"},
	{"claude", "API Error: HTTP 429 Too Many Requests"},
	{"codex", "ERROR: You’ve hit your usage limit. Upgrade to Pro (https://chatgpt.com/explore/pro), visit https://chatgpt.com/settings/usage to purchase more credits or try again at Oct 20th, 2026 8:00 PM."},
	{"codex", "ERROR: You’ve hit your usage limit for GPT-6 Sol. Switch to another model now, or try again at 8:00 PM."},
	{"codex", "ERROR: Usage limit reached. You've reached your usage limit. Increase your limits to continue using codex."},
	{"codex", "ERROR: Quota exceeded. Check your plan and billing details."},
	{"codex", "ERROR: You're out of credits. Your workspace is out of credits. Add credits to continue using Codex."},
	{"codex", "ERROR: Your workspace is out of credits. Ask your workspace owner to refill in order to continue."},
	{"codex", "ERROR: You hit your spend cap set in your workspace. Increase your spend cap to continue."},
	{"codex", "ERROR: You’ve reached your workspace credit limit"},
	{"codex", "ERROR: exceeded retry limit, last status: 429 Too Many Requests, request id: req_1"},
	{"codex", `ERROR: unexpected status 429 Too Many Requests: {"error":{"code":"insufficient_quota"}}`},
	{"codex", "ERROR: stream disconnected before completion: Rate limit reached for gpt-6 in organization org-x on tokens per min (TPM): Limit 30000, Used 30000. Please try again in 2s."},
	{"codex", "[2026-10-12T08:00:00] ERROR: You've hit your usage limit. Try again in 3 days 2 hours."},
	{"codex", "2026-10-12T08:00:00.123456Z ERROR codex_core::codex: Turn error: You've hit your usage limit. Try again at 8:00 PM."},
	{"codex", "Error: turn failed\n\nCaused by:\n    You’ve hit your usage limit. Try again at 8:00 PM."},
	{"codex", "OpenAI Codex v0.162.1 (research preview)\n--------\nworkdir: /repo\n--------\nuser\nreview this\n\ncodex\nI will run the tests.\n\nERROR: You've hit your usage limit. Try again at 8:00 PM."},
	{"grok", "API error (status 429): You’ve hit the rate limit for your plan. Upgrade your account or try again later."},
	{"grok", "Error: API error (status 429): You’ve hit your team’s API rate limit. Ask a team admin to purchase more credits for higher limits, or try again later."},
	{"grok", "You’ve reached your free Grok Build usage limit for now. Get SuperGrok for much higher limits, or try again later: https://grok.com/supergrok?referrer=grok-build"},
	{"grok", "You hit your free usage limit."},
	{"grok", "You hit your weekly limit."},
	{"grok", "You’ve hit the credit limit for your plan."},
	{"grok", "You’ve hit your spending cap."},
	{"grok", "Error: Your account is out of credits or over your spending limit. Add credits and retry."},
	{"grok", "Rate limited (429): You've hit the rate limit for your plan."},
	{"grok", "Error: failed after 3 retries: request error: Rate limited (429)"},
	{"grok", "Error: prompt failed\n\nCaused by:\n    API error (status 429): You’ve hit the rate limit for your plan."},
	{"muse", "Error: usage limit reached (HTTP 429 · attempt 1)"},
	{"muse", "usage limit reached (HTTP 429 · attempt 3)"},
	{"muse", "rate limited (HTTP 429 · attempt 2)"},
	{"muse", "request failed (HTTP 429 · attempt 1)"},
	{"agy", "Error: RESOURCE_EXHAUSTED (code 429)"},
	{"agy", "Error: RESOURCE_EXHAUSTED (code 429): You have exhausted your quota on this model."},
	{"agy", "You have exhausted your quota on this model."},
	{"agy", "E1012 08:11:57.633062  188 client.go:224] retrying after 429\nError: You have exhausted your quota on this model."},
	{"agy", "Error: rpc error: code = ResourceExhausted desc = You have exhausted your quota on this model."},
	{"gemini", "Error: Error 429, Message: Resource has been exhausted (e.g. check quota)., Status: RESOURCE_EXHAUSTED"},
	{"gemini", "Error: googleapi: Error 429: Quota exceeded for quota metric 'Generate Content API requests per minute'"},
}

// cliQuotaNoise is task content and diagnostics that merely mention limits:
// tool output, the model's prose and code, retry logs. No CLI prints it as its
// own refusal.
var cliQuotaNoise = []string{
	"[ERROR] Rate limit exceeded for client 10.0.0.1",
	"HTTP/2 429",
	"HTTP/1.1 429 Too Many Requests",
	"Too many requests from a single IP are now rejected with HTTP 429.",
	"Too many requests in a short window now trigger the limiter.",
	"429 passed",
	"===== 429 passed in 3.2s =====",
	"line 429",
	`  File "app.py", line 429, in handler`,
	"429 Too Many Requests",
	"Rate limited (429) responses are now retried with backoff.",
	"Rate limit exceeded: the gh CLI refused my request, so I skipped the PR step.",
	"Usage limit reached for the free tier now shows an upgrade banner.",
	"Quota exceeded for a project now raises QuotaError.",
	"Error: Request failed with status code 429",
	"Error: Too many requests",
	"✗ Error: quota exceeded for this project",
	"gh: API rate limit exceeded for user ID 1234.",
	"npm ERR! 429 Too Many Requests - GET https://registry.npmjs.org/x",
	"Added a rate limit to the login endpoint; resets at midnight",
	"Implemented the per-user usage limit check",
	"The weekly limit reached its maximum value in the test.",
	"exec\nbash -lc 'curl -si https://api.github.com/rate_limit' in /repo\n succeeded in 0.4s:\nHTTP/2 429\nx-ratelimit-remaining: 0",
	"Here is the handler:\n```\nRate limit exceeded\nYou've hit your limit\n```\nDone.",
	"def check():\n    raise RateLimitError('Rate limit exceeded')",
	"E1012 08:11:57.633062  188 client.go:224] RESOURCE_EXHAUSTED (code 429): retrying",
	"2026-10-12T08:00:00.123Z WARN codex_core::client: unexpected status 429 Too Many Requests; retrying",
	// Words no CLI prints as a refusal on their own.
	"You have reached your quota of 5 free builds on the vendor plan.",
	"Quota exceeded.",
	"ERROR: Quota exceeded.",
	"insufficient_quota",
	"insufficient_quota is the error type OpenAI returns when credits run out",
	"ERROR: insufficient_quota",
	"You've hit your weekly allowance.",
	// Muse Code's /feedback upload receipt, not its verdict.
	"error: rate limited (retry after 30s)",
	"Not uploaded: rate limited (retry after 30s)",
	// Capacity refusals are transient, not an exhausted pool.
	`API Error: 529 {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
	"API Error: Server is temporarily limiting requests (not your usage limit) · this may be a temporary capacity issue.",
}

var cliQuotaProviders = []string{"claude", "codex", "grok", "muse", "agy", "gemini"}

func TestLooksLikeQuotaExhaustionDetectsEachCLIsOwnRefusals_QuotaHints(t *testing.T) {
	for _, tc := range cliQuotaRefusals {
		for name, text := range map[string]string{
			"as printed":         tc.text,
			"ascii apostrophes":  strings.ReplaceAll(tc.text, "’", "'"),
			"curly apostrophes":  strings.ReplaceAll(tc.text, "'", "’"),
			"after other output": "Loaded config from ~/.config\n\n" + tc.text + "\n",
			"colored":            "\x1b[1;31m" + strings.ReplaceAll(tc.text, ": ", ":\x1b[0m ") + "\x1b[0m",
			"crlf":               strings.ReplaceAll(tc.text, "\n", "\r\n") + "\r\n",
		} {
			if !looksLikeQuotaExhaustion(tc.cli, text) {
				t.Errorf("%s refusal (%s) not detected: %q", tc.cli, name, text)
			}
		}
	}
}

func TestLooksLikeQuotaExhaustionIgnoresTaskContent_QuotaHints(t *testing.T) {
	for _, text := range cliQuotaNoise {
		for name, variant := range map[string]string{
			"alone":            text,
			"after an error":   "Error: exit status 1\n" + text,
			"curly apostrophe": strings.ReplaceAll(text, "'", "’"),
			"colored":          "\x1b[33m" + text + "\x1b[0m",
		} {
			for _, cli := range cliQuotaProviders {
				if looksLikeQuotaExhaustion(cli, variant) {
					t.Errorf("task content (%s) read as a %s refusal: %q", name, cli, variant)
				}
			}
		}
	}
}

// A CLI's formats count for that CLI only: what one CLI prints itself is task
// content in another CLI's output (round-2 verifier corpus).
func TestLooksLikeQuotaExhaustionIsProviderSpecific_QuotaHints(t *testing.T) {
	for _, tc := range []struct {
		text   string
		owners []string // the CLIs that print it as their own refusal
	}{
		{"You've hit your GitHub API rate limit, so I could not open the PR.", []string{"claude"}},
		{"Rate limit exceeded", []string{"claude"}},
		{"ERROR: rate limit exceeded", []string{"claude"}},
		{"Error: 429 Too Many Requests", []string{"codex"}},
		{"You've hit your limit · resets 8pm", []string{"claude"}},
		{"Error: You have exhausted your quota on this model.", []string{"agy", "gemini"}},
		{"ERROR: (gcloud.ai.models.predict) RESOURCE_EXHAUSTED: Quota exceeded for aiplatform.googleapis.com/generate_content_requests_per_minute_per_project_per_base_model with base model: gemini-pro.", []string{"agy", "gemini"}},
		{"usage limit reached (HTTP 429 · attempt 3)", []string{"muse"}},
		{"You hit your weekly limit.", []string{"grok"}},
		{"ERROR: You've hit your usage limit. Try again at 8:00 PM.", []string{"claude", "codex"}},
	} {
		for _, cli := range cliQuotaProviders {
			want := false
			for _, owner := range tc.owners {
				want = want || owner == cli
			}
			if got := looksLikeQuotaExhaustion(cli, tc.text); got != want {
				t.Errorf("%s: %q read as its own refusal = %t, want %t", cli, tc.text, got, want)
			}
		}
	}
}

// codex exec writes a transcript to stderr: the agent's messages and the
// output of the commands it ran are task content; only its own "ERROR:"
// events count (round-2 verifier: prose and relayed tool output sealed codex
// for 30 minutes).
func TestCodexTranscriptContentNeverSeals_QuotaHints(t *testing.T) {
	exit := errors.New("exit status 1")
	for _, text := range []string{
		"[2026-10-12T08:00:03] codex\nYou’ve hit your GitHub API rate limit, so I could not open the PR.\n[2026-10-12T08:00:04] tokens used: 1234\nERROR: stream disconnected before completion: error sending request",
		"codex\nYou've hit your GitHub API rate limit, so I could not open the PR.\nERROR: turn failed",
		"exec\nbash -lc 'gcloud ai models predict' in /repo\n exited 1 in 2.1s:\nERROR: (gcloud.ai.models.predict) RESOURCE_EXHAUSTED: Quota exceeded for aiplatform.googleapis.com/generate_content_requests_per_minute_per_project_per_base_model with base model: gemini-pro. Please submit a quota increase request.\nERROR: stream disconnected before completion: error sending request",
		"codex\nThe server answered:\nRate limit exceeded\nso I added a backoff.\nERROR: turn failed",
		"codex\nUsage limit reached · is what the banner says now.\nERROR: turn failed",
		"exec\nbash -lc 'go run ./cmd/sync' in /repo\n exited 1 in 0.4s:\nERROR: rate limit exceeded\nERROR: turn failed",
		"exec\nbash -lc './deploy.sh' in /repo\n exited 1 in 0.4s:\nERROR: Quota exceeded.\nERROR: turn failed",
		"exec\nbash -lc 'python call_openai.py' in /repo\n exited 1 in 2.0s:\nERROR: insufficient_quota\nERROR: turn failed",
		"codex\nThe function now returns early with\nQuota exceeded.\nwhen the bucket is empty.\nERROR: turn failed",
		"codex\ninsufficient_quota is the error type OpenAI returns when credits run out.\nERROR: turn failed",
		"codex\nYou have reached your quota of 5 free builds on the vendor plan.\nERROR: turn failed",
		"exec\nbash -lc 'curl -sf https://api.example.com' in /repo\n exited 22 in 0.31s:\nError: 429 Too Many Requests\nRate limit exceeded",
		"exec\nbash -lc 'vendor-cli sync' in /repo\n exited 1 in 0.3s:\nYou've hit your usage limit for the Acme API.",
	} {
		if looksLikeQuotaExhaustion("codex", text) {
			t.Errorf("codex transcript content read as codex's refusal: %q", text)
		}
		if err := formatCLIExecutionError("codex", "", text, exit, nil, time.Second); ErrorKindOf(err) == ErrorKindRateLimit {
			t.Errorf("codex transcript content sealed the pool: %v", err)
		}
	}
	own := "[2026-10-12T08:00:00] codex\nWorking on it.\n[2026-10-12T08:00:05] ERROR: You’ve hit your usage limit. Try again at 8:00 PM."
	if err := formatCLIExecutionError("codex", "", own, exit, nil, time.Second); ErrorKindOf(err) != ErrorKindRateLimit {
		t.Errorf("codex's own usage-limit event did not seal: %v", err)
	}
}

// A CLI failure is classified as a quota/rate-limit refusal (sealing the pool)
// only on the CLI's own message, never on stderr noise.
func TestFormatCLIExecutionErrorSealsOnlyOnCLIRefusals_QuotaHints(t *testing.T) {
	exit := errors.New("exit status 1")
	for _, tc := range cliQuotaRefusals {
		err := formatCLIExecutionError(tc.cli, "", tc.text, exit, nil, time.Second)
		var perr *ProviderError
		if !errors.As(err, &perr) || perr.Kind != ErrorKindRateLimit || perr.StatusCode != 429 || !perr.Retryable || perr.Provider != tc.cli {
			t.Errorf("%s refusal %q classified as %v", tc.cli, tc.text, err)
		}
	}
	for _, text := range cliQuotaNoise {
		for _, cli := range cliQuotaProviders {
			if err := formatCLIExecutionError(cli, "", text, exit, nil, time.Second); ErrorKindOf(err) == ErrorKindRateLimit {
				t.Errorf("%s stderr noise %q sealed the pool: %v", cli, text, err)
			}
		}
	}
	// A CLI the catalog does not know (a custom command) is read with the
	// generic patterns, in the report it ends with only
	// (TestCustomCLIRefusalSealsWithGenericPatterns_R4).
	if err := formatCLIExecutionError("aider", "", "You've hit your limit · resets 8pm", exit, nil, time.Second); ErrorKindOf(err) != ErrorKindRateLimit {
		t.Errorf("an unknown CLI's refusal did not seal its pool: %v", err)
	}
	if err := formatCLIExecutionError("aider", "", "You've hit your limit · resets 8pm\nretrying\nError: build failed", exit, nil, time.Second); ErrorKindOf(err) == ErrorKindRateLimit {
		t.Errorf("an unknown CLI sealed on a line before its error report: %v", err)
	}
	// stderr is read as the CLI wrote it: an indented line is content.
	if err := formatCLIExecutionError("claude", "", "    Rate limit exceeded", exit, nil, time.Second); ErrorKindOf(err) == ErrorKindRateLimit {
		t.Errorf("an indented line sealed the pool: %v", err)
	}
}
