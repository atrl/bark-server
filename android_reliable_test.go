package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"
)

type fakeFCMSender struct {
	results []fcmSendResult
	calls   []androidDelivery
}

func (s *fakeFCMSender) send(_ context.Context, _ string, message androidDelivery) fcmSendResult {
	s.calls = append(s.calls, message)
	if len(s.results) == 0 {
		return fcmSendResult{Accepted: true}
	}
	result := s.results[0]
	s.results = s.results[1:]
	return result
}

func mustDeliver(t *testing.T, hub *androidDeliveryHub, key string, payload map[string]interface{}) {
	t.Helper()
	if err := hub.deliver(key, payload); err != nil {
		t.Fatal(err)
	}
}
func mustSync(t *testing.T, hub *androidDeliveryHub, key string, limit int) androidSyncResult {
	t.Helper()
	result, err := hub.syncMessages(key, 0, limit)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestReliableOutboxSurvivesLostResponseAndRestartUntilAck(t *testing.T) {
	directory := t.TempDir()
	store, err := newFileAndroidDeliveryStore(directory, 3)
	if err != nil {
		t.Fatal(err)
	}
	hub := newAndroidDeliveryHub(store)
	for i := 0; i < 2; i++ {
		mustDeliver(t, hub, "key", map[string]interface{}{"id": "same-business-id", "body": "message"})
	}
	first := mustSync(t, hub, "key", 1)
	if len(first.Messages) != 1 || !first.More {
		t.Fatalf("unexpected page: %#v", first)
	}
	if first.Messages[0].DeliveryID == "same-business-id" || first.Messages[0].CreatedAtMillis == 0 {
		t.Fatal("delivery must have independent identity and timestamp")
	}
	store2, _ := newFileAndroidDeliveryStore(directory, 3)
	restarted := newAndroidDeliveryHub(store2)
	// Simulate a response lost after server-side read: no ACK was sent.
	second := mustSync(t, restarted, "key", 3)
	if len(second.Messages) != 2 || second.Messages[0].DeliveryID != first.Messages[0].DeliveryID {
		t.Fatalf("read/restart lost a delivery: %#v", second)
	}
	if second.Messages[0].DeliveryID == second.Messages[1].DeliveryID {
		t.Fatal("business ID reused as delivery ID")
	}
	if second.Messages[0].NotificationTag != second.Messages[1].NotificationTag {
		t.Fatal("business updates must use same notification tag")
	}
	id := first.Messages[0].DeliveryID
	if err := restarted.ack("key", []string{id, id}); err != nil {
		t.Fatal(err)
	}
	if err := restarted.ack("key", []string{id}); err != nil {
		t.Fatal(err)
	}
	remaining := mustSync(t, restarted, "key", 10)
	if len(remaining.Messages) != 1 || remaining.Messages[0].DeliveryID != second.Messages[1].DeliveryID {
		t.Fatal("ACK was not idempotent or removed unrelated delivery")
	}
}

func TestOutboxFullPreservesOldMessagesAndLegacyMigration(t *testing.T) {
	directory := t.TempDir()
	store, _ := newFileAndroidDeliveryStore(directory, 2)
	if err := os.WriteFile(store.queueFile("key"), []byte(`[{"id":"one"},{"id":"two"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	hub := newAndroidDeliveryHub(store)
	if err := hub.deliver("key", map[string]interface{}{"id": "three"}); !errors.Is(err, errAndroidQueueFull) {
		t.Fatalf("want queue full: %v", err)
	}
	result := mustSync(t, hub, "key", 10)
	if len(result.Messages) != 2 || result.Messages[0].Payload["id"] != "one" || result.Messages[1].Payload["id"] != "two" {
		t.Fatalf("old messages discarded: %#v", result)
	}
	repeated := mustSync(t, hub, "key", 10)
	if result.Messages[0].DeliveryID != repeated.Messages[0].DeliveryID {
		t.Fatal("migrated delivery ID changed")
	}
	if _, _, err := hub.popLegacy("key"); !errors.Is(err, errAndroidReliableTransport) {
		t.Fatal("legacy poll bypasses reliable ACK")
	}
}

func TestFCMRetryAcceptedAndFetchLease(t *testing.T) {
	hub := newAndroidDeliveryHub()
	sender := &fakeFCMSender{results: []fcmSendResult{{Retryable: true, RetryAfter: 3 * time.Minute}, {Accepted: true}}}
	hub.sender = sender
	if err := hub.setTransport("key", androidTransport{Provider: "fcm", Token: "registration-token"}); err != nil {
		t.Fatal(err)
	}
	mustDeliver(t, hub, "key", map[string]interface{}{"body": "retry"})
	now := time.Now()
	if err := hub.flush(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if err := hub.flush(context.Background(), now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(sender.calls) != 1 {
		t.Fatal("ignored Retry-After")
	}
	if err := hub.flush(context.Background(), now.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := hub.flush(context.Background(), now.Add(8*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(sender.calls) != 2 || sender.calls[0].DeliveryID != sender.calls[1].DeliveryID {
		t.Fatal("retry changed identity or resent accepted delivery")
	}
	result := mustSync(t, hub, "key", 50)
	if len(result.Messages) != 1 || !result.Messages[0].FCMAccepted {
		t.Fatal("FCM accepted delivery removed before ACK")
	}

	mustDeliver(t, hub, "key", map[string]interface{}{"body": "lost sync response"})
	mustSync(t, hub, "key", 50)
	before, _ := hub.store.read("key")
	lease := before.Messages[1].FetchLeaseMillis
	mustSync(t, hub, "key", 50)
	after, _ := hub.store.read("key")
	if after.Messages[1].FetchLeaseMillis != lease {
		t.Fatal("repeated reads renew lease indefinitely")
	}
	if err := hub.flush(context.Background(), time.UnixMilli(lease-1)); err != nil {
		t.Fatal(err)
	}
	if len(sender.calls) != 2 {
		t.Fatal("FCM raced client fetch persistence")
	}
	if err := hub.flush(context.Background(), time.UnixMilli(lease+1)); err != nil {
		t.Fatal(err)
	}
	if len(sender.calls) != 3 {
		t.Fatal("lost sync response permanently disabled FCM")
	}
}

func TestInvalidTokenRetainsOutboxAndTokenRotationResumes(t *testing.T) {
	hub := newAndroidDeliveryHub()
	sender := &fakeFCMSender{results: []fcmSendResult{{InvalidToken: true}, {Accepted: true}}}
	hub.sender = sender
	hub.setTransport("key", androidTransport{Provider: "fcm", Token: "old"})
	mustDeliver(t, hub, "key", map[string]interface{}{"body": "keep"})
	hub.flush(context.Background(), time.Now())
	box, _ := hub.store.read("key")
	if len(box.Messages) != 1 || box.Transport.Token != "" {
		t.Fatal("invalid token must clear binding, not message")
	}
	hub.setTransport("key", androidTransport{Provider: "fcm", Token: "new"})
	hub.flush(context.Background(), time.Now())
	box, _ = hub.store.read("key")
	if len(sender.calls) != 2 || !box.Messages[0].FCMAccepted {
		t.Fatal("rotation did not unblock pending delivery")
	}
	hub.setTransport("key", androidTransport{Provider: "poll"})
	box, _ = hub.store.read("key")
	if box.Transport.Token != "" {
		t.Fatal("poll retained FCM token")
	}
}

type blockingFCMSender struct {
	started chan struct{}
	release chan struct{}
}

func (s blockingFCMSender) send(ctx context.Context, _ string, _ androidDelivery) fcmSendResult {
	close(s.started)
	select {
	case <-s.release:
		return fcmSendResult{Accepted: true}
	case <-ctx.Done():
		return fcmSendResult{Retryable: true}
	}
}
func TestSlowFCMDoesNotBlockOtherDeviceSync(t *testing.T) {
	hub := newAndroidDeliveryHub()
	sender := blockingFCMSender{make(chan struct{}), make(chan struct{})}
	hub.sender = sender
	hub.setTransport("slow", androidTransport{Provider: "fcm", Token: "token"})
	mustDeliver(t, hub, "slow", map[string]interface{}{"body": "slow"})
	mustDeliver(t, hub, "other", map[string]interface{}{"body": "other"})
	finished := make(chan struct{})
	go func() { defer close(finished); hub.flushDevice(context.Background(), "slow", time.Now()) }()
	<-sender.started
	synced := make(chan struct{})
	go func() { hub.syncMessages("other", 0, 50); close(synced) }()
	select {
	case <-synced:
	case <-time.After(250 * time.Millisecond):
		close(sender.release)
		<-finished
		t.Fatal("one FCM timeout blocked another device")
	}
	close(sender.release)
	<-finished
}

func androidTestRequest(t *testing.T, method, path, token, body string) (int, []byte) {
	t.Helper()
	request, _ := http.NewRequest(method, path, bytes.NewBufferString(body))
	request.Host = "example.com"
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("X-Bark-Device-Token", token)
	}
	response, err := app.Test(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var result json.RawMessage
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, result
}

func isolateAndroidRoutes(t *testing.T) *androidDeliveryHub {
	t.Helper()
	previousDB, previousHub := db, androidHub
	db = testDeviceDatabase{"one": "android:secret-one", "two": "android:secret-two"}
	androidHub = newAndroidDeliveryHub()
	t.Cleanup(func() { db = previousDB; androidHub = previousHub })
	return androidHub
}

func TestAndroidReceiveAuthIsolationAndIdempotentAck(t *testing.T) {
	hub := isolateAndroidRoutes(t)
	mustDeliver(t, hub, "one", map[string]interface{}{"body": "private-one"})
	mustDeliver(t, hub, "two", map[string]interface{}{"body": "private-two"})
	for _, token := range []string{"", "android:wrong", "android:secret-two"} {
		for _, endpoint := range []struct{ method, path, body string }{{"GET", "/android/sync/one?timeout=0", ""}, {"POST", "/android/ack/one", `{"delivery_ids":[]}`}, {"POST", "/android/transport/one", `{"provider":"poll"}`}} {
			code, _ := androidTestRequest(t, endpoint.method, endpoint.path, token, endpoint.body)
			if code != 401 {
				t.Fatalf("unauthorized %s got %d", endpoint.path, code)
			}
		}
	}
	code, _ := androidTestRequest(t, "POST", "/android/transport/one", "android:secret-one", `{"provider":"fcm","token":"fcm-token"}`)
	if code != 503 {
		t.Fatal("unconfigured FCM claimed success")
	}
	code, raw := androidTestRequest(t, "GET", "/android/sync/one?timeout=0", "android:secret-one", "")
	if code != 200 {
		t.Fatalf("sync: %d %s", code, raw)
	}
	var envelope struct {
		Data androidSyncResult `json:"data"`
	}
	json.Unmarshal(raw, &envelope)
	id := envelope.Data.Messages[0].DeliveryID
	for i := 0; i < 2; i++ {
		code, _ = androidTestRequest(t, "POST", "/android/ack/one", "android:secret-one", `{"delivery_ids":["`+id+`"]}`)
		if code != 200 {
			t.Fatal("ACK failed")
		}
	}
	if len(mustSync(t, hub, "one", 50).Messages) != 0 || len(mustSync(t, hub, "two", 50).Messages) != 1 {
		t.Fatal("ACK crossed device boundary")
	}
}

func TestAndroidRegistrationCannotBypassReceiveAuth(t *testing.T) {
	hub := isolateAndroidRoutes(t)
	hub.setTransport("one", androidTransport{Provider: "fcm", Token: "old-fcm"})
	mustDeliver(t, hub, "one", map[string]interface{}{"body": "private"})
	for _, replacement := range []string{"android:attacker", "deleted", "ios-token"} {
		for _, method := range []string{"POST", "GET"} {
			path, body := "/register", `{"device_key":"one","device_token":"`+replacement+`"}`
			if method == "GET" {
				path += "?key=one&devicetoken=" + replacement
				body = ""
			}
			code, _ := androidTestRequest(t, method, path, "", body)
			if code != 403 {
				t.Fatalf("key-only %s replacement %s got %d", method, replacement, code)
			}
		}
	}
	code, _ := androidTestRequest(t, "POST", "/register", "", `{"device_key":"one","device_token":"android:secret-one"}`)
	if code != 200 {
		t.Fatal("same-token registration retry failed")
	}
	code, _ = androidTestRequest(t, "POST", "/register", "android:secret-one", `{"device_key":"one","device_token":"ios-token"}`)
	if code != 409 {
		t.Fatal("cross-platform conversion could discard receive auth")
	}
	code, _ = androidTestRequest(t, "POST", "/register", "android:secret-one", `{"device_key":"one","device_token":"android:rotated"}`)
	if code != 200 {
		t.Fatal("authorized token rotation failed")
	}
	box, _ := hub.store.read("one")
	if box.Transport.Token != "" {
		t.Fatal("rotation kept old FCM recipient")
	}
	code, _ = androidTestRequest(t, "POST", "/android/transport/one", "android:secret-one", `{"provider":"poll"}`)
	if code != 401 {
		t.Fatal("old token retained authority")
	}
	code, _ = androidTestRequest(t, "POST", "/register", "android:rotated", `{"device_key":"one","device_token":"deleted"}`)
	if code != 200 {
		t.Fatal("authorized deletion failed")
	}
	box, _ = hub.store.read("one")
	if len(box.Messages) != 0 || !box.Revoked {
		t.Fatal("deleted device retained private outbox")
	}
	if _, err := db.DeviceTokenByKey("one"); err == nil {
		t.Fatal("deleted mapping retained")
	}
	code, _ = androidTestRequest(t, "POST", "/register", "", `{"device_key":"one","device_token":"android:attacker"}`)
	if code != 403 {
		t.Fatal("deleted key could be revived by its holder")
	}
}

func TestDelayedFCMRetryCannotReplaceNewerAcceptedBusinessUpdate(t *testing.T) {
	hub := newAndroidDeliveryHub()
	sender := &fakeFCMSender{results: []fcmSendResult{{Retryable: true}, {Accepted: true}}}
	hub.sender = sender
	hub.setTransport("key", androidTransport{Provider: "fcm", Token: "token"})
	mustDeliver(t, hub, "key", map[string]interface{}{"id": "shared-id", "body": "old"})
	mustDeliver(t, hub, "key", map[string]interface{}{"id": "shared-id", "body": "new"})
	now := time.Now()
	if err := hub.flush(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if err := hub.flush(context.Background(), now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(sender.calls) != 2 {
		t.Fatal("older retry overwrote newer accepted update")
	}
	result := mustSync(t, hub, "key", 50)
	if len(result.Messages) != 2 {
		t.Fatal("superseded delivery removed without ACK")
	}
}
