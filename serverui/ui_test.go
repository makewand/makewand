package serverui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandler_ServesAdminShellAndAssets(t *testing.T) {
	handler := Handler()

	indexReq := httptest.NewRequest(http.MethodGet, "/admin", nil)
	indexRec := httptest.NewRecorder()
	handler.ServeHTTP(indexRec, indexReq)
	if indexRec.Code != http.StatusOK {
		t.Fatalf("index status = %d, want 200; body: %s", indexRec.Code, indexRec.Body.String())
	}
	if !strings.Contains(indexRec.Body.String(), "makewand Admin") {
		t.Fatalf("index body = %q, want admin shell", indexRec.Body.String())
	}
	if csp := indexRec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("Content-Security-Policy = %q, want frame-ancestors protection", csp)
	}

	assetReq := httptest.NewRequest(http.MethodGet, "/admin/style.css", nil)
	assetRec := httptest.NewRecorder()
	handler.ServeHTTP(assetRec, assetReq)
	if assetRec.Code != http.StatusOK {
		t.Fatalf("asset status = %d, want 200; body: %s", assetRec.Code, assetRec.Body.String())
	}
	if !strings.Contains(assetRec.Body.String(), ".shell") {
		t.Fatalf("asset body = %q, want stylesheet", assetRec.Body.String())
	}
}

func TestHandler_FallsBackToIndexForAdminRoutes(t *testing.T) {
	handler := Handler()

	req := httptest.NewRequest(http.MethodGet, "/admin/projects/dashboard", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Admin Console") {
		t.Fatalf("body = %q, want SPA fallback", rec.Body.String())
	}
}

func TestHandler_ServesPlaygroundAndChatAssets(t *testing.T) {
	handler := Handler()

	indexReq := httptest.NewRequest(http.MethodGet, "/admin", nil)
	indexRec := httptest.NewRecorder()
	handler.ServeHTTP(indexRec, indexReq)
	if indexRec.Code != http.StatusOK {
		t.Fatalf("index status = %d, want 200", indexRec.Code)
	}
	body := indexRec.Body.String()
	if !strings.Contains(body, `data-view="playground"`) {
		t.Fatalf("index body missing playground nav link")
	}
	if !strings.Contains(body, `id="chat-history-container"`) {
		t.Fatalf("index body missing chat history container")
	}
	if !strings.Contains(body, `id="playground-input"`) {
		t.Fatalf("index body missing playground input")
	}

	cssReq := httptest.NewRequest(http.MethodGet, "/admin/style.css", nil)
	cssRec := httptest.NewRecorder()
	handler.ServeHTTP(cssRec, cssReq)
	if cssRec.Code != http.StatusOK {
		t.Fatalf("css status = %d, want 200", cssRec.Code)
	}
	cssBody := cssRec.Body.String()
	if !strings.Contains(cssBody, ".chat-history-wrap") || !strings.Contains(cssBody, "overflow-y: auto") {
		t.Fatalf("style.css missing chat-history-wrap scrollable overflow styles")
	}

	jsReq := httptest.NewRequest(http.MethodGet, "/admin/app.js", nil)
	jsRec := httptest.NewRecorder()
	handler.ServeHTTP(jsRec, jsReq)
	if jsRec.Code != http.StatusOK {
		t.Fatalf("js status = %d, want 200", jsRec.Code)
	}
	jsBody := jsRec.Body.String()
	if !strings.Contains(jsBody, "classifyWebIntent") || !strings.Contains(jsBody, "IDENTITY_EXPLANATION") {
		t.Fatalf("app.js missing intent classification logic")
	}
}

