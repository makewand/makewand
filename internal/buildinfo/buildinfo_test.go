package buildinfo

import "testing"

func TestVersionString_DevDefault(t *testing.T) {
	// Reset to defaults
	oldVersion := Version
	oldCommit := Commit
	oldDirty := Dirty
	defer func() {
		Version = oldVersion
		Commit = oldCommit
		Dirty = oldDirty
	}()

	Version = "dev"
	Commit = ""
	Dirty = ""

	if got := VersionString(); got != "dev" {
		t.Errorf("VersionString() = %q, want dev", got)
	}
}

func TestVersionString_Released(t *testing.T) {
	oldVersion := Version
	defer func() { Version = oldVersion }()

	Version = "v0.2.0"

	if got := VersionString(); got != "v0.2.0" {
		t.Errorf("VersionString() = %q, want v0.2.0", got)
	}
}

func TestVersionString_DevWithCommit(t *testing.T) {
	oldVersion := Version
	oldCommit := Commit
	oldDirty := Dirty
	defer func() {
		Version = oldVersion
		Commit = oldCommit
		Dirty = oldDirty
	}()

	Version = "dev"
	Commit = "abc123"
	Dirty = ""

	if got := VersionString(); got != "dev-abc123" {
		t.Errorf("VersionString() = %q, want dev-abc123", got)
	}
}

func TestVersionString_DevWithCommitAndDirty(t *testing.T) {
	oldVersion := Version
	oldCommit := Commit
	oldDirty := Dirty
	defer func() {
		Version = oldVersion
		Commit = oldCommit
		Dirty = oldDirty
	}()

	Version = "dev"
	Commit = "abc123"
	Dirty = "dirty"

	if got := VersionString(); got != "dev-abc123-dirty" {
		t.Errorf("VersionString() = %q, want dev-abc123-dirty", got)
	}
}
