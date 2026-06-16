package main

import (
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v2"
)

func TestRouterAuthDoesNotInterceptDeviceKeyRoutes(t *testing.T) {
	app := fiber.New()
	routerAuth("bark-user", "bark-pass", app, "")
	app.Get("/android/poll/:device_key", func(c *fiber.Ctx) error {
		return c.SendStatus(fiber.StatusNoContent)
	})
	app.Post("/push", func(c *fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	req, _ := http.NewRequest("GET", "/android/poll/android-key", nil)
	req.Host = "example.com"
	res, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != fiber.StatusNoContent {
		t.Fatalf("android poll should be guarded by its device key handler, not Basic Auth: want %d, got %d", fiber.StatusNoContent, res.StatusCode)
	}

	req, _ = http.NewRequest("POST", "/push", nil)
	req.Host = "example.com"
	res, err = app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != fiber.StatusOK {
		t.Fatalf("push route should be guarded by its device key handler, not Basic Auth: want %d, got %d", fiber.StatusOK, res.StatusCode)
	}
}
