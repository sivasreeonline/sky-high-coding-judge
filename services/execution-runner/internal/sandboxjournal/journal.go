package sandboxjournal

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	formatVersion = 1
	maxJournalSize = 64 << 20
	maxRecordSize = 1 << 20
)

var (
	ErrClosed = errors.New("sandbox lifecycle journal is closed")
	ErrConflict = errors.New("sandbox lifecycle transition conflict")
	ErrNotFound = errors.New("sandbox lifecycle record not found")
	ErrLocked = errors.New("sandbox lifecycle journal already has an owner")
	runnerIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)
	executionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	containerIDPattern = regexp.MustCompile(`^[a-f0-9]{12,64}$`)
)

type EventType string

const (
	EventCreateIntent EventType = "CREATE_INTENT"
	EventCreated EventType = "CREATED"
	EventCleaned EventType = "CLEANED"
)

// Event is append-only. It intentionally contains identifiers and lifecycle
// metadata only; source code, stdin, outputs and test fixtures never belong here.
type Event struct {
	Version int `json:"version"`
	Sequence uint64 `json:"sequence"`
	Type EventType `json:"type"`
	LifecycleID string `json:"lifecycleId"`
	RunnerInstanceID string `json:"runnerInstanceId"`
	ExecutionID string `json:"executionId"`
	ContainerID string `json:"containerId,omitempty"`
	At time.Time `json:"at"`
}

type Lifecycle struct {
	LifecycleID string
	RunnerInstanceID string
	ExecutionID string
	ContainerID string
	Created bool
	Cleaned bool
	LastSequence uint64
}

type Journal struct {
	mu sync.Mutex
	file *os.File
	path string
	lockPath string
	lockToken string
	sequence uint64
	records map[string]Lifecycle
	closed bool
}

func Open(path string) (*Journal, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("sandbox lifecycle journal path is required")
	}
	directory, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("resolve sandbox journal directory: %w", err)
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create sandbox journal directory: %w", err)
	}
	resolvedDirectory, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return nil, fmt.Errorf("resolve sandbox journal directory symlinks: %w", err)
	}
	directory = resolvedDirectory
	directoryInfo, err := os.Stat(directory)
	if err != nil {
		return nil, fmt.Errorf("inspect sandbox journal directory: %w", err)
	}
	if !directoryInfo.IsDir() || directoryInfo.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("sandbox lifecycle journal directory must be private to its owner")
	}
	canonical := filepath.Join(directory, filepath.Base(path))
	lockPath := canonical + ".lock"
	token, err := acquireLock(lockPath)
	if err != nil {
		return nil, err
	}
	releaseOnError := true
	defer func() {
		if releaseOnError {
			_ = releaseLock(lockPath, token)
		}
	}()

	initialInfo, statBeforeOpenErr := os.Lstat(canonical)
	if statBeforeOpenErr == nil {
		if initialInfo.Mode()&os.ModeSymlink != 0 || !initialInfo.Mode().IsRegular() {
			return nil, errors.New("sandbox lifecycle journal must be a regular non-symlink file")
		}
		if initialInfo.Mode().Perm()&0o077 != 0 {
			return nil, errors.New("sandbox lifecycle journal permissions must exclude group and other access")
		}
	} else if !errors.Is(statBeforeOpenErr, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect sandbox lifecycle journal: %w", statBeforeOpenErr)
	}
	file, err := os.OpenFile(canonical, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open sandbox lifecycle journal: %w", err)
	}
	openedInfo, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		return nil, fmt.Errorf("stat opened sandbox lifecycle journal: %w", statErr)
	}
	currentInfo, pathStatErr := os.Lstat(canonical)
	if pathStatErr != nil || currentInfo.Mode()&os.ModeSymlink != 0 ||
		!currentInfo.Mode().IsRegular() || !os.SameFile(openedInfo, currentInfo) ||
		(statBeforeOpenErr == nil && !os.SameFile(initialInfo, openedInfo)) {
		_ = file.Close()
		return nil, errors.New("sandbox lifecycle journal path changed during open")
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("secure sandbox lifecycle journal: %w", err)
	}
	if errors.Is(statBeforeOpenErr, os.ErrNotExist) {
		if err := syncDirectory(directory); err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("persist sandbox lifecycle journal directory entry: %w", err)
		}
	}
	journal := &Journal{
		file: file, path: canonical, lockPath: lockPath, lockToken: token,
		records: make(map[string]Lifecycle),
	}
	if err := journal.recover(); err != nil {
		_ = file.Close()
		return nil, err
	}
	releaseOnError = false
	return journal, nil
}

