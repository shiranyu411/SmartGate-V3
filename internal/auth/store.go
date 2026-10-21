package auth

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

const (
	defaultIterations      = 120000
	defaultKeyLen          = 32
	defaultSessionTTL      = 24 * time.Hour
	minPasswordLen         = 6
	maxAuthStateBytes      = 1 << 20
	maxSessions            = 64
	loginFailureLimit      = 5
	loginFailureWindow     = 30 * time.Second
)

var (
	ErrNeedsSetup        = errors.New("auth setup required")
	ErrAlreadySetup      = errors.New("auth already setup")
	ErrInvalidCredential = errors.New("invalid credential")
	ErrTooManyAttempts   = errors.New("too many login attempts")
)

type Store struct {
	path string
	now  func() time.Time

	mu               sync.Mutex
	failedAttempts   int
	lockUntil        time.Time
}

type State struct {
	Password  *PasswordRecord `json:"password,omitempty"`
	Sessions  []SessionRecord `json:"sessions,omitempty"`
	UpdatedAt time.Time       `json:"updated_at"`
}

type PasswordRecord struct {
	Algorithm  string `json:"algorithm"`
	Iterations int    `json:"iterations"`
	Salt       string `json:"salt"`
	Hash       string `json:"hash"`
}

type SessionRecord struct {
	TokenHash string    `json:"token_hash"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type CheckResult struct {
	NeedsSetup    bool `json:"needs_setup"`
	Authenticated bool `json:"authenticated"`
}

func NewStore(path string) *Store {
	return &Store{path: path, now: time.Now}
}

// SetNow replaces the clock used for session TTL and login backoff (tests).
func (s *Store) SetNow(now func() time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if now == nil {
		s.now = time.Now
		return
	}
	s.now = now
}

func (s *Store) NeedsSetup() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.load()
	if err != nil {
		return false, err
	}
	return state.Password == nil, nil
}

func (s *Store) Setup(password string) (string, error) {
	if err := validatePassword(password); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.load()
	if err != nil {
		return "", err
	}
	if state.Password != nil {
		return "", ErrAlreadySetup
	}
	record, err := hashPassword(password)
	if err != nil {
		return "", err
	}
	token, session, err := newSession(s.now())
	if err != nil {
		return "", err
	}
	state.Password = &record
	state.Sessions = []SessionRecord{session}
	state.UpdatedAt = s.now().UTC()
	return token, s.save(state)
}

func (s *Store) Login(password string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if !s.lockUntil.IsZero() && now.Before(s.lockUntil) {
		return "", ErrTooManyAttempts
	}
	if !s.lockUntil.IsZero() && !now.Before(s.lockUntil) {
		s.failedAttempts = 0
		s.lockUntil = time.Time{}
	}
	state, err := s.load()
	if err != nil {
		return "", err
	}
	if state.Password == nil {
		return "", ErrNeedsSetup
	}
	if !verifyPassword(password, *state.Password) {
		s.failedAttempts++
		if s.failedAttempts >= loginFailureLimit {
			s.lockUntil = now.Add(loginFailureWindow)
		}
		return "", ErrInvalidCredential
	}
	token, session, err := newSession(now)
	if err != nil {
		return "", err
	}
	s.failedAttempts = 0
	s.lockUntil = time.Time{}
	state.Sessions = pruneSessions(append(state.Sessions, session), now)
	state.UpdatedAt = now.UTC()
	return token, s.save(state)
}

func (s *Store) Check(token string) (CheckResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.load()
	if err != nil {
		return CheckResult{}, err
	}
	result := CheckResult{NeedsSetup: state.Password == nil}
	if token == "" || state.Password == nil {
		return result, nil
	}
	hash := hashToken(token)
	now := s.now()
	for _, session := range state.Sessions {
		if now.After(session.ExpiresAt) {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(hash), []byte(session.TokenHash)) == 1 {
			result.Authenticated = true
			return result, nil
		}
	}
	return result, nil
}

func (s *Store) Logout(token string) error {
	if token == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.load()
	if err != nil {
		return err
	}
	hash := hashToken(token)
	next := state.Sessions[:0]
	for _, session := range state.Sessions {
		if subtle.ConstantTimeCompare([]byte(hash), []byte(session.TokenHash)) == 1 {
			continue
		}
		next = append(next, session)
	}
	state.Sessions = next
	state.UpdatedAt = s.now().UTC()
	return s.save(state)
}

func (s *Store) ChangePassword(oldPassword, newPassword string, keepTokens ...string) error {
	if err := validatePassword(newPassword); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.load()
	if err != nil {
		return err
	}
	if state.Password == nil {
		return ErrNeedsSetup
	}
	if !verifyPassword(oldPassword, *state.Password) {
		return ErrInvalidCredential
	}
	record, err := hashPassword(newPassword)
	if err != nil {
		return err
	}
	state.Password = &record
	state.Sessions = keptSessions(state.Sessions, keepTokens, s.now())
	state.UpdatedAt = s.now().UTC()
	return s.save(state)
}

func (s *Store) load() (State, error) {
	var state State
	b, err := readAuthFileBounded(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, err
	}
	if err := json.Unmarshal(b, &state); err != nil {
		return State{}, err
	}
	state.Sessions = pruneSessions(state.Sessions, s.now())
	return state, nil
}

func (s *Store) save(state State) error {
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if len(b) > maxAuthStateBytes {
		return fmt.Errorf("auth state exceeds %d bytes", maxAuthStateBytes)
	}
	if old, err := readAuthFileBounded(s.path); err == nil && bytes.Equal(old, b) {
		return nil
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func readAuthFileBounded(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > maxAuthStateBytes {
		return nil, fmt.Errorf("auth state exceeds %d bytes", maxAuthStateBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	body, readErr := io.ReadAll(io.LimitReader(file, maxAuthStateBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(body) > maxAuthStateBytes {
		return nil, fmt.Errorf("auth state exceeds %d bytes", maxAuthStateBytes)
	}
	return body, nil
}

func validatePassword(password string) error {
	if len(password) < minPasswordLen {
		return fmt.Errorf("password must be at least %d characters", minPasswordLen)
	}
	return nil
}

func hashPassword(password string) (PasswordRecord, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return PasswordRecord{}, err
	}
	return PasswordRecord{
		Algorithm:  "pbkdf2-sha256",
		Iterations: defaultIterations,
		Salt:       base64.RawURLEncoding.EncodeToString(salt),
		Hash:       base64.RawURLEncoding.EncodeToString(pbkdf2SHA256([]byte(password), salt, defaultIterations, defaultKeyLen)),
	}, nil
}

func verifyPassword(password string, record PasswordRecord) bool {
	if record.Algorithm != "pbkdf2-sha256" || record.Iterations <= 0 {
		return false
	}
	salt, err := base64.RawURLEncoding.DecodeString(record.Salt)
	if err != nil {
		return false
	}
	want, err := base64.RawURLEncoding.DecodeString(record.Hash)
	if err != nil {
		return false
	}
	got := pbkdf2SHA256([]byte(password), salt, record.Iterations, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

func newSession(now time.Time) (string, SessionRecord, error) {
	tokenBytes := make([]byte, 48)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", SessionRecord{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	return token, SessionRecord{
		TokenHash: hashToken(token),
		CreatedAt: now.UTC(),
		ExpiresAt: now.Add(defaultSessionTTL).UTC(),
	}, nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func pruneSessions(sessions []SessionRecord, now time.Time) []SessionRecord {
	next := sessions[:0]
	for _, session := range sessions {
		if now.Before(session.ExpiresAt) {
			next = append(next, session)
		}
	}
	if len(next) > maxSessions {
		next = next[len(next)-maxSessions:]
	}
	return next
}

func keptSessions(sessions []SessionRecord, tokens []string, now time.Time) []SessionRecord {
	if len(tokens) == 0 {
		return nil
	}
	keep := map[string]struct{}{}
	for _, token := range tokens {
		if token == "" {
			continue
		}
		keep[hashToken(token)] = struct{}{}
	}
	if len(keep) == 0 {
		return nil
	}
	next := sessions[:0]
	for _, session := range sessions {
		if now.After(session.ExpiresAt) {
			continue
		}
		if _, ok := keep[session.TokenHash]; ok {
			next = append(next, session)
		}
	}
	return next
}

func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	hLen := sha256.Size
	numBlocks := (keyLen + hLen - 1) / hLen
	out := make([]byte, 0, numBlocks*hLen)
	var block [4]byte
	for i := 1; i <= numBlocks; i++ {
		block[0] = byte(i >> 24)
		block[1] = byte(i >> 16)
		block[2] = byte(i >> 8)
		block[3] = byte(i)
		mac := hmac.New(sha256.New, password)
		mac.Write(salt)
		mac.Write(block[:])
		u := mac.Sum(nil)
		t := append([]byte(nil), u...)
		for j := 1; j < iter; j++ {
			mac = hmac.New(sha256.New, password)
			mac.Write(u)
			u = mac.Sum(nil)
			for k := range t {
				t[k] ^= u[k]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}
