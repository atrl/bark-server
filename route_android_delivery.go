package main

import (
	"time"

	"github.com/gofiber/fiber/v2"
)

func init() {
	registerRoute("android", func(router fiber.Router) {
		router.Get("/android/poll/:device_key", routeAndroidPoll)
	})
}

func routeAndroidPoll(c *fiber.Ctx) error {
	deviceKey := c.Params("device_key")
	if deviceKey == "" {
		return c.Status(400).JSON(failed(400, "device key is empty"))
	}

	deviceToken, err := db.DeviceTokenByKey(deviceKey)
	if err != nil {
		return c.Status(400).JSON(failed(400, "failed to get device token: %v", err))
	}
	if !isAndroidDeviceToken(deviceToken) {
		return c.Status(400).JSON(failed(400, "device is not registered as android"))
	}

	timeoutSeconds := c.QueryInt("timeout", 30)
	if timeoutSeconds < 1 {
		timeoutSeconds = 1
	}
	if timeoutSeconds > 60 {
		timeoutSeconds = 60
	}

	payload, ok := pollAndroidPush(deviceKey, time.Duration(timeoutSeconds)*time.Second)
	if !ok {
		return c.SendStatus(fiber.StatusNoContent)
	}
	return c.JSON(data(payload))
}
