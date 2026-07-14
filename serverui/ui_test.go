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
