package auth

import (
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestSetupLoginCheckLogout(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "auth.json"))
	needsSetup, err := store.NeedsSetup()
	if err != nil {
		t.Fatalf("needs setup: %v", err)
	}
	if !needsSetup {
		t.Fatal("new auth store should need setup")
	}
	token, err := store.Setup("test-password")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if token == "" {
		t.Fatal("setup returned empty token")
	}
	check, err := store.Check(token)
	if err != nil {
		t.Fatalf("check token: %v", err)
	}
	if check.NeedsSetup || !check.Authenticated {
		t.Fatalf("unexpected check result: %+v", check)
	}
	if _, err := store.Login("wrong-password"); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("wrong password err = %v", err)
	}
	nextToken, err := store.Login("test-password")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if nextToken == "" || nextToken == token {
		t.Fatalf("login token should be fresh, got setup=%q login=%q", token, nextToken)
	}
	if err := store.Logout(nextToken); err != nil {
		t.Fatalf("logout: %v", err)
	}
	check, err = store.Check(nextToken)
	if err != nil {
		t.Fatalf("check after logout: %v", err)
	}
	if check.Authenticated {
		t.Fatal("logged out token should not authenticate")
	}
}

func TestChangePasswordRevokesSessions(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "auth.json"))
	token, err := store.Setup("old-password")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := store.ChangePassword("old-password", "new-password"); err != nil {
		t.Fatalf("change password: %v", err)
	}
	check, err := store.Check(token)
	if err != nil {
		t.Fatalf("check old token: %v", err)
	}
	if check.Authenticated {
		t.Fatal("old token should be revoked after password change")
	}
	if _, err := store.Login("old-password"); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("old password should fail, got %v", err)
	}
	if _, err := store.Login("new-password"); err != nil {
		t.Fatalf("new password login: %v", err)
	}
}

func TestChangePasswordCanKeepCurrentSession(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "auth.json"))
	token, err := store.Setup("old-password")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	otherToken, err := store.Login("old-password")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if err := store.ChangePassword("old-password", "new-password", token); err != nil {
		t.Fatalf("change password: %v", err)
	}
	check, err := store.Check(token)
	if err != nil {
		t.Fatalf("check kept token: %v", err)
	}
	if !check.Authenticated {
		t.Fatal("kept token should remain authenticated after password change")
	}
	check, err = store.Check(otherToken)
	if err != nil {
		t.Fatalf("check other token: %v", err)
	}
	if check.Authenticated {
		t.Fatal("other token should be revoked after password change")
	}
}


func TestLoginRateLimitBackoffAndClearOnSuccess(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "auth.json"))
	var nowUnix atomic.Int64
	nowUnix.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix())
	store.SetNow(func() time.Time { return time.Unix(nowUnix.Load(), 0).UTC() })
	if _, err := store.Setup("test-password"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	for i := 0; i < loginFailureLimit; i++ {
		if _, err := store.Login("wrong-password"); !errors.Is(err, ErrInvalidCredential) {
			t.Fatalf("attempt %d: want invalid credential, got %v", i+1, err)
		}
	}
	if _, err := store.Login("wrong-password"); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("after limit: want too many attempts, got %v", err)
	}
	if _, err := store.Login("test-password"); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("correct password during lockout should still 429: %v", err)
	}
	nowUnix.Add(int64(loginFailureWindow.Seconds()) + 1)
	if _, err := store.Login("wrong-password"); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("after window wrong password should be invalid again, got %v", err)
	}
	// Four more failures to re-lock (1 already above + 4 = 5)
	for i := 0; i < loginFailureLimit-1; i++ {
		if _, err := store.Login("wrong-password"); !errors.Is(err, ErrInvalidCredential) {
			t.Fatalf("re-lock attempt %d: %v", i+1, err)
		}
	}
	if _, err := store.Login("wrong-password"); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("re-lock should 429, got %v", err)
	}
	nowUnix.Add(int64(loginFailureWindow.Seconds()) + 1)
	if _, err := store.Login("test-password"); err != nil {
		t.Fatalf("success after window: %v", err)
	}
	// success clears counter: 4 failures must not lock
	for i := 0; i < loginFailureLimit-1; i++ {
		if _, err := store.Login("wrong-password"); !errors.Is(err, ErrInvalidCredential) {
			t.Fatalf("post-success attempt %d: %v", i+1, err)
		}
	}
	if _, err := store.Login("test-password"); err != nil {
		t.Fatalf("login after partial failures should succeed: %v", err)
	}
}
