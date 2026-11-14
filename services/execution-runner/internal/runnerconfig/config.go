package runnerconfig

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sivasreeonline/orbit-engine/services/execution-runner/internal/runnerauth"
)

var (
	keyIDPattern          = regexp.MustCompile("^[A-Za-z0-9._:-]{1,128}$")
	runnerInstanceIDPattern = regexp.MustCompile("^[a-f0-9]{32}$")
)

const (
	EnvControlPlaneURL  = "RUNNER_CONTROL_PLANE_URL"
	EnvKeyID            = "RUNNER_KEY_ID"
	EnvPrivateKeyFile   = "RUNNER_PRIVATE_KEY_FILE"
	EnvJournalPath      = "RUNNER_JOURNAL_PATH"
	EnvRunnerInstanceID = "RUNNER_INSTANCE_ID"
)

type Config struct {
	ControlPlaneURL  string
	KeyID            string
	PrivateKey       ed25519.PrivateKey
	JournalPath      string
	RunnerInstanceID string
}

// Load reads the runner's explicit identity configuration. The lookup
// function is injectable so validation can be tested without mutating process
// environment. Secret contents are never included in returned errors.
func Load(lookup func(string) (string, bool)) (*Config, error) {
	if lookup == nil {
		return nil, errors.New("runner environment lookup is required")
	}

	controlPlaneURL, err := requiredValue(lookup, EnvControlPlaneURL)
	if err != nil {
		return nil, err
	}
	if err := validateControlPlaneOrigin(controlPlaneURL); err != nil {
		return nil, fmt.Errorf("%s: %w", EnvControlPlaneURL, err)
	}

	keyID, err := requiredValue(lookup, EnvKeyID)
	if err != nil {
		return nil, err
	}
	if !keyIDPattern.MatchString(keyID) {
		return nil, fmt.Errorf("%s is invalid", EnvKeyID)
	}

	keyFile, err := requiredValue(lookup, EnvPrivateKeyFile)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(keyFile) {
		return nil, fmt.Errorf("%s must be an absolute path", EnvPrivateKeyFile)
	}
	privateKey, err := runnerauth.LoadPrivateKeyFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("%s could not be loaded securely: %w", EnvPrivateKeyFile, err)
	}

	runnerInstanceID, err := requiredValue(lookup, EnvRunnerInstanceID)
	if err != nil {
		zero(privateKey)
		return nil, err
	}
	if !runnerInstanceIDPattern.MatchString(runnerInstanceID) {
		zero(privateKey)
		return nil, fmt.Errorf("%s must be 32 lowercase hexadecimal characters", EnvRunnerInstanceID)
	}

	journalPath, err := requiredValue(lookup, EnvJournalPath)
	if err != nil {
		zero(privateKey)
		return nil, err
	}
	if !filepath.IsAbs(journalPath) || filepath.Clean(journalPath) == string(filepath.Separator) {
		zero(privateKey)
		return nil, fmt.Errorf("%s must be an absolute non-root path", EnvJournalPath)
	}

	return &Config{
		ControlPlaneURL:  controlPlaneURL,
		KeyID:            keyID,
		PrivateKey:       privateKey,
		JournalPath:      journalPath,
		RunnerInstanceID: runnerInstanceID,
	}, nil
}

// Destroy clears the in-memory private-key bytes held by this configuration.
func (config *Config) Destroy() {
	if config == nil {
		return
	}
	zero(config.PrivateKey)
	config.PrivateKey = nil
}

func requiredValue(lookup func(string) (string, bool), name string) (string, error) {
	value, ok := lookup(name)
	value = strings.TrimSpace(value)
	if !ok || value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}

func validateControlPlaneOrigin(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed == nil || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Path != "" && parsed.Path != "/") {
		return errors.New("must be an origin URL without credentials, path, query or fragment")
	}
	if parsed.Scheme == "https" {
		return nil
	}
	if parsed.Scheme == "http" && isLoopback(parsed.Hostname()) {
		return nil
	}
	return errors.New("must use HTTPS; HTTP is permitted only for loopback development")
}

func isLoopback(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func zero(key ed25519.PrivateKey) {
	for index := range key {
		key[index] = 0
	}
}
