package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"golang.org/x/oauth2"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type testRoundTripper func(*http.Request) (*http.Response, error)

func (f testRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func testServiceAccount(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]string{
		"type": "service_account", "project_id": "test-project", "private_key_id": "test-key-id",
		"private_key":  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})),
		"client_email": "test@test-project.iam.gserviceaccount.com", "token_uri": googleTokenURL,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestFCMHTTPV1UsesScopedOAuthAndCachesAccessToken(t *testing.T) {
	tokenCalls, sendCalls := 0, 0
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			tokenCalls++
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
				t.Error("wrong OAuth grant")
			}
			assertion := strings.Split(r.Form.Get("assertion"), ".")
			if len(assertion) != 3 {
				t.Error("missing signed JWT")
				w.WriteHeader(400)
				return
			}
			claimsRaw, err := base64.RawURLEncoding.DecodeString(assertion[1])
			if err != nil {
				t.Error(err)
			}
			var claims map[string]interface{}
			json.Unmarshal(claimsRaw, &claims)
			if claims["scope"] != fcmScope || claims["aud"] != googleTokenURL {
				t.Errorf("wrong scope/audience: %#v", claims)
			}
			io.WriteString(w, `{"access_token":"test-access-token","token_type":"Bearer","expires_in":3600}`)
		case "/v1/projects/test-project/messages:send":
			sendCalls++
			if r.Header.Get("Authorization") != "Bearer test-access-token" {
				t.Error("missing OAuth bearer")
			}
			raw, _ := io.ReadAll(r.Body)
			if strings.Contains(string(raw), "android:install-secret") || strings.Contains(string(raw), "device_key") {
				t.Error("device receive credentials leaked to FCM")
			}
			var envelope struct {
				Message map[string]interface{} `json:"message"`
			}
			json.Unmarshal(raw, &envelope)
			if envelope.Message["token"] != "fcm-registration-token" {
				t.Error("wrong FCM recipient")
			}
			notification := envelope.Message["notification"].(map[string]interface{})
			if notification["title"] != "Hello" || notification["body"] != "Sub\nBody" {
				t.Error("wrong notification payload")
			}
			android := envelope.Message["android"].(map[string]interface{})
			notif := android["notification"].(map[string]interface{})
			if notif["channel_id"] != "bark_default" {
				t.Error("wrong Android channel")
			}
			data := envelope.Message["data"].(map[string]interface{})
			if data["bark_delivery_id"] != "delivery-one" || data["bark_server_url"] != "https://bark.example" || data["notification_tag"] != notif["tag"] {
				t.Error("FCM/sync identity mismatch")
			}
			io.WriteString(w, `{"name":"projects/test-project/messages/accepted"}`)
		default:
			t.Errorf("unexpected outbound path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer mock.Close()
	destination, _ := url.Parse(mock.URL)
	client := &http.Client{Timeout: time.Second, Transport: testRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "oauth2.googleapis.com" && r.URL.Host != "fcm.googleapis.com" {
			t.Fatalf("unexpected outbound host %s", r.URL.Host)
		}
		copied := r.Clone(r.Context())
		u := *r.URL
		u.Scheme = destination.Scheme
		u.Host = destination.Host
		copied.URL = &u
		return http.DefaultTransport.RoundTrip(copied)
	})}
	sender, err := newHTTPFCMSender("test-project", "https://bark.example", testServiceAccount(t), client)
	if err != nil {
		t.Fatal(err)
	}
	message := androidDelivery{DeliveryID: "delivery-one", Payload: map[string]interface{}{"device_key": "private-device-key", "device_token": "android:install-secret", "title": "Hello", "subtitle": "Sub", "body": "Body"}}
	for i := 0; i < 2; i++ {
		if result := sender.send(context.Background(), "fcm-registration-token", message); !result.Accepted {
			t.Fatalf("send failed: %#v", result)
		}
	}
	if tokenCalls != 1 || sendCalls != 2 {
		t.Fatalf("token caching/send count: %d/%d", tokenCalls, sendCalls)
	}
}

