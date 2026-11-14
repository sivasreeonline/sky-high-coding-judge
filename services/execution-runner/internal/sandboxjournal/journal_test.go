package sandboxjournal

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testRunnerID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const testExecutionID = "exec_test_01"
const testContainerID = "0123456789abcdef"

func privateTempDir(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	return directory
}

func TestJournalPersistsLifecycleTransitionsAndRecoversPending(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "sandbox.jsonl")
	journal, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	lifecycleID, err := journal.Begin(testRunnerID, testExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := journal.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].LifecycleID != lifecycleID ||
		pending[0].Created || pending[0].Cleaned || pending[0].ContainerID != "" {
		t.Fatalf("intent pending state = %+v", pending)
	}
	if err := journal.Created(lifecycleID, testContainerID); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	pending, err = reopened.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || !pending[0].Created || pending[0].Cleaned ||
		pending[0].ContainerID != testContainerID {
		t.Fatalf("recovered created state = %+v", pending)
	}
	if err := reopened.Cleaned(lifecycleID); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	final, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer final.Close()
	pending, err = final.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("cleaned lifecycle still pending: %+v", pending)
	}
}

func TestJournalRejectsConcurrentOwnerAndDoesNotStealLock(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "sandbox.jsonl")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("second owner error = %v, want ErrLocked", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Open(path)
	if err != nil {
		t.Fatalf("open after clean release: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestJournalTruncatesOnlyIncompleteTrailingRecord(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "sandbox.jsonl")
	journal, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Begin(testRunnerID, testExecutionID); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("{incomplete"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	recovered, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	pending, err := recovered.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("recovery lost valid intent: %+v", pending)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "incomplete") {
		t.Fatal("incomplete trailing bytes were not truncated")
	}
}

func TestJournalRejectsTrailingJSONOnCompleteRecord(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "sandbox.jsonl")
	journal, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Begin(testRunnerID, testExecutionID); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content = []byte(strings.TrimSuffix(string(content), "\n") + " {}\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("trailing JSON data on a complete record was accepted")
	}
}

func TestJournalRejectsMalformedCompleteRecord(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "sandbox.jsonl")
	if err := os.WriteFile(path, []byte("{not-json}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("malformed complete record was accepted")
	}
	if _, err := os.Stat(path + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed open left an owned lock behind: %v", err)
	}
}

func TestJournalRejectsInvalidTransitionsAndIdentifiers(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "sandbox.jsonl")
	journal, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()

	if _, err := journal.Begin("not-a-runner-id", testExecutionID); err == nil {
		t.Fatal("invalid runner instance ID accepted")
	}
	if _, err := journal.Begin(testRunnerID, "bad execution id"); err == nil {
		t.Fatal("invalid execution ID accepted")
	}
	if err := journal.Created(strings.Repeat("a", 32), testContainerID); !errors.Is(err, ErrConflict) {
		t.Fatalf("unknown lifecycle transition error = %v, want conflict", err)
	}
	lifecycleID, err := journal.Begin(testRunnerID, testExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Cleaned(lifecycleID); !errors.Is(err, ErrConflict) {
		t.Fatalf("clean-before-created error = %v, want conflict", err)
	}
	if err := journal.Created(lifecycleID, "not-a-container-id"); err == nil {
		t.Fatal("invalid container ID accepted")
	}
	if err := journal.Created(lifecycleID, testContainerID); err != nil {
		t.Fatal(err)
	}
	if err := journal.Created(lifecycleID, testContainerID); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate created event error = %v, want conflict", err)
	}
	if err := journal.Cleaned(lifecycleID); err != nil {
		t.Fatal(err)
	}
	if err := journal.Cleaned(lifecycleID); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate cleaned event error = %v, want conflict", err)
	}
}

func TestJournalRejectsSymlinkAndPermissiveExistingFile(t *testing.T) {
	directory := privateTempDir(t)
	target := filepath.Join(directory, "target.jsonl")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(target); err == nil {
		t.Fatal("permissive existing journal was accepted")
	}

	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "linked.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}
	if _, err := Open(link); err == nil {
		t.Fatal("symlinked journal was accepted")
	}
}
