package main

import "testing"

// newTestStore returns an in-memory SQLite store and registers cleanup.
func newTestStore(t *testing.T) Store {
	t.Helper()
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("newTestStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// newTestBroker returns a broker backed by an in-memory store.
func newTestBroker(t *testing.T, sync, async bool) *Broker {
	t.Helper()
	broker, err := NewBroker(newTestStore(t), "", sync, async)
	if err != nil {
		t.Fatalf("newTestBroker: %v", err)
	}
	return broker
}