func TestFCMEncryptedAndDataModesDoNotLeakPayload(t *testing.T) {
	encrypted := androidDelivery{DeliveryID: "encrypted-id", Payload: map[string]interface{}{"id": "business-id", "device_key": "private-key", "ciphertext": "SECRET_CIPHERTEXT", "iv": "SECRET_IV", "title": "PRIVATE_TITLE", "body": "PRIVATE_BODY", "url": "https://private.example"}}
	raw, _ := json.Marshal(fcmMessage("token", "https://bark.example", encrypted))
	for _, secret := range []string{"SECRET_CIPHERTEXT", "SECRET_IV", "PRIVATE_TITLE", "PRIVATE_BODY", "private.example", "private-key", "business-id"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("encrypted FCM request leaked %s", secret)
		}
	}
	if !strings.Contains(string(raw), "收到加密消息") {
		t.Fatal("encrypted notification missing generic text")
	}
	cases := []androidDelivery{
		{NotificationMode: "data", Payload: map[string]interface{}{"body": "private-data-mode"}},
		{Payload: map[string]interface{}{"delete": "1"}},
		{Payload: map[string]interface{}{"call": "1"}},
		{Payload: map[string]interface{}{"autocopy": "1"}},
		{Payload: map[string]interface{}{"sound": "bell.caf"}},
		{Payload: map[string]interface{}{"actions": "custom"}},
		{Payload: map[string]interface{}{"ttl": "60"}},
	}
	for _, message := range cases {
		message.DeliveryID = "data-id"
		request := fcmMessage("token", "https://bark.example", message)
		if _, ok := request["notification"]; ok {
			t.Fatalf("local-handling payload sent automatic notification: %#v", message)
		}
		data := request["data"].(map[string]string)
		if data["bark_delivery_id"] != "data-id" || data["notification_tag"] == "" {
			t.Fatal("data-only callback cannot identify delivery")
		}
	}
}

func TestNotificationTagsIsolateServersAndDevices(t *testing.T) {
	first := androidDelivery{DeliveryID: "one", Payload: map[string]interface{}{"device_key": "device-a", "id": strings.Repeat("long-id", 1000)}}
	sameBusiness := first
	sameBusiness.DeliveryID = "two"
	if androidNotificationTag("https://one.example", first) != androidNotificationTag("https://one.example", sameBusiness) {
		t.Fatal("same business id must replace same notification")
	}
	if len(androidNotificationTag("https://one.example", first)) != 69 {
		t.Fatal("tag is not fixed length")
	}
	if androidNotificationTag("https://one.example", first) == androidNotificationTag("https://two.example", first) {
		t.Fatal("tag collided across servers")
	}
	differentDevice := androidDelivery{DeliveryID: "one", Payload: map[string]interface{}{"device_key": "device-b", "id": first.Payload["id"]}}
	if androidNotificationTag("https://one.example", first) == androidNotificationTag("https://one.example", differentDevice) {
		t.Fatal("tag collided across device profiles")
	}
}

func TestFCMResponseClassification(t *testing.T) {
	for _, test := range []struct {
		name           string
		code           int
		body           string
		retry, invalid bool
	}{
		{"throttle", 429, `{}`, true, false},
		{"outage", 503, `{}`, true, false},
		{"unregistered", 404, `{"error":{"details":[{"errorCode":"UNREGISTERED"}]}}`, false, true},
		{"bad payload", 400, `{"error":{"details":[{"errorCode":"INVALID_ARGUMENT"}]}}`, false, false},
		{"bad acceptance", 200, `{}`, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(test.code)
				io.WriteString(w, test.body)
			}))
			defer mock.Close()
			sender := &httpFCMSender{client: mock.Client(), endpoint: mock.URL, publicURL: "https://bark.example", tokens: staticFCMToken{}}
			result := sender.send(context.Background(), "token", androidDelivery{DeliveryID: "id", Payload: map[string]interface{}{"body": "body"}})
			if result.Accepted || result.Retryable != test.retry || result.InvalidToken != test.invalid {
				t.Fatalf("classification: %#v", result)
			}
			if test.code == 429 && result.RetryAfter != 120*time.Second {
				t.Fatal("Retry-After discarded")
			}
		})
	}
}

type staticFCMToken struct{}

func (staticFCMToken) Token() (*oauth2.Token, error) {
	return &oauth2.Token{AccessToken: "mock", TokenType: "Bearer"}, nil
}
