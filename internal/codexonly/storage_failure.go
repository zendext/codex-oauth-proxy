package codexonly

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
)

var ErrStorageFailure = errors.New("SQLite storage failure")

type storageFailureState struct {
	once     sync.Once
	mu       sync.RWMutex
	err      error
	errors   chan error
	onReport func()
}

func newStorageFailureState(onReport func()) *storageFailureState {
	return &storageFailureState{
		errors:   make(chan error, 1),
		onReport: onReport,
	}
}

func (s *storageFailureState) report(err error) error {
	if s == nil || err == nil {
		return err
	}
	s.once.Do(func() {
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		if s.onReport != nil {
			s.onReport()
		}
		s.errors <- err
	})
	return s.current()
}

func (s *storageFailureState) current() error {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.err
}

func (s *storageFailureState) channel() <-chan error {
	if s == nil {
		return nil
	}
	return s.errors
}

func (s *storageFailureState) commit(commit func()) error {
	if s == nil {
		commit()
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	commit()
	return nil
}

func expectedStoreError(err error) bool {
	return errors.Is(err, ErrInvalidInput) ||
		errors.Is(err, ErrDuplicateUserName) ||
		errors.Is(err, ErrUserNotFound) ||
		errors.Is(err, ErrInvalidAPIKey) ||
		errors.Is(err, ErrDisabledCredential) ||
		errors.Is(err, sql.ErrNoRows) ||
		errors.Is(err, sql.ErrTxDone) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

func (s *UserStore) checkReady() error {
	if s == nil || s.db == nil {
		return ErrInvalidInput
	}
	return s.failures.current()
}

func (s *UserStore) databaseError(operation string, err error) error {
	if err == nil || expectedStoreError(err) {
		return err
	}
	if current := s.failures.current(); current != nil {
		return current
	}
	failure := fmt.Errorf("%w: %s: %w", ErrStorageFailure, operation, err)
	return s.failures.report(failure)
}
