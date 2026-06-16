package main

import (
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v2"
)

func TestRouterAuthRequiresAndroidPollCredentials(t *testing.T) {
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
	if res.StatusCode != fiber.StatusTeapot {
		t.Fatalf("android poll should require auth: want %d, got %d", fiber.StatusTeapot, res.StatusCode)
	}

	req, _ = http.NewRequest("GET", "/android/poll/android-key", nil)
	req.Host = "example.com"
	req.SetBasicAuth("bark-user", "bark-pass")
	res, err = app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != fiber.StatusNoContent {
		t.Fatalf("android poll with credentials should pass: want %d, got %d", fiber.StatusNoContent, res.StatusCode)
	}

	req, _ = http.NewRequest("POST", "/push", nil)
	req.Host = "example.com"
	res, err = app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != fiber.StatusTeapot {
		t.Fatalf("push route should still require auth: want %d, got %d", fiber.StatusTeapot, res.StatusCode)
	}
}
