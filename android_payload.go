package main

import (
	"fmt"
	"github.com/finb/bark-server/v2/apns"
	"strings"
)

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
