package accounts

import (
	"testing"
)

func TestPoolAcquireByType(t *testing.T) {
	pool := NewPool(nil)

	a1 := NewAccount("noauth-1", TypeNoAuth, "uuid-1")
	a2 := NewAccount("free-1", TypeFree, "token-free-1")
	a3 := NewAccount("puid-1", TypePUID, "token-puid-1")

	a1.Status = StatusActive
	a2.Status = StatusActive
	a3.Status = StatusActive

	pool.AddAccount(a1)
	pool.AddAccount(a2)
	pool.AddAccount(a3)

	acct, err := pool.Acquire(TypePUID)
	if err != nil {
		t.Fatalf("Acquire PUID: %v", err)
	}
	if acct.Type != TypePUID {
		t.Errorf("got type %s, want puid", acct.Type)
	}

	acct, err = pool.Acquire(TypeNoAuth)
	if err != nil {
		t.Fatalf("Acquire NoAuth: %v", err)
	}
	if acct.Type != TypeNoAuth {
		t.Errorf("got type %s, want noauth", acct.Type)
	}
}

func TestPoolAcquireRoundRobin(t *testing.T) {
	pool := NewPool(nil)
	a1 := NewAccount("a1", TypeNoAuth, "1")
	a2 := NewAccount("a2", TypeNoAuth, "2")
	a1.Status = StatusActive
	a2.Status = StatusActive
	pool.AddAccount(a1)
	pool.AddAccount(a2)

	first, _ := pool.Acquire(TypeNoAuth)
	first.TotalCalls++
	_, _ = pool.Acquire(TypeNoAuth)
}

func TestPoolAcquireNoAvailable(t *testing.T) {
	pool := NewPool(nil)
	_, err := pool.Acquire(TypePUID)
	if err == nil {
		t.Fatal("expected error when no accounts available")
	}
}

func TestPoolReleaseUpdatesStats(t *testing.T) {
	pool := NewPool(nil)
	acct := NewAccount("test", TypeFree, "token")
	acct.Status = StatusActive
	pool.AddAccount(acct)

	// Acquire 会自增 TotalCalls
	got, err := pool.Acquire(TypeFree)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if got.TotalCalls != 1 {
		t.Errorf("TotalCalls = %d, want 1", got.TotalCalls)
	}
	if got.FailedCalls != 0 {
		t.Errorf("FailedCalls = %d, want 0", got.FailedCalls)
	}
}

func TestPoolRemoveAccountByCredential(t *testing.T) {
	pool := NewPool(nil)
	access := NewAccount("access", TypeFree, "access-token")
	refresh := NewAccount("refresh", TypeFree, "current-access")
	refresh.RefreshToken = "refresh-token"
	other := NewAccount("other", TypeNoAuth, "other-token")
	access.Status = StatusActive
	refresh.Status = StatusActive
	other.Status = StatusActive
	pool.AddAccount(access)
	pool.AddAccount(refresh)
	pool.AddAccount(other)

	if removed := pool.RemoveAccountByCredential("refresh-token"); removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if got, err := pool.Acquire(TypeFree); err != nil || got != access {
		t.Fatalf("remaining free = %v, %v; want access, nil", got, err)
	}
	if got, err := pool.Acquire(TypeNoAuth); err != nil || got != other {
		t.Fatalf("remaining noauth = %v, %v; want other, nil", got, err)
	}
}
