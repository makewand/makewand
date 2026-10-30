//go:build !windows

package router

import (
	"os"
	"path/filepath"
	"testing"
)

func TestQuotaAuthLeafAliasIsNotAnRPCGeneration(t *testing.T) {
	q := rpcQuotaIdentity(t)
	alias := filepath.Join(t.TempDir(), "auth.json")
	if err := os.Symlink(filepath.Join(q.CredentialRoot, "auth.json"), alias); err != nil {
		t.Fatal(err)
	}
	if _, err := readQuotaAuthVersion(alias); err == nil {
		t.Fatal("a switchable credential alias was accepted as a pinned file generation")
	}
}
