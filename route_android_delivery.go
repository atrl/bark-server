package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
)

func init() {
	registerRoute("android", func(router fiber.Router) {
		router.Get("/android/poll/:device_key", routeAndroidPoll)
		router.Post("/android/transport/:device_key", routeAndroidTransport)
		router.Get("/android/sync/:device_key", routeAndroidSync)
		router.Post("/android/ack/:device_key", routeAndroidAck)
	})
}

func deviceTokensEqual(expected, provided string) bool {
	// Hash to fixed lengths before constant-time comparison.
	a, b := sha256.Sum256([]byte(expected)), sha256.Sum256([]byte(provided))
	return provided != "" && subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

func authenticatedAndroidDevice(c *fiber.Ctx) (string, bool) {
	key := c.Params("device_key")
	token, err := db.DeviceTokenByKey(key)
	if err != nil || !isAndroidDeviceToken(token) || !deviceTokensEqual(token, c.Get("X-Bark-Device-Token")) {
		return "", false
	}
	return key, true
}

func routeAndroidTransport(c *fiber.Ctx) error {
	key, ok := authenticatedAndroidDevice(c)
	if !ok {
		return c.Status(401).JSON(failed(401, "invalid Android device credentials"))
	}
	var transport androidTransport
	if err := c.BodyParser(&transport); err != nil {
		return c.Status(400).JSON(failed(400, "invalid transport JSON"))
	}
	if transport.NotificationMode == "" {
		transport.NotificationMode = "notification"
	}
	if transport.NotificationMode != "notification" && transport.NotificationMode != "data" {
		return c.Status(400).JSON(failed(400, "notification_mode must be notification or data"))
	}
	switch transport.Provider {
	case "fcm":
		if androidHub.sender == nil {
			return c.Status(503).JSON(failed(503, "FCM is not configured on this server"))
		}
		if strings.TrimSpace(transport.Token) == "" || len(transport.Token) > 4096 || strings.ContainsAny(transport.Token, "\r\n\t ") {
			return c.Status(400).JSON(failed(400, "invalid FCM token"))
		}
	case "poll":
		transport.Token = ""
	default:
		return c.Status(400).JSON(failed(400, "provider must be fcm or poll"))
	}
	if err := androidHub.setTransport(key, transport, c.Get("X-Bark-Device-Token")); err != nil {
		if errors.Is(err, errAndroidUnauthorized) {
			return c.Status(401).JSON(failed(401, "%s", err))
		}
		return c.Status(500).JSON(failed(500, "unable to persist Android transport"))
	}
	return c.JSON(data(map[string]string{"provider": transport.Provider, "server_url": androidHub.publicURL}))
}

func routeAndroidSync(c *fiber.Ctx) error {
	key, ok := authenticatedAndroidDevice(c)
	if !ok {
		return c.Status(401).JSON(failed(401, "invalid Android device credentials"))
	}
	timeout := max(0, min(c.QueryInt("timeout", 30), 60))
	limit := max(1, min(c.QueryInt("limit", 50), 100))
	result, err := androidHub.syncMessages(key, time.Duration(timeout)*time.Second, limit, c.Get("X-Bark-Device-Token"))
	if err != nil {
		if errors.Is(err, errAndroidUnauthorized) {
			return c.Status(401).JSON(failed(401, "%s", err))
		}
		return c.Status(500).JSON(failed(500, "unable to read Android outbox"))
	}
	c.Set("Cache-Control", "no-store")
	return c.JSON(data(result))
}

func routeAndroidAck(c *fiber.Ctx) error {
	key, ok := authenticatedAndroidDevice(c)
	if !ok {
		return c.Status(401).JSON(failed(401, "invalid Android device credentials"))
	}
	var request struct {
		DeliveryIDs []string `json:"delivery_ids"`
	}
	if err := c.BodyParser(&request); err != nil || len(request.DeliveryIDs) > 100 {
		return c.Status(400).JSON(failed(400, "delivery_ids must be an array of at most 100 IDs"))
	}
	for _, id := range request.DeliveryIDs {
		if id == "" || len(id) > 128 {
			return c.Status(400).JSON(failed(400, "invalid delivery ID"))
		}
	}
	if err := androidHub.ack(key, request.DeliveryIDs, c.Get("X-Bark-Device-Token")); err != nil {
		if errors.Is(err, errAndroidUnauthorized) {
			return c.Status(401).JSON(failed(401, "%s", err))
		}
		return c.Status(500).JSON(failed(500, "unable to acknowledge Android deliveries"))
	}
	return c.JSON(success())
}

func routeAndroidPoll(c *fiber.Ctx) error {
	deviceKey := c.Params("device_key")
	if deviceKey == "" {
		return c.Status(400).JSON(failed(400, "device key is empty"))
	}
	deviceToken, err := db.DeviceTokenByKey(deviceKey)
	if err != nil {
		return c.Status(400).JSON(failed(400, "failed to get device token"))
	}
	if !isAndroidDeviceToken(deviceToken) {
		return c.Status(400).JSON(failed(400, "device is not registered as android"))
	}
	box, err := androidHub.store.read(deviceKey)
	if err != nil {
		if errors.Is(err, errAndroidUnauthorized) {
			return c.Status(401).JSON(failed(401, "%s", err))
		}
		return c.Status(500).JSON(failed(500, "unable to read Android outbox"))
	}
	if box.Transport.Provider != "" {
		return c.Status(409).JSON(failed(409, "%s", errAndroidReliableTransport))
	}
	timeout := max(1, min(c.QueryInt("timeout", 30), 60))
	payload, ok := pollAndroidPush(deviceKey, time.Duration(timeout)*time.Second)
	if !ok {
		return c.SendStatus(fiber.StatusNoContent)
	}
	c.Set("Cache-Control", "no-store")
	return c.JSON(data(payload))
}
