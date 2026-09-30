package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/finb/bark-server/v2/apns"
	"github.com/google/uuid"
	"github.com/mritd/logger"
)

const androidDeviceTokenPrefix = "android:"
const defaultAndroidQueueLimit = 1024
const defaultApnsSound = "1107"

var errAndroidQueueFull = errors.New("android outbox is full; receive and acknowledge pending messages before retrying")
var errAndroidUnauthorized = errors.New("invalid Android device credentials")
var errAndroidReliableTransport = errors.New("device uses reliable delivery; use authenticated sync and ack")
var androidHub = newAndroidDeliveryHub()

// Delivery IDs identify deliveries, not the caller's replace/delete notification ID.
type androidDelivery struct {
	DeliveryID       string                 `json:"delivery_id"`
	Payload          map[string]interface{} `json:"payload"`
	CreatedAtMillis  int64                  `json:"created_at_millis"`
	FCMAccepted      bool                   `json:"fcm_accepted"`
	NotificationTag  string                 `json:"notification_tag"`
	FetchLeaseMillis int64                  `json:"fetch_lease_millis,omitempty"`
	Attempts         int                    `json:"attempts,omitempty"`
	RetryAtMillis    int64                  `json:"retry_at_millis,omitempty"`
	FCMBlocked       bool                   `json:"fcm_blocked,omitempty"`
	NotificationMode string                 `json:"-"`
}

type androidSyncMessage struct {
	DeliveryID      string                 `json:"delivery_id"`
	Payload         map[string]interface{} `json:"payload"`
	CreatedAtMillis int64                  `json:"created_at_millis"`
	FCMAccepted     bool                   `json:"fcm_accepted"`
	NotificationTag string                 `json:"notification_tag"`
}

type androidSyncResult struct {
	Messages []androidSyncMessage `json:"messages"`
	More     bool                 `json:"more"`
}

type androidTransport struct {
	Provider         string `json:"provider"`
	Token            string `json:"token,omitempty"`
	NotificationMode string `json:"notification_mode,omitempty"`
}

type androidDeviceOutbox struct {
	Version   int               `json:"version"`
	Revoked   bool              `json:"revoked,omitempty"`
	DeviceKey string            `json:"device_key"`
	Transport androidTransport  `json:"transport"`
	Messages  []androidDelivery `json:"messages"`
}

type androidDeliveryStore interface {
	read(deviceKey string) (androidDeviceOutbox, error)
	update(deviceKey string, change func(*androidDeviceOutbox) error) error
	deviceKeys() ([]string, error)
}

type androidDeliveryHub struct {
	store   androidDeliveryStore
	mu      sync.Mutex
	signals map[string]chan struct{}
	// Serialize fetching with sending. A short fetch lease prevents sending while
	// the client persists/ACKs; a lost sync response cannot disable FCM forever.
	deliveryLocks map[string]*sync.Mutex
	publicURL     string
	sender        androidFCMSender
	wake          chan struct{}
	cancel        context.CancelFunc
	done          chan struct{}
}

