package lockset

import "testing"

func TestSessionRejectsReverseOrder(t *testing.T) {
	locks := New()
	session := locks.Session("test-op")
	releaseCommit, err := session.Acquire(LevelCommitLane)
	if err != nil {
		t.Fatalf("acquire commit lane: %v", err)
	}
	defer releaseCommit()

	if _, err := session.Acquire(LevelRescueGate); err == nil {
		t.Fatal("expected reverse-order acquisition to fail")
	}
}

func TestSessionAllowsForwardOrder(t *testing.T) {
	locks := New()
	session := locks.Session("test-op")
	releaseCommit, err := session.Acquire(LevelCommitLane)
	if err != nil {
		t.Fatalf("acquire commit lane: %v", err)
	}
	releaseStore, err := session.Acquire(LevelStore)
	if err != nil {
		t.Fatalf("acquire store lock: %v", err)
	}
	releaseStore()
	releaseCommit()
}
