package main

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const fcmScope = "https://www.googleapis.com/auth/firebase.messaging"
const googleTokenURL = "https://oauth2.googleapis.com/token"

// Provider acceptance is not confirmation that a notification was displayed.
type fcmSendResult struct {
	Accepted     bool
	Retryable    bool
	InvalidToken bool
	RetryAfter   time.Duration
}

type androidFCMSender interface {
	send(context.Context, string, androidDelivery) fcmSendResult
}

type httpFCMSender struct {
	client    *http.Client
	tokens    oauth2.TokenSource
	endpoint  string
	publicURL string
}

func newFCMSenderFromEnvironment() (androidFCMSender, error) {
	project := strings.TrimSpace(os.Getenv("BARK_FCM_PROJECT_ID"))
	publicURL := strings.TrimSpace(os.Getenv("BARK_PUBLIC_URL"))
	credentialsPath := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
	// FCM is optional: partial or absent setup keeps reliable polling available.
	if project == "" || publicURL == "" || credentialsPath == "" {
		return nil, nil
	}
	credentials, err := os.ReadFile(credentialsPath)
	if err != nil {
		return nil, fmt.Errorf("cannot read GOOGLE_APPLICATION_CREDENTIALS")
	}
	return newHTTPFCMSender(project, publicURL, credentials, &http.Client{Timeout: 10 * time.Second})
}

func newHTTPFCMSender(project, publicURL string, credentials []byte, client *http.Client) (*httpFCMSender, error) {
	if !regexp.MustCompile(`^[a-z][a-z0-9-]{4,61}[a-z0-9]$`).MatchString(project) {
		return nil, fmt.Errorf("BARK_FCM_PROJECT_ID must be a Google Cloud project ID")
	}
	if err := validateBarkPublicURL(publicURL); err != nil {
		return nil, err
	}
	config, err := google.JWTConfigFromJSON(credentials, fcmScope)
	if err != nil || config.Email == "" || len(config.PrivateKey) == 0 {
		return nil, fmt.Errorf("GOOGLE_APPLICATION_CREDENTIALS must contain a service account private key")
	}
	block, _ := pem.Decode(config.PrivateKey)
	if block == nil {
		return nil, fmt.Errorf("service account private key is not valid PEM")
	}
	signingKey, parseErr := x509.ParsePKCS1PrivateKey(block.Bytes)
	if parseErr != nil {
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("service account private key cannot be parsed")
		}
		var ok bool
		signingKey, ok = parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("service account private key must be RSA")
		}
	}
	if err := signingKey.Validate(); err != nil {
		return nil, fmt.Errorf("service account private key is invalid")
	}
	// Only send signed assertions to Google's OAuth endpoint, never a URL supplied
	// by an inbound request or a credential file with an unexpected token_uri.
	config.TokenURL = googleTokenURL
	authContext := context.WithValue(context.Background(), oauth2.HTTPClient, client)
	return &httpFCMSender{
		client:    client,
		tokens:    oauth2.ReuseTokenSource(nil, config.TokenSource(authContext)),
		endpoint:  "https://fcm.googleapis.com/v1/projects/" + project + "/messages:send",
		publicURL: strings.TrimRight(publicURL, "/"),
	}, nil
}

func payloadString(payload map[string]interface{}, key string) string {
	if value, ok := payload[key]; ok && value != nil {
		return fmt.Sprint(value)
	}
	return ""
}

