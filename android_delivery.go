package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/finb/bark-server/v2/apns"
)

const androidDeviceTokenPrefix = "android:"
const defaultAndroidQueueLimit = 64
const defaultApnsSound = "1107"

var androidHub = newAndroidDeliveryHub()

type androidDeliveryHub struct {
	store   androidDeliveryStore
	mu      sync.Mutex
	signals map[string]chan struct{}
}

type androidDeliveryStore interface {
	append(deviceKey string, payload map[string]interface{}) error
	pop(deviceKey string) (map[string]interface{}, bool, error)
}

func newAndroidDeliveryHub(stores ...androidDeliveryStore) *androidDeliveryHub {
	store := androidDeliveryStore(newMemoryAndroidDeliveryStore(defaultAndroidQueueLimit))
	if len(stores) > 0 && stores[0] != nil {
		store = stores[0]
	}
	return &androidDeliveryHub{
		store:   store,
		signals: make(map[string]chan struct{}),
	}
}

func isAndroidDeviceToken(token string) bool {
	return strings.HasPrefix(token, androidDeviceTokenPrefix)
}

func deliverAndroidPush(msg *apns.PushMessage) error {
	if msg.DeviceKey == "" {
		return fmt.Errorf("device key is empty")
	}
	return androidHub.deliver(msg.DeviceKey, androidPayloadFromPushMessage(msg))
}

func pollAndroidPush(deviceKey string, timeout time.Duration) (map[string]interface{}, bool) {
	return androidHub.poll(deviceKey, timeout)
}

func initializeAndroidDelivery(dataDir string) error {
	store, err := newFileAndroidDeliveryStore(filepath.Join(dataDir, "android-delivery"), defaultAndroidQueueLimit)
	if err != nil {
		return err
	}
	androidHub = newAndroidDeliveryHub(store)
	return nil
}

func (h *androidDeliveryHub) signal(deviceKey string) chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ch, ok := h.signals[deviceKey]; ok {
		return ch
	}
	ch := make(chan struct{}, 1)
	h.signals[deviceKey] = ch
	return ch
}

func (h *androidDeliveryHub) deliver(deviceKey string, payload map[string]interface{}) error {
	if err := h.store.append(deviceKey, payload); err != nil {
		return err
	}
	ch := h.signal(deviceKey)
	select {
	case ch <- struct{}{}:
	default:
	}
	return nil
}

func (h *androidDeliveryHub) poll(deviceKey string, timeout time.Duration) (map[string]interface{}, bool) {
	if payload, ok, err := h.store.pop(deviceKey); err == nil && ok {
		return payload, true
	}
	ch := h.signal(deviceKey)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ch:
		payload, ok, err := h.store.pop(deviceKey)
		return payload, err == nil && ok
	case <-timer.C:
		return nil, false
	}
}

func androidPayloadFromPushMessage(msg *apns.PushMessage) map[string]interface{} {
	payload := make(map[string]interface{}, len(msg.ExtParams)+6)
	if msg.Id != "" {
		payload["id"] = msg.Id
	}
	payload["device_key"] = msg.DeviceKey
	if msg.Title != "" {
		payload["title"] = msg.Title
	}
	if msg.Subtitle != "" {
		payload["subtitle"] = msg.Subtitle
	}
	if msg.Body != "" {
		payload["body"] = msg.Body
	}
	if msg.Sound != "" && msg.Sound != defaultApnsSound {
		payload["sound"] = msg.Sound
	}
	for k, v := range msg.ExtParams {
		key := strings.ToLower(k)
		if isAndroidCanonicalPayloadKey(key, payload) {
			continue
		}
		payload[key] = fmt.Sprintf("%v", v)
	}
	return payload
}

func isAndroidCanonicalPayloadKey(key string, payload map[string]interface{}) bool {
	switch key {
	case "id":
		_, exists := payload[key]
		return exists
	case "device_key", "title", "subtitle", "body", "sound":
		return true
	default:
		return false
	}
}

type memoryAndroidDeliveryStore struct {
	limit  int
	mu     sync.Mutex
	queues map[string][]map[string]interface{}
}

func newMemoryAndroidDeliveryStore(limit int) *memoryAndroidDeliveryStore {
	return &memoryAndroidDeliveryStore{
		limit:  limit,
		queues: make(map[string][]map[string]interface{}),
	}
}

func (s *memoryAndroidDeliveryStore) append(deviceKey string, payload map[string]interface{}) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	queue := append(s.queues[deviceKey], payload)
	if s.limit > 0 && len(queue) > s.limit {
		queue = queue[len(queue)-s.limit:]
	}
	s.queues[deviceKey] = queue
	return nil
}

func (s *memoryAndroidDeliveryStore) pop(deviceKey string) (map[string]interface{}, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	queue := s.queues[deviceKey]
	if len(queue) == 0 {
		return nil, false, nil
	}
	payload := queue[0]
	if len(queue) == 1 {
		delete(s.queues, deviceKey)
	} else {
		s.queues[deviceKey] = queue[1:]
	}
	return payload, true, nil
}

type fileAndroidDeliveryStore struct {
	dir   string
	limit int
	mu    sync.Mutex
}

func newFileAndroidDeliveryStore(dir string, limit int) (*fileAndroidDeliveryStore, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	return &fileAndroidDeliveryStore{dir: dir, limit: limit}, nil
}

func (s *fileAndroidDeliveryStore) append(deviceKey string, payload map[string]interface{}) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	queue, err := s.readQueue(deviceKey)
	if err != nil {
		return err
	}
	queue = append(queue, payload)
	if s.limit > 0 && len(queue) > s.limit {
		queue = queue[len(queue)-s.limit:]
	}
	return s.writeQueue(deviceKey, queue)
}

func (s *fileAndroidDeliveryStore) pop(deviceKey string) (map[string]interface{}, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	queue, err := s.readQueue(deviceKey)
	if err != nil {
		return nil, false, err
	}
	if len(queue) == 0 {
		return nil, false, nil
	}
	payload := queue[0]
	return payload, true, s.writeQueue(deviceKey, queue[1:])
}

func (s *fileAndroidDeliveryStore) readQueue(deviceKey string) ([]map[string]interface{}, error) {
	data, err := os.ReadFile(s.queueFile(deviceKey))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var queue []map[string]interface{}
	if err := json.Unmarshal(data, &queue); err != nil {
		return nil, err
	}
	return queue, nil
}

func (s *fileAndroidDeliveryStore) writeQueue(deviceKey string, queue []map[string]interface{}) error {
	path := s.queueFile(deviceKey)
	if len(queue) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	data, err := json.Marshal(queue)
	if err != nil {
		return err
	}
	tempPath := path + ".tmp"
	if err := os.WriteFile(tempPath, data, 0600); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

func (s *fileAndroidDeliveryStore) queueFile(deviceKey string) string {
	sum := sha256.Sum256([]byte(deviceKey))
	return filepath.Join(s.dir, hex.EncodeToString(sum[:])+".json")
}
