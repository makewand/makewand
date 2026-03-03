package engine

import (
	"context"
	"strings"
	"testing"
)

func TestStartPreview_RequiresAllowProjectScriptsForNode(t *testing.T) {
	proj, err := NewProject("preview-node", t.TempDir())
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	if err := proj.WriteFile("package.json", `{"name":"x","scripts":{"dev":"vite"}}`); err != nil {
		t.Fatalf("WriteFile(package.json): %v", err)
	}

	_, err = proj.StartPreview(context.Background(), false)
	if err == nil {
		t.Fatal("StartPreview should reject project scripts without explicit allow flag")
	}
	if !strings.Contains(err.Error(), "--allow-project-scripts") {
		t.Fatalf("StartPreview error = %q, want --allow-project-scripts hint", err.Error())
	}
}

func TestStartPreview_RequiresAllowProjectScriptsForPython(t *testing.T) {
	proj, err := NewProject("preview-python", t.TempDir())
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	if err := proj.WriteFile("manage.py", "print('ok')\n"); err != nil {
		t.Fatalf("WriteFile(manage.py): %v", err)
	}

	_, err = proj.StartPreview(context.Background(), false)
	if err == nil {
		t.Fatal("StartPreview should reject manage.py execution without explicit allow flag")
	}
	if !strings.Contains(err.Error(), "--allow-project-scripts") {
		t.Fatalf("StartPreview error = %q, want --allow-project-scripts hint", err.Error())
	}
}