func acquireLock(path string) (string, error) {
	tokenBytes := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, tokenBytes); err != nil {
		return "", fmt.Errorf("generate sandbox journal lock token: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("%w: %s", ErrLocked, path)
		}
		return "", fmt.Errorf("create sandbox journal lock: %w", err)
	}
	payload, err := json.Marshal(struct {
		PID int `json:"pid"`
		Token string `json:"token"`
		CreatedAt time.Time `json:"createdAt"`
	}{os.Getpid(), token, time.Now().UTC()})
	if err == nil {
		var written int
		written, err = file.Write(payload)
		if err == nil && written != len(payload) {
			err = io.ErrShortWrite
		}
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return "", errors.Join(err, closeErr)
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return "", fmt.Errorf("persist sandbox journal lock directory entry: %w", err)
	}
	return token, nil
}

func releaseLock(path, token string) error {
	payload, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var owner struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(payload, &owner); err != nil {
		return err
	}
	if owner.Token != token || token == "" {
		return errors.New("sandbox lifecycle journal lock ownership changed")
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func (journal *Journal) recover() error {
	info, err := journal.file.Stat()
	if err != nil {
		return fmt.Errorf("stat sandbox lifecycle journal: %w", err)
	}
	if info.Size() > maxJournalSize {
		return errors.New("sandbox lifecycle journal exceeds size limit")
	}
	content, err := io.ReadAll(journal.file)
	if err != nil {
		return fmt.Errorf("read sandbox lifecycle journal: %w", err)
	}
	lastNewline := bytes.LastIndexByte(content, '\n')
	if lastNewline != len(content)-1 {
		validLength := lastNewline + 1
		if err := journal.file.Truncate(int64(validLength)); err != nil {
			return fmt.Errorf("truncate incomplete sandbox lifecycle record: %w", err)
		}
		content = content[:validLength]
		if err := journal.file.Sync(); err != nil {
			return fmt.Errorf("sync recovered sandbox lifecycle journal: %w", err)
		}
	}
	var expected uint64 = 1
	for len(content) > 0 {
		lineEnd := bytes.IndexByte(content, '\n')
		if lineEnd < 0 {
			return errors.New("sandbox lifecycle journal recovery found unterminated record")
		}
		line := content[:lineEnd]
		content = content[lineEnd+1:]
		if len(line) == 0 || len(line) > maxRecordSize {
			return errors.New("sandbox lifecycle journal contains an invalid record size")
		}
		var event Event
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&event); err != nil {
			return fmt.Errorf("decode sandbox lifecycle record: %w", err)
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return errors.New("sandbox lifecycle record contains trailing JSON data")
		}
		if event.Version != formatVersion || event.Sequence != expected || event.At.IsZero() {
			return errors.New("sandbox lifecycle journal version, sequence or timestamp is invalid")
		}
		if err := journal.apply(event); err != nil {
			return fmt.Errorf("recover sandbox lifecycle transition: %w", err)
		}
		journal.sequence = event.Sequence
		expected++
	}
	_, err = journal.file.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("seek sandbox lifecycle journal: %w", err)
	}
	return nil
}

func (journal *Journal) Begin(runnerInstanceID, executionID string) (string, error) {
	if !runnerIDPattern.MatchString(runnerInstanceID) || !executionIDPattern.MatchString(executionID) {
		return "", errors.New("invalid runner instance or execution identifier")
	}
	idBytes := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, idBytes); err != nil {
		return "", fmt.Errorf("generate sandbox lifecycle ID: %w", err)
	}
	id := hex.EncodeToString(idBytes)
	event := Event{
		Type: EventCreateIntent, LifecycleID: id,
		RunnerInstanceID: runnerInstanceID, ExecutionID: executionID,
	}
	if err := journal.append(event); err != nil {
		return "", err
	}
	return id, nil
}

func (journal *Journal) Created(lifecycleID, containerID string) error {
	if !runnerIDPattern.MatchString(lifecycleID) || !containerIDPattern.MatchString(containerID) {
		return errors.New("invalid lifecycle or container identifier")
	}
	journal.mu.Lock()
	defer journal.mu.Unlock()
	current, ok := journal.records[lifecycleID]
	if !ok || current.Created || current.Cleaned {
		return ErrConflict
	}
	return journal.appendLocked(Event{
		Type: EventCreated, LifecycleID: lifecycleID,
		RunnerInstanceID: current.RunnerInstanceID, ExecutionID: current.ExecutionID,
		ContainerID: containerID,
	})
}

