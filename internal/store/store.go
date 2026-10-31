package store

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var ErrExists = errors.New("user already exists")

type User struct {
	Username     string    `json:"username"`
	PasswordHash string    `json:"password_hash"`
	CreatedAt    time.Time `json:"created_at"`
}

type Device struct {
	ID        string    `json:"id"`
	Username  string    `json:"username"`
	Name      string    `json:"name"`
	Platform  string    `json:"platform"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

type disk struct {
	JWTSecret []byte             `json:"jwt_secret"`
	Users     map[string]*User   `json:"users"`
	Devices   map[string]*Device `json:"devices"`
}

type Store struct {
	mu   sync.RWMutex
	path string
	d    disk
}

func Open(path string) (*Store, error) {
	s := &Store{path: path}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &s.d); err != nil {
			return nil, fmt.Errorf("parse store %s: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return nil, err
	}
	if s.d.Users == nil {
		s.d.Users = map[string]*User{}
	}
	if s.d.Devices == nil {
		s.d.Devices = map[string]*Device{}
	}
	if len(s.d.JWTSecret) == 0 {
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, err
		}
		s.d.JWTSecret = secret
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) Secret() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]byte(nil), s.d.JWTSecret...)
}

func (s *Store) CreateUser(username, passwordHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.d.Users[username]; ok {
		return ErrExists
	}
	s.d.Users[username] = &User{Username: username, PasswordHash: passwordHash, CreatedAt: time.Now().UTC()}
	if err := s.saveLocked(); err != nil {
		delete(s.d.Users, username)
		return err
	}
	return nil
}

func (s *Store) User(username string) (User, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.d.Users[username]
	if !ok {
		return User{}, false
	}
	return *u, true
}

func (s *Store) UpsertDevice(d Device) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	existing, ok := s.d.Devices[d.ID]
	if ok {
		if existing.Username != d.Username {
			return fmt.Errorf("device %s belongs to another account", d.ID)
		}
		existing.Name = d.Name
		existing.Platform = d.Platform
		existing.LastSeen = now
	} else {
		d.FirstSeen = now
		d.LastSeen = now
		s.d.Devices[d.ID] = &d
	}
	return s.saveLocked()
}

func (s *Store) MarkSeen(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.d.Devices[id]
	if !ok {
		return nil
	}
	d.LastSeen = time.Now().UTC()
	return s.saveLocked()
}

func (s *Store) Devices(username string) []Device {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Device, 0, len(s.d.Devices))
	for _, d := range s.d.Devices {
		if d.Username == username {
			out = append(out, *d)
		}
	}
	return out
}

func (s *Store) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s.d, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".store-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path)
}
