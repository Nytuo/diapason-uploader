// Package devices keeps the per-device tokens handed out by pairing.
package devices

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"time"
)

// FileName is the state file kept next to the master token file.
const FileName = ".diapason-uploader-devices.json"

const maxDevices = 64

type Device struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Hash    string    `json:"hash"`
	Created time.Time `json:"created"`

	lastSeen time.Time
}

// View is the public shape returned to clients (never includes the hash).
type View struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Created  time.Time  `json:"created"`
	LastSeen *time.Time `json:"lastSeen,omitempty"`
}

type Store struct {
	path string

	mu      sync.Mutex
	devices map[string]*Device
}

// Open loads the store at path (missing file = empty).
func Open(path string) (*Store, error) {
	s := &Store{path: path, devices: map[string]*Device{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	var list []*Device
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	for _, d := range list {
		s.devices[d.Hash] = d
	}
	return s, nil
}

func hashOf(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Count returns the number of paired devices.
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.devices)
}

// Add registers a device and returns its new token (shown once).
func (s *Store) Add(name string) (string, View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.devices) >= maxDevices {
		return "", View{}, errors.New("too many paired devices; revoke some first")
	}
	if name == "" {
		name = "Unnamed device"
	}
	token := randHex(24)
	d := &Device{ID: randHex(6), Name: name, Hash: hashOf(token), Created: time.Now().UTC()}
	s.devices[d.Hash] = d
	if err := s.saveLocked(); err != nil {
		delete(s.devices, d.Hash)
		return "", View{}, err
	}
	return token, view(d), nil
}

// Verify reports whether token belongs to a paired device.
func (s *Store) Verify(token string) (View, bool) {
	if token == "" {
		return View{}, false
	}
	h := hashOf(token)
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[h]
	if !ok {
		return View{}, false
	}
	d.lastSeen = time.Now()
	return view(d), true
}

// List returns all devices, oldest first.
func (s *Store) List() []View {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]View, 0, len(s.devices))
	for _, d := range s.devices {
		out = append(out, view(d))
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Created.Before(out[j-1].Created); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Revoke removes a device by ID.
func (s *Store) Revoke(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for h, d := range s.devices {
		if d.ID == id {
			delete(s.devices, h)
			if err := s.saveLocked(); err != nil {
				s.devices[h] = d
				return false
			}
			return true
		}
	}
	return false
}

func view(d *Device) View {
	v := View{ID: d.ID, Name: d.Name, Created: d.Created}
	if !d.lastSeen.IsZero() {
		t := d.lastSeen
		v.LastSeen = &t
	}
	return v
}

func (s *Store) saveLocked() error {
	list := make([]*Device, 0, len(s.devices))
	for _, d := range s.devices {
		list = append(list, d)
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
