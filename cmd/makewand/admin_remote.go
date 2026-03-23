package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/makewand/makewand/serveraudit"
	"github.com/makewand/makewand/serverauth"
)

type remoteTokenListResponse struct {
	Data []serverauth.TokenRuleView `json:"data"`
}

type remoteTokenIssueResponse struct {
	TokenID string                   `json:"token_id"`
	Token   string                   `json:"token"`
	Rule    serverauth.TokenRuleView `json:"rule"`
}

type remoteAuditSummaryResponse struct {
	Path    string              `json:"path"`
	Summary serveraudit.Summary `json:"summary"`
}

type remoteAuditEventsResponse struct {
	Path string              `json:"path"`
	Data []serveraudit.Event `json:"data"`
}

func resolveOptionalRemoteAdminTarget(flagURL, flagToken string) (string, string, bool, error) {
	urlValue := strings.TrimSpace(flagURL)
	tokenValue := strings.TrimSpace(flagToken)
	if urlValue == "" && tokenValue == "" {
		return "", "", false, nil
	}
	if urlValue == "" {
		urlValue = strings.TrimSpace(os.Getenv("MAKEWAND_REMOTE_URL"))
	}
	if tokenValue == "" {
		tokenValue = strings.TrimSpace(os.Getenv("MAKEWAND_REMOTE_TOKEN"))
	}
	if urlValue == "" || tokenValue == "" {
		return "", "", false, fmt.Errorf("remote admin commands require both remote URL and remote token")
	}
	return strings.TrimRight(urlValue, "/"), tokenValue, true, nil
}

func newAdminHTTPClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Second}
}

func adminGetJSON(baseURL, token, path string, query url.Values, dest any) error {
	endpoint := baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return doAdminJSON(req, dest)
}

func adminPostJSON(baseURL, token, path string, body any, dest any) error {
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(data)
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+path, payload)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return doAdminJSON(req, dest)
}

func doAdminJSON(req *http.Request, dest any) error {
	resp, err := newAdminHTTPClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		var failure struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &failure) == nil && strings.TrimSpace(failure.Error.Message) != "" {
			return fmt.Errorf("remote admin request failed (%d): %s", resp.StatusCode, failure.Error.Message)
		}
		return fmt.Errorf("remote admin request failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if dest == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	return json.Unmarshal(data, dest)
}