func newAndroidDeliveryHub(stores ...androidDeliveryStore) *androidDeliveryHub {
	store := androidDeliveryStore(newMemoryAndroidDeliveryStore(defaultAndroidQueueLimit))
	if len(stores) > 0 && stores[0] != nil {
		store = stores[0]
	}
	return &androidDeliveryHub{store: store, signals: make(map[string]chan struct{}), wake: make(chan struct{}, 1), deliveryLocks: make(map[string]*sync.Mutex)}
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
	hub := newAndroidDeliveryHub(store)
	hub.publicURL = strings.TrimRight(strings.TrimSpace(os.Getenv("BARK_PUBLIC_URL")), "/")
	if hub.publicURL != "" {
		if err := validateBarkPublicURL(hub.publicURL); err != nil {
			return err
		}
	}
	hub.sender, err = newFCMSenderFromEnvironment()
	if err != nil {
		return fmt.Errorf("invalid FCM configuration: %w", err)
	}
	androidHub = hub
	if hub.sender != nil {
		hub.start()
	}
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

func (h *androidDeliveryHub) notify(deviceKey string) {
	select {
	case h.signal(deviceKey) <- struct{}{}:
	default:
	}
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

func newAndroidDelivery(payload map[string]interface{}) androidDelivery {
	return androidDelivery{DeliveryID: uuid.NewString(), Payload: payload, CreatedAtMillis: time.Now().UnixMilli()}
}

func (h *androidDeliveryHub) deliver(deviceKey string, payload map[string]interface{}) error {
	message := newAndroidDelivery(payload)
	message.NotificationTag = androidNotificationTag(h.publicURL, message)
	if err := h.store.update(deviceKey, func(box *androidDeviceOutbox) error {
		if box.Revoked {
			return fmt.Errorf("Android device was revoked")
		}
		box.Messages = append(box.Messages, message)
		return nil
	}); err != nil {
		return err
	}
	h.notify(deviceKey)
	return nil
}

// Old poll remains destructive for clients without ACK. Registering either new
// transport prevents this endpoint from bypassing reliable outbox semantics.
func (h *androidDeliveryHub) popLegacy(deviceKey string) (map[string]interface{}, bool, error) {
	var payload map[string]interface{}
	err := h.store.update(deviceKey, func(box *androidDeviceOutbox) error {
		if box.Transport.Provider != "" {
			return errAndroidReliableTransport
		}
		if len(box.Messages) > 0 {
			payload = box.Messages[0].Payload
			box.Messages = box.Messages[1:]
		}
		return nil
	})
	return payload, payload != nil && err == nil, err
}

func (h *androidDeliveryHub) poll(deviceKey string, timeout time.Duration) (map[string]interface{}, bool) {
	ch := h.signal(deviceKey)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		payload, ok, err := h.popLegacy(deviceKey)
		if err != nil || ok {
			return payload, ok
		}
		select {
		case <-ch:
		case <-timer.C:
			return nil, false
		}
	}
}

func (h *androidDeliveryHub) setTransport(deviceKey string, transport androidTransport, credentials ...string) error {
	lock := h.deliveryLock(deviceKey)
	lock.Lock()
	defer lock.Unlock()
	if !validAndroidCredentials(deviceKey, credentials) {
		return errAndroidUnauthorized
	}
	if err := h.store.update(deviceKey, func(box *androidDeviceOutbox) error {
		changed := box.Transport != transport
		box.Transport = transport
		if changed {
			for i := range box.Messages {
				box.Messages[i].FCMBlocked = false
				box.Messages[i].RetryAtMillis = 0
			}
		}
		return nil
	}); err != nil {
		return err
	}
	h.notify(deviceKey)
	return nil
}

func (h *androidDeliveryHub) fetch(deviceKey string, limit int, credentials ...string) (androidSyncResult, error) {
	lock := h.deliveryLock(deviceKey)
	lock.Lock()
	defer lock.Unlock()
	if !validAndroidCredentials(deviceKey, credentials) {
		return androidSyncResult{}, errAndroidUnauthorized
	}
	result := androidSyncResult{Messages: []androidSyncMessage{}}
	err := h.store.update(deviceKey, func(box *androidDeviceOutbox) error {
		// A successful sync upgrades old registrations to ACK semantics too.
		if box.Transport.Provider == "" {
			box.Transport.Provider = "poll"
		}
		result.More = len(box.Messages) > limit
		for i := 0; i < len(box.Messages) && i < limit; i++ {
			msg := &box.Messages[i]
			if msg.FetchLeaseMillis == 0 {
				msg.FetchLeaseMillis = time.Now().Add(2 * time.Minute).UnixMilli()
			}
			if msg.NotificationTag == "" {
				msg.NotificationTag = androidNotificationTag(h.publicURL, *msg)
			}
			result.Messages = append(result.Messages, androidSyncMessage{msg.DeliveryID, msg.Payload, msg.CreatedAtMillis, msg.FCMAccepted, msg.NotificationTag})
		}
		return nil
	})
	return result, err
}

func (h *androidDeliveryHub) syncMessages(deviceKey string, timeout time.Duration, limit int, credentials ...string) (androidSyncResult, error) {
	ch := h.signal(deviceKey)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		result, err := h.fetch(deviceKey, limit, credentials...)
		if err != nil || len(result.Messages) > 0 || timeout <= 0 {
			return result, err
		}
		select {
		case <-ch:
		case <-timer.C:
			return result, nil
		}
	}
}

