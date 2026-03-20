package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/makewand/makewand/internal/config"
	"github.com/makewand/makewand/internal/i18n"
)

func TestViewPendingApproval_LocalizedZh(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Language = "zh"
	app := *NewApp(ModeChat, cfg, "")
	app.setPendingApproval(
		approvalFileWrite,
		fmt.Sprintf(i18n.Msg().FileConfirmWrite, 1),
		pendingWriteDetails(1),
	)

	view := app.viewPendingApproval(40)
	if !strings.Contains(view, "待确认操作") {
		t.Fatalf("view = %q, want localized approval title", view)
	}
	if !strings.Contains(view, "待写入 1 个文件") {
		t.Fatalf("view = %q, want localized pending write detail", view)
	}
	if !strings.Contains(view, "使用 /approve 或 /deny") {
		t.Fatalf("view = %q, want localized action hint", view)
	}
	if strings.Contains(view, "Pending write") || strings.Contains(view, "Pending Approval") {
		t.Fatalf("view = %q, should not contain english approval strings", view)
	}
}
