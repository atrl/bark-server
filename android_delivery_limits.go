package main

import (
	"fmt"
	"os"
	"strconv"
)

const defaultAndroidQueueBytes = 4 * 1024 * 1024
const defaultAndroidMessageBytes = 32 * 1024
const maxAndroidOutboxFileBytes = 32 * 1024 * 1024

type androidDeliveryLimits struct {
	Count        int
	QueueBytes   int
	MessageBytes int
}

func androidDeliveryLimitsFromEnvironment() (androidDeliveryLimits, error) {
	limits := androidDeliveryLimits{defaultAndroidQueueLimit, defaultAndroidQueueBytes, defaultAndroidMessageBytes}
	for _, setting := range []struct {
		name             string
		target           *int
		minimum, maximum int
	}{
		{"BARK_ANDROID_QUEUE_LIMIT", &limits.Count, 1, 100000},
		{"BARK_ANDROID_QUEUE_MAX_BYTES", &limits.QueueBytes, 64 * 1024, 16 * 1024 * 1024},
		{"BARK_ANDROID_MESSAGE_MAX_BYTES", &limits.MessageBytes, 1024, 256 * 1024},
	} {
		if value := os.Getenv(setting.name); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed < setting.minimum || parsed > setting.maximum {
				return limits, fmt.Errorf("%s must be an integer between %d and %d", setting.name, setting.minimum, setting.maximum)
			}
			*setting.target = parsed
		}
	}
	if limits.MessageBytes > limits.QueueBytes {
		return limits, fmt.Errorf("BARK_ANDROID_MESSAGE_MAX_BYTES must not exceed BARK_ANDROID_QUEUE_MAX_BYTES")
	}
	return limits, nil
}