func (h *androidDeliveryHub) ack(deviceKey string, ids []string, credentials ...string) error {
	lock := h.deliveryLock(deviceKey)
	lock.Lock()
	defer lock.Unlock()
	if !validAndroidCredentials(deviceKey, credentials) {
		return errAndroidUnauthorized
	}
	acknowledged := make(map[string]bool, len(ids))
	for _, id := range ids {
		acknowledged[id] = true
	}
	return h.store.update(deviceKey, func(box *androidDeviceOutbox) error {
		remaining := make([]androidDelivery, 0, len(box.Messages))
		for _, message := range box.Messages {
			if !acknowledged[message.DeliveryID] {
				remaining = append(remaining, message)
			}
		}
		box.Messages = remaining
		return nil
	})
}

func (h *androidDeliveryHub) start() {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel, h.done = cancel, make(chan struct{})
	go func() {
		defer close(h.done)
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			if err := h.flush(ctx, time.Now()); err != nil && ctx.Err() == nil {
				logger.Error("Android FCM outbox storage operation failed; pending messages retained")
			}
			select {
			case <-ctx.Done():
				return
			case <-h.wake:
			case <-ticker.C:
			}
		}
	}()
}

func (h *androidDeliveryHub) close() {
	if h.cancel != nil {
		h.cancel()
		<-h.done
	}
}

func (h *androidDeliveryHub) flush(ctx context.Context, now time.Time) error {
	if h.sender == nil {
		return nil
	}
	keys, err := h.store.deviceKeys()
	if err != nil {
		return err
	}
	for _, key := range keys {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := h.flushDevice(ctx, key, now); err != nil {
			return err
		}
	}
	return nil
}

func (h *androidDeliveryHub) deliveryLock(key string) *sync.Mutex {
	h.mu.Lock()
	defer h.mu.Unlock()
	if lock := h.deliveryLocks[key]; lock != nil {
		return lock
	}
	lock := &sync.Mutex{}
	h.deliveryLocks[key] = lock
	return lock
}

func (h *androidDeliveryHub) flushDevice(ctx context.Context, key string, now time.Time) error {
	box, err := h.store.read(key)
	if err != nil {
		return err
	}
	for _, message := range box.Messages {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := h.flushMessage(ctx, key, message.DeliveryID, now); err != nil {
			return err
		}
	}
	return nil
}

func (h *androidDeliveryHub) flushMessage(ctx context.Context, key, deliveryID string, now time.Time) error {
	lock := h.deliveryLock(key)
	lock.Lock()
	defer lock.Unlock()
	box, err := h.store.read(key)
	if err != nil {
		return err
	}
	if box.Transport.Provider != "fcm" || box.Transport.Token == "" {
		return nil
	}
	for index, message := range box.Messages {
		if message.DeliveryID != deliveryID {
			continue
		}
		if message.FCMAccepted || message.FetchLeaseMillis > now.UnixMilli() || message.FCMBlocked || message.RetryAtMillis > now.UnixMilli() {
			return nil
		}

		// Do not let a delayed retry overwrite a newer accepted update/delete that
		// uses the same business notification ID. Keep the old event for sync/ACK.
		for _, newer := range box.Messages[index+1:] {
			if newer.FCMAccepted && newer.NotificationTag == message.NotificationTag && message.NotificationTag != "" {
				return h.store.update(key, func(current *androidDeviceOutbox) error {
					for i := range current.Messages {
						if current.Messages[i].DeliveryID == deliveryID {
							current.Messages[i].FCMBlocked = true
						}
					}
					return nil
				})
			}
		}
		message.NotificationMode = box.Transport.NotificationMode
		result := h.sender.send(ctx, box.Transport.Token, message)
		return h.store.update(key, func(current *androidDeviceOutbox) error {
			for i := range current.Messages {
				if current.Messages[i].DeliveryID != deliveryID {
					continue
				}
				item := &current.Messages[i]
				item.Attempts++
				item.FCMAccepted = result.Accepted
				item.FCMBlocked = !result.Accepted && !result.Retryable
				backoff := time.Minute * time.Duration(1<<min(item.Attempts-1, 6))
				if result.RetryAfter > backoff {
					backoff = result.RetryAfter
				}
				item.RetryAtMillis = now.Add(backoff).UnixMilli()
			}
			if result.InvalidToken {
				current.Transport.Token = ""
			}
			return nil
		})
	}
	return nil
}

func validAndroidCredentials(key string, credentials []string) bool {
	if len(credentials) == 0 {
		return true
	} // Internal helpers and unit tests.
	token, err := db.DeviceTokenByKey(key)
	return err == nil && isAndroidDeviceToken(token) && deviceTokensEqual(token, credentials[0])
}
