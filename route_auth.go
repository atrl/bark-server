package main

import (
	"github.com/gofiber/fiber/v2"
	"github.com/mritd/logger"
)

func routerAuth(user, passwd string, router fiber.Router, urlPrefix string) {
	if user == "" && passwd == "" {
		logger.Info("Bark Server Has No Basic Auth.")
		return
	}

	logger.Warn("Bark Server Basic Auth flags are deprecated and ignored; device_key guards push and Android polling routes.")
}