func (journal *Journal) Cleaned(lifecycleID string) error {
	if !runnerIDPattern.MatchString(lifecycleID) {
		return errors.New("invalid lifecycle identifier")
	}
	journal.mu.Lock()
	defer journal.mu.Unlock()
	current, ok := journal.records[lifecycleID]
	if !ok {
		return ErrNotFound
	}
	if !current.Created || current.Cleaned {
		return ErrConflict
	}
	return journal.appendLocked(Event{
		Type: EventCleaned, LifecycleID: lifecycleID,
		RunnerInstanceID: current.RunnerInstanceID, ExecutionID: current.ExecutionID,
		ContainerID: current.ContainerID,
	})
}

func (journal *Journal) Pending() ([]Lifecycle, error) {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	if journal.closed {
		return nil, ErrClosed
	}
	pending := make([]Lifecycle, 0)
	for _, record := range journal.records {
		if !record.Cleaned {
			pending = append(pending, record)
		}
	}
	return pending, nil
}

func (journal *Journal) append(event Event) error {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	return journal.appendLocked(event)
}

func (journal *Journal) appendLocked(event Event) error {
	if journal.closed {
		return ErrClosed
	}
	if journal.sequence >= maxJournalSize {
		return errors.New("sandbox lifecycle journal sequence limit reached")
	}
	event.Version = formatVersion
	event.Sequence = journal.sequence + 1
	event.At = time.Now().UTC()
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode sandbox lifecycle event: %w", err)
	}
	if len(payload)+1 > maxRecordSize {
		return errors.New("sandbox lifecycle event exceeds record size limit")
	}
	info, err := journal.file.Stat()
	if err != nil {
		return fmt.Errorf("stat sandbox lifecycle journal before append: %w", err)
	}
	if info.Size()+int64(len(payload)+1) > maxJournalSize {
		return errors.New("sandbox lifecycle journal reached its size limit")
	}
	payload = append(payload, '\n')
	written, writeErr := journal.file.Write(payload)
	if writeErr == nil && written != len(payload) {
		writeErr = io.ErrShortWrite
	}
	if writeErr != nil {
		return fmt.Errorf("append sandbox lifecycle event: %w", writeErr)
	}
	if err := journal.file.Sync(); err != nil {
		return fmt.Errorf("sync sandbox lifecycle event: %w", err)
	}
	if err := journal.apply(event); err != nil {
		return fmt.Errorf("apply sandbox lifecycle event: %w", err)
	}
	journal.sequence = event.Sequence
	return nil
}

func (journal *Journal) apply(event Event) error {
	if !runnerIDPattern.MatchString(event.LifecycleID) ||
		!runnerIDPattern.MatchString(event.RunnerInstanceID) ||
		!executionIDPattern.MatchString(event.ExecutionID) {
		return errors.New("sandbox lifecycle event has invalid identifiers")
	}
	current, exists := journal.records[event.LifecycleID]
	switch event.Type {
	case EventCreateIntent:
		if exists || event.ContainerID != "" {
			return ErrConflict
		}
		journal.records[event.LifecycleID] = Lifecycle{
			LifecycleID: event.LifecycleID, RunnerInstanceID: event.RunnerInstanceID,
			ExecutionID: event.ExecutionID, LastSequence: event.Sequence,
		}
	case EventCreated:
		if !exists || current.Created || current.Cleaned ||
			current.RunnerInstanceID != event.RunnerInstanceID ||
			current.ExecutionID != event.ExecutionID ||
			!containerIDPattern.MatchString(event.ContainerID) {
			return ErrConflict
		}
		current.ContainerID = event.ContainerID
		current.Created = true
		current.LastSequence = event.Sequence
		journal.records[event.LifecycleID] = current
	case EventCleaned:
		if !exists || !current.Created || current.Cleaned ||
			current.RunnerInstanceID != event.RunnerInstanceID ||
			current.ExecutionID != event.ExecutionID ||
			current.ContainerID != event.ContainerID {
			return ErrConflict
		}
		current.Cleaned = true
		current.LastSequence = event.Sequence
		journal.records[event.LifecycleID] = current
	default:
		return errors.New("unknown sandbox lifecycle event type")
	}
	return nil
}

func (journal *Journal) Close() error {
	if journal == nil {
		return nil
	}
	journal.mu.Lock()
	defer journal.mu.Unlock()
	if journal.closed {
		return nil
	}
	if err := journal.file.Sync(); err != nil {
		return fmt.Errorf("sync sandbox lifecycle journal on close: %w", err)
	}
	if err := journal.file.Close(); err != nil {
		return fmt.Errorf("close sandbox lifecycle journal: %w", err)
	}
	if err := releaseLock(journal.lockPath, journal.lockToken); err != nil {
		return fmt.Errorf("release sandbox lifecycle journal lock: %w", err)
	}
	journal.closed = true
	return nil
}
