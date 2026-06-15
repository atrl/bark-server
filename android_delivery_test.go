package main

import (
	"testing"
	"time"

	"github.com/finb/bark-server/v2/apns"
)

func TestAndroidDeliveryPersistsPendingPushesAcrossHubRestart(t *testing.T) {
	store, err := newFileAndroidDeliveryStore(t.TempDir(), 64)
	if err != nil {
		t.Fatal(err)
	}
	firstHub := newAndroidDeliveryHub(store)

	if err := firstHub.deliver("android-key", androidPayloadFromPushMessage(&apns.PushMessage{
		Id:        "restart-pending-id",
		DeviceKey: "android-key",
		Title:     "Restart pending",
		Body:      "Delivered after restart",
		ExtParams: map[string]interface{}{
			"group": "durable",
		},
	})); err != nil {
		t.Fatal(err)
	}

	restartedHub := newAndroidDeliveryHub(store)
	payload, ok := restartedHub.poll("android-key", 10*time.Millisecond)
	if !ok {
		t.Fatal("expected pending Android payload after hub restart")
	}
	if got := payload["id"]; got != "restart-pending-id" {
		t.Fatalf("id: want restart-pending-id, got %#v in %#v", got, payload)
	}
	if got := payload["group"]; got != "durable" {
		t.Fatalf("group: want durable, got %#v in %#v", got, payload)
	}

	if payload, ok := restartedHub.poll("android-key", 10*time.Millisecond); ok {
		t.Fatalf("pending Android payload should be consumed once, got %#v", payload)
	}
}

func TestAndroidPayloadOmitsApnsDefaultSound(t *testing.T) {
	payload := androidPayloadFromPushMessage(&apns.PushMessage{
		DeviceKey: "android-key",
		Body:      "Default sound should not leak",
		Sound:     defaultApnsSound,
		ExtParams: map[string]interface{}{},
	})

	if got, ok := payload["sound"]; ok {
		t.Fatalf("android payload should omit APNs default sound, got %#v in %#v", got, payload)
	}
}

func TestAndroidPayloadPreservesExplicitBarkSound(t *testing.T) {
	payload := androidPayloadFromPushMessage(&apns.PushMessage{
		DeviceKey: "android-key",
		Body:      "Explicit sound",
		Sound:     "bell.caf",
		ExtParams: map[string]interface{}{},
	})

	if got := payload["sound"]; got != "bell.caf" {
		t.Fatalf("android payload sound: want bell.caf, got %#v in %#v", got, payload)
	}
}

func TestAndroidPayloadKeepsCanonicalAlertFieldsOverExtParams(t *testing.T) {
	payload := androidPayloadFromPushMessage(&apns.PushMessage{
		Id:        "canonical-id",
		DeviceKey: "android-key",
		Title:     "Canonical title",
		Subtitle:  "Canonical subtitle",
		Body:      "Canonical body",
		Sound:     "bell.caf",
		ExtParams: map[string]interface{}{
			"id":         "shadow-id",
			"title":      "Shadow title",
			"subtitle":   "Shadow subtitle",
			"body":       "Shadow body",
			"sound":      "shadow.caf",
			"device_key": "shadow-key",
			"group":      "ops",
		},
	})

	assertAndroidPayloadField(t, payload, "id", "canonical-id")
	assertAndroidPayloadField(t, payload, "device_key", "android-key")
	assertAndroidPayloadField(t, payload, "title", "Canonical title")
	assertAndroidPayloadField(t, payload, "subtitle", "Canonical subtitle")
	assertAndroidPayloadField(t, payload, "body", "Canonical body")
	assertAndroidPayloadField(t, payload, "sound", "bell.caf")
	assertAndroidPayloadField(t, payload, "group", "ops")
}

func TestAndroidPayloadKeepsExtParamIdWhenCanonicalIdIsMissing(t *testing.T) {
	payload := androidPayloadFromPushMessage(&apns.PushMessage{
		DeviceKey: "android-key",
		Body:      "Body",
		ExtParams: map[string]interface{}{
			"id":     "extension-id",
			"delete": "1",
		},
	})

	assertAndroidPayloadField(t, payload, "id", "extension-id")
	assertAndroidPayloadField(t, payload, "delete", "1")
}

func assertAndroidPayloadField(t *testing.T, payload map[string]interface{}, key string, want interface{}) {
	t.Helper()
	if got := payload[key]; got != want {
		t.Fatalf("%s: want %#v, got %#v in %#v", key, want, got, payload)
	}
}
