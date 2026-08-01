package logging

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type RollingFile struct {
	mu        sync.Mutex
	path      string
	lastPurge time.Time
}

func NewRollingFile(path string) (*RollingFile, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, err
	}
	rolling := &RollingFile{path: path}
	return rolling, rolling.Purge(time.Now().UTC())
}

func (f *RollingFile) Write(data []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now().UTC()
	if now.Sub(f.lastPurge) >= time.Minute {
		if err := f.purgeLocked(now); err != nil {
			return 0, err
		}
	}
	file, err := os.OpenFile(f.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	n, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil {
		return n, writeErr
	}
	return n, closeErr
}

func (f *RollingFile) Purge(now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.purgeLocked(now.UTC())
}

func (f *RollingFile) purgeLocked(now time.Time) error {
	f.lastPurge = now
	input, err := os.Open(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer input.Close()

	tempPath := f.path + ".tmp"
	output, err := os.OpenFile(tempPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	success := false
	defer func() {
		_ = output.Close()
		if !success {
			_ = os.Remove(tempPath)
		}
	}()
	writer := bufio.NewWriter(output)
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	cutoff := now.Add(-Retention)
	for scanner.Scan() {
		line := scanner.Bytes()
		var envelope struct {
			Time time.Time `json:"time"`
		}
		if json.Unmarshal(line, &envelope) != nil || envelope.Time.Before(cutoff) {
			continue
		}
		if _, err := writer.Write(line); err != nil {
			return err
		}
		if err := writer.WriteByte('\n'); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if err := input.Close(); err != nil {
		return err
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	if err := output.Sync(); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	if err := os.Remove(f.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(tempPath, f.path); err != nil {
		return err
	}
	if err := os.Chmod(f.path, 0o600); err != nil {
		return err
	}
	if err := f.removeLegacyBackups(); err != nil {
		return err
	}
	success = true
	return nil
}

func (f *RollingFile) removeLegacyBackups() error {
	directory := filepath.Dir(f.path)
	base := filepath.Base(f.path)
	extension := filepath.Ext(base)
	stem := strings.TrimSuffix(base, extension)
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		legacy := strings.HasPrefix(name, base+".") ||
			(strings.HasPrefix(name, stem+"-") && strings.HasSuffix(name, extension))
		if legacy && name != base && name != base+".tmp" {
			if err := os.Remove(filepath.Join(directory, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

func (f *RollingFile) Path() string { return filepath.Clean(f.path) }
