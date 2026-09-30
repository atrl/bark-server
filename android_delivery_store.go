package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Both stores apply changes transactionally: errors leave the previous state.
type memoryAndroidDeliveryStore struct {
	limit int
	mu    sync.Mutex
	boxes map[string][]byte
}

func newMemoryAndroidDeliveryStore(limit int) *memoryAndroidDeliveryStore {
	return &memoryAndroidDeliveryStore{limit: limit, boxes: make(map[string][]byte)}
}

func decodeAndroidOutbox(key string, raw []byte) (androidDeviceOutbox, error) {
	box := androidDeviceOutbox{Version: 1, DeviceKey: key, Messages: []androidDelivery{}}
	if len(raw) == 0 {
		return box, nil
	}
	if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
		// Migrate legacy payload-only queues on their next update, without dropping any.
		var payloads []map[string]interface{}
		if err := json.Unmarshal(raw, &payloads); err != nil {
			return box, err
		}
		for _, payload := range payloads {
			box.Messages = append(box.Messages, newAndroidDelivery(payload))
		}
		return box, nil
	}
	if err := json.Unmarshal(raw, &box); err != nil {
		return box, err
	}
	if box.Version != 1 || (key != "" && box.DeviceKey != key) {
		return box, fmt.Errorf("invalid android outbox identity or version")
	}
	return box, nil
}

func (s *memoryAndroidDeliveryStore) read(key string) (androidDeviceOutbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return decodeAndroidOutbox(key, s.boxes[key])
}

func (s *memoryAndroidDeliveryStore) update(key string, change func(*androidDeviceOutbox) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	box, err := decodeAndroidOutbox(key, s.boxes[key])
	if err != nil {
		return err
	}
	previousCount := len(box.Messages)
	if err := change(&box); err != nil {
		return err
	}
	if s.limit > 0 && len(box.Messages) > s.limit && len(box.Messages) > previousCount {
		return errAndroidQueueFull
	}
	raw, err := json.Marshal(box)
	if err != nil {
		return err
	}
	s.boxes[key] = raw
	return nil
}

func (s *memoryAndroidDeliveryStore) deviceKeys() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.boxes))
	for key := range s.boxes {
		keys = append(keys, key)
	}
	return keys, nil
}

type fileAndroidDeliveryStore struct {
	dir   string
	limit int
	mu    sync.Mutex
}

func newFileAndroidDeliveryStore(dir string, limit int) (*fileAndroidDeliveryStore, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	return &fileAndroidDeliveryStore{dir: dir, limit: limit}, nil
}

func (s *fileAndroidDeliveryStore) readUnlocked(key string) (androidDeviceOutbox, error) {
	raw, err := os.ReadFile(s.queueFile(key))
	if err != nil && !os.IsNotExist(err) {
		return androidDeviceOutbox{}, err
	}
	return decodeAndroidOutbox(key, raw)
}

func (s *fileAndroidDeliveryStore) read(key string) (androidDeviceOutbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readUnlocked(key)
}

func (s *fileAndroidDeliveryStore) update(key string, change func(*androidDeviceOutbox) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	box, err := s.readUnlocked(key)
	if err != nil {
		return err
	}
	previousCount := len(box.Messages)
	if err := change(&box); err != nil {
		return err
	}
	if s.limit > 0 && len(box.Messages) > s.limit && len(box.Messages) > previousCount {
		return errAndroidQueueFull
	}
	raw, err := json.Marshal(box)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(s.dir, ".outbox-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(raw); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), s.queueFile(key)); err != nil {
		return err
	}
	dir, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *fileAndroidDeliveryStore) deviceKeys() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	keys := []string{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		// Old queues cannot have a registered FCM transport; migrate on sync/push.
		if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
			continue
		}
		box, err := decodeAndroidOutbox("", raw)
		if err != nil {
			return nil, err
		}
		if box.DeviceKey != "" {
			keys = append(keys, box.DeviceKey)
		}
	}
	return keys, nil
}

func (s *fileAndroidDeliveryStore) queueFile(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(s.dir, hex.EncodeToString(sum[:])+".json")
}
