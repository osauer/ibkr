package ibkr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// inactiveSymbolStore persists inactive symbol metadata across sessions
// so the connector can avoid re-requesting obviously delisted contracts
// on every startup. Unexported because the load/save methods reference
// the unexported inactiveSymbolState, so this contract cannot be
// satisfied by external callers. The daemon wires a file-backed store so
// confirmed delisted/no-definition instruments do not get re-requested after
// every restart.
type inactiveSymbolStore interface {
	LoadInactiveSymbols(ctx context.Context) (map[string]inactiveSymbolState, error)
	SaveInactiveSymbol(ctx context.Context, symbol string, state inactiveSymbolState) error
	RemoveInactiveSymbol(ctx context.Context, symbol string) error
}

const inactiveSymbolStoreVersion = 1

type fileInactiveSymbolStore struct {
	path string
	mu   sync.Mutex
}

type inactiveSymbolStoreFile struct {
	Version int                             `json:"version"`
	Symbols map[string]inactiveSymbolRecord `json:"symbols"`
}

type inactiveSymbolRecord struct {
	Reason   string    `json:"reason"`
	MarkedAt time.Time `json:"marked_at"`
}

func newFileInactiveSymbolStore(path string) inactiveSymbolStore {
	return &fileInactiveSymbolStore{path: strings.TrimSpace(path)}
}

func (s *fileInactiveSymbolStore) LoadInactiveSymbols(ctx context.Context) (map[string]inactiveSymbolState, error) {
	if s == nil || s.path == "" {
		return map[string]inactiveSymbolState{}, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	file, err := s.loadLocked(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]inactiveSymbolState, len(file.Symbols))
	for symbol, record := range file.Symbols {
		symbol = strings.ToUpper(strings.TrimSpace(symbol))
		reason := strings.TrimSpace(record.Reason)
		if symbol == "" || reason == "" || isCashRouteKey(symbol) || !shouldPersistInactiveReason(reason) {
			continue
		}
		out[symbol] = inactiveSymbolState{
			reason:   reason,
			markedAt: record.MarkedAt,
		}
	}
	return out, nil
}

func (s *fileInactiveSymbolStore) SaveInactiveSymbol(ctx context.Context, symbol string, state inactiveSymbolState) error {
	if s == nil || s.path == "" {
		return nil
	}
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	state.reason = strings.TrimSpace(state.reason)
	if symbol == "" || state.reason == "" || isCashRouteKey(symbol) || !shouldPersistInactiveReason(state.reason) {
		return nil
	}
	if state.markedAt.IsZero() {
		state.markedAt = time.Now()
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	file, err := s.loadLocked(ctx)
	if err != nil {
		return err
	}
	if file.Symbols == nil {
		file.Symbols = make(map[string]inactiveSymbolRecord)
	}
	file.Symbols[symbol] = inactiveSymbolRecord{
		Reason:   state.reason,
		MarkedAt: state.markedAt,
	}
	return s.writeLocked(ctx, file)
}

func (s *fileInactiveSymbolStore) RemoveInactiveSymbol(ctx context.Context, symbol string) error {
	if s == nil || s.path == "" {
		return nil
	}
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if symbol == "" {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	file, err := s.loadLocked(ctx)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	delete(file.Symbols, symbol)
	return s.writeLocked(ctx, file)
}

func (s *fileInactiveSymbolStore) loadLocked(ctx context.Context) (inactiveSymbolStoreFile, error) {
	if err := ctx.Err(); err != nil {
		return inactiveSymbolStoreFile{}, err
	}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return inactiveSymbolStoreFile{
			Version: inactiveSymbolStoreVersion,
			Symbols: map[string]inactiveSymbolRecord{},
		}, nil
	}
	if err != nil {
		return inactiveSymbolStoreFile{}, fmt.Errorf("read inactive symbol store %s: %w", s.path, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return inactiveSymbolStoreFile{
			Version: inactiveSymbolStoreVersion,
			Symbols: map[string]inactiveSymbolRecord{},
		}, nil
	}
	var file inactiveSymbolStoreFile
	if err := json.Unmarshal(data, &file); err != nil {
		return inactiveSymbolStoreFile{}, fmt.Errorf("decode inactive symbol store %s: %w", s.path, err)
	}
	if file.Version != inactiveSymbolStoreVersion {
		return inactiveSymbolStoreFile{}, fmt.Errorf("inactive symbol store %s has unsupported version %d", s.path, file.Version)
	}
	if file.Symbols == nil {
		file.Symbols = map[string]inactiveSymbolRecord{}
	}
	return file, nil
}

func (s *fileInactiveSymbolStore) writeLocked(ctx context.Context, file inactiveSymbolStoreFile) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	file.Version = inactiveSymbolStoreVersion
	if file.Symbols == nil {
		file.Symbols = map[string]inactiveSymbolRecord{}
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("encode inactive symbol store: %w", err)
	}
	data = append(data, '\n')
	return writePrivateInactiveSymbolStore(s.path, data)
}

func writePrivateInactiveSymbolStore(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	base := filepath.Base(path)
	tmp, err := os.CreateTemp(dir, base+".tmp.*")
	if err != nil {
		return fmt.Errorf("create temp inactive symbol store: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write inactive symbol store temp: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod inactive symbol store temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close inactive symbol store temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename inactive symbol store %s: %w", path, err)
	}
	return nil
}
