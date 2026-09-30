package main

import (
	"github.com/gofiber/fiber/v2"
	"github.com/mritd/logger"
)

type DeviceInfo struct {
	DeviceKey   string `form:"device_key,omitempty" json:"device_key,omitempty" xml:"device_key,omitempty" query:"device_key,omitempty"`
	DeviceToken string `form:"device_token,omitempty" json:"device_token,omitempty" xml:"device_token,omitempty" query:"device_token,omitempty"`

	// compatible with old req
	OldDeviceKey   string `form:"key,omitempty" json:"key,omitempty" xml:"key,omitempty" query:"key,omitempty"`
	OldDeviceToken string `form:"devicetoken,omitempty" json:"devicetoken,omitempty" xml:"devicetoken,omitempty" query:"devicetoken,omitempty"`
}

func init() {
	registerRoute("register", func(router fiber.Router) {
		router.Post("/register", func(c *fiber.Ctx) error { return doRegister(c, false) })
		router.Get("/register/:device_key", doRegisterCheck)
	})

	// compatible with old requests
	registerRouteWithWeight("register_compat", 100, func(router fiber.Router) {
		router.Get("/register", func(c *fiber.Ctx) error { return doRegister(c, true) })
	})
}

func doRegister(c *fiber.Ctx, compat bool) error {
	var deviceInfo DeviceInfo
	if compat {
		if err := c.QueryParser(&deviceInfo); err != nil {
			return c.Status(400).JSON(failed(400, "request bind failed1: %v", err))
		}
	} else {
		if err := c.BodyParser(&deviceInfo); err != nil {
			return c.Status(400).JSON(failed(400, "request bind failed2: %v", err))
		}
	}

	if deviceInfo.DeviceKey == "" && deviceInfo.OldDeviceKey != "" {
		deviceInfo.DeviceKey = deviceInfo.OldDeviceKey
	}

	if deviceInfo.DeviceToken == "" {
		if deviceInfo.OldDeviceToken != "" {
			deviceInfo.DeviceToken = deviceInfo.OldDeviceToken
		} else {
			return c.Status(400).JSON(failed(400, "device token is empty"))
		}
	}

	// DeviceToken length is variable, but should not be too long.
	if len(deviceInfo.DeviceToken) > 160 {
		return c.Status(400).JSON(failed(400, "device token is invalid"))
	}

	// Existing Android keys are push addresses, not receive credentials. Knowing
	// one must not permit replacing its installation token and reading its outbox.
	lock := androidHub.deliveryLock(deviceInfo.DeviceKey)
	lock.Lock()
	defer lock.Unlock()
	if deviceInfo.DeviceKey != "" {
		existing, lookupErr := db.DeviceTokenByKey(deviceInfo.DeviceKey)
		box, storageErr := androidHub.store.read(deviceInfo.DeviceKey)
		if storageErr != nil {
			return c.Status(500).JSON(failed(500, "unable to read Android registration state"))
		}
		if box.Revoked {
			return c.Status(403).JSON(failed(403, "device key was revoked; register a new key"))
		}
		if lookupErr == nil && isAndroidDeviceToken(existing) && deviceInfo.DeviceToken != existing {
			if !deviceTokensEqual(existing, c.Get("X-Bark-Device-Token")) {
				return c.Status(403).JSON(failed(403, "existing Android installation token is required"))
			}
			if deviceInfo.DeviceToken != "deleted" && !isAndroidDeviceToken(deviceInfo.DeviceToken) {
				return c.Status(409).JSON(failed(409, "register a new key when changing device platform"))
			}
			if err := androidHub.store.update(deviceInfo.DeviceKey, func(current *androidDeviceOutbox) error {
				current.Transport = androidTransport{Provider: "poll"}
				if deviceInfo.DeviceToken == "deleted" {
					current.Messages = nil
					current.Revoked = true
				}
				return nil
			}); err != nil {
				return c.Status(500).JSON(failed(500, "unable to revoke Android transport"))
			}
			if deviceInfo.DeviceToken == "deleted" {
				if err := db.DeleteDeviceByKey(deviceInfo.DeviceKey); err != nil {
					return c.Status(500).JSON(failed(500, "unable to delete Android device"))
				}
				return c.JSON(data(map[string]string{"key": deviceInfo.DeviceKey, "device_key": deviceInfo.DeviceKey, "device_token": "deleted"}))
			}
		}
	}

	// if deviceInfo.DeviceKey=="", newKey will be filled with a new uuid
	// otherwise it equal to deviceInfo.DeviceKey
	newKey, err := db.SaveDeviceTokenByKey(deviceInfo.DeviceKey, deviceInfo.DeviceToken)
	if err != nil {
		logger.Error("device registration failed")
		return c.Status(500).JSON(failed(500, "device registration failed: %v", err))
	}
	deviceInfo.DeviceKey = newKey

	return c.Status(200).JSON(data(map[string]string{
		// compatible with old resp
		"key":          deviceInfo.DeviceKey,
		"device_key":   deviceInfo.DeviceKey,
		"device_token": deviceInfo.DeviceToken,
	}))
}

func doRegisterCheck(c *fiber.Ctx) error {
	deviceKey := c.Params("device_key")

	if deviceKey == "" {
		return c.Status(400).JSON(failed(400, "device key is empty"))
	}

	_, err := db.DeviceTokenByKey(deviceKey)
	if err != nil {
		return c.Status(400).JSON(failed(400, "%s", err.Error()))
	}
	return c.Status(200).JSON(success())
}
