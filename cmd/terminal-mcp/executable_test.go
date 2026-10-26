package main

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

func TestInstallIfMissingDoesNotSilentlyUpdateOrDowngrade(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "first")
	other := filepath.Join(dir, "other")
	installed := filepath.Join(dir, "installed")
	if err := os.WriteFile(source, []byte("first binary"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("different binary"), 0700); err != nil {
		t.Fatal(err)
	}
	newInstall, err := installIfMissing(source, installed, 0700)
	if err != nil || !newInstall {
		t.Fatalf("initial installation failed: newly installed=%t err=%v", newInstall, err)
	}
	want, err := fileSHA256(source)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := fileSHA256(installed)
	if err != nil || actual != want {
		t.Fatalf("first install changed executable: actual=%q want=%q err=%v", actual, want, err)
	}
	if again, err := installIfMissing(source, installed, 0700); err != nil || again {
		t.Fatalf("identical source attempted reinstall: %t %v", again, err)
	}
	equalButSeparate := filepath.Join(dir, "same-bytes")
	if err := os.WriteFile(equalButSeparate, []byte("first binary"), 0700); err != nil {
		t.Fatal(err)
	}
	if again, err := installIfMissing(equalButSeparate, installed, 0700); err != nil || again {
		t.Fatalf("equivalent separately downloaded binary was rejected: %t %v", again, err)
	}
	if _, err := installIfMissing(other, installed, 0700); err == nil ||
		!strings.Contains(err.Error(), "installed_version_mismatch") ||
		!strings.Contains(err.Error(), "update --candidate") {
		t.Fatalf("mismatch did not explain explicit upgrade: %v", err)
	}
	if actual, err := fileSHA256(installed); err != nil || actual != want {
		t.Fatalf("mismatched start modified existing installation: %q %v", actual, err)
	}
}

func TestInstallIfMissingRejectsSymlinkedInstallation(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	installed := filepath.Join(dir, "installed")
	if err := os.WriteFile(source, []byte("binary"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(source, installed); err != nil {
		t.Skipf("no symlink support: %v", err)
	}
	if _, err := installIfMissing(source, installed, 0700); err == nil {
		t.Fatal("unexpectedly followed a symlinked installed executable")
	}
}

func TestRevisionMetadataIncludesDirtyAndNoFalseRevision(t *testing.T) {
	settings := []debug.BuildSetting{{Key: "vcs.revision", Value: strings.Repeat("a", 40)}, {Key: "vcs.modified", Value: "true"}}
	if got := revisionFromSettings(settings); got != "aaaaaaaaaaaa-dirty" {
		t.Fatalf("incorrect revision with modified flag: %q", got)
	}
	if got := revisionFromSettings(nil); got != "" {
		t.Fatalf("unversioned binary reported a revision: %q", got)
	}
}