func trimUTF8Bytes(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func fcmMessage(token, publicURL string, delivery androidDelivery) map[string]interface{} {
	tag := delivery.NotificationTag
	if tag == "" {
		tag = androidNotificationTag(publicURL, delivery)
	}
	data := map[string]string{
		"bark_delivery_id":      delivery.DeliveryID,
		"bark_server_url":       publicURL,
		"bark_notification_tag": tag,
		"notification_tag":      tag,
	}
	android := map[string]interface{}{"priority": "high", "ttl": "86400s"}
	message := map[string]interface{}{"token": token, "data": data, "android": android}
	deletion := strings.ToLower(payloadString(delivery.Payload, "delete"))
	if requiresAndroidLocalHandling(delivery) || (deletion != "" && deletion != "0" && deletion != "false") {
		// Control messages need app execution; Android can delay data-only delivery.
		if deletion != "" && deletion != "0" && deletion != "false" {
			data["bark_control"] = "delete"
		}
		return message
	}
	title, body := payloadString(delivery.Payload, "title"), payloadString(delivery.Payload, "body")
	if payloadString(delivery.Payload, "ciphertext") != "" {
		// Never send encrypted contents, IVs, or user-supplied fallback text to FCM.
		title, body = "Bark", "收到加密消息，打开 Bark 查看"
	} else if subtitle := payloadString(delivery.Payload, "subtitle"); subtitle != "" {
		body = subtitle + "\n" + body
	}
	if title == "" {
		title = "Bark"
	}
	message["notification"] = map[string]string{"title": trimUTF8Bytes(title, 180), "body": trimUTF8Bytes(body, 2000)}
	android["notification"] = map[string]interface{}{
		"tag":           data["bark_notification_tag"],
		"channel_id":    "bark_default",
		"default_sound": true,
	}
	return message
}

func (s *httpFCMSender) send(ctx context.Context, token string, delivery androidDelivery) fcmSendResult {
	accessToken, err := s.tokens.Token()
	if err != nil {
		return fcmSendResult{Retryable: true}
	}
	raw, err := json.Marshal(map[string]interface{}{"message": fcmMessage(token, s.publicURL, delivery)})
	if err != nil {
		return fcmSendResult{}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(raw))
	if err != nil {
		return fcmSendResult{}
	}
	request.Header.Set("Authorization", "Bearer "+accessToken.AccessToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := s.client.Do(request)
	if err != nil {
		return fcmSendResult{Retryable: true}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 65536))
	if err != nil {
		return fcmSendResult{Retryable: true}
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		var result struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(body, &result) != nil || result.Name == "" {
			return fcmSendResult{Retryable: true}
		}
		return fcmSendResult{Accepted: true}
	}
	result := fcmSendResult{Retryable: response.StatusCode == 429 || response.StatusCode >= 500 || response.StatusCode == 401}
	if seconds, err := strconv.Atoi(response.Header.Get("Retry-After")); err == nil && seconds > 0 {
		result.RetryAfter = time.Duration(seconds) * time.Second
	} else if retryAt, err := http.ParseTime(response.Header.Get("Retry-After")); err == nil {
		result.RetryAfter = time.Until(retryAt)
	}
	var failure struct {
		Error struct {
			Details []struct {
				ErrorCode string `json:"errorCode"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &failure) == nil {
		for _, detail := range failure.Error.Details {
			if detail.ErrorCode == "UNREGISTERED" {
				result.InvalidToken = true
				result.Retryable = false
			}
		}
	}
	return result
}

func validateBarkPublicURL(publicURL string) error {
	parsed, err := url.Parse(publicURL)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || len(publicURL) > 512 {
		return fmt.Errorf("BARK_PUBLIC_URL must be an HTTPS URL without credentials, query or fragment")
	}
	return nil
}

func androidNotificationTag(publicURL string, delivery androidDelivery) string {
	id := payloadString(delivery.Payload, "id")
	if id == "" {
		id = delivery.DeliveryID
	}
	sum := sha256.Sum256([]byte(strings.TrimRight(publicURL, "/") + "\n" + payloadString(delivery.Payload, "device_key") + "\n" + id))
	return "bark:" + hex.EncodeToString(sum[:])
}

func requiresAndroidLocalHandling(delivery androidDelivery) bool {
	if delivery.NotificationMode == "data" {
		return true
	}
	for _, key := range []string{"call", "autocopy", "sound", "action", "actions", "volume", "ttl"} {
		value := strings.ToLower(payloadString(delivery.Payload, key))
		if value != "" && value != "0" && value != "false" {
			return true
		}
	}
	return false
}
