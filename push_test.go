package main

import (
	"bytes"
	"fmt"
	"net/http"
	"testing"
	"time"

	"io"

	"github.com/finb/bark-server/v2/database"
	"github.com/gofiber/fiber/v2"
	jsoniter "github.com/json-iterator/go"
)

// Before running the tests, a valid deviceToken must be set. Otherwise, the tests will fail.
const (
	deviceToken = "android:test-device"
	key         = "MemoryBaseKey"
)

var app *fiber.App

func TestMain(m *testing.M) {
	db = database.NewMemBase()
	db.SaveDeviceTokenByKey(key, deviceToken)
	app = NewServer()
	m.Run()
}

func TestRegister(t *testing.T) {
	Endpoint(t, []APITestCase{
		{
			Name:           "Normal registration",
			Method:         "GET",
			URL:            "/register?devicetoken=" + deviceToken,
			Body:           "",
			IsJson:         false,
			WantStatusCode: 200,
		},
		{
			Name:           "Registration with key",
			Method:         "GET",
			URL:            "/register?key=" + key + "&devicetoken=" + deviceToken,
			Body:           "",
			IsJson:         false,
			WantStatusCode: 200,
		},
		{
			Name:           "Registration with wrong key",
			Method:         "GET",
			URL:            "/register?key=" + "wrongKey" + "&devicetoken=" + deviceToken,
			Body:           "",
			IsJson:         false,
			WantStatusCode: 500,
		},
		{
			Name:           "Registration without devicetoken",
			Method:         "GET",
			URL:            "/register?key=" + key,
			Body:           "",
			IsJson:         false,
			WantStatusCode: 400,
		},
	})
}

func TestPushTitleAndBody(t *testing.T) {
	// Correct push
	Endpoint(t, []APITestCase{
		{
			Name:           "GET push body",
			Method:         "GET",
			URL:            "/" + key + "/body",
			Body:           "",
			IsJson:         false,
			WantStatusCode: 200,
		},
		{
			Name:           "GET push title body",
			Method:         "GET",
			URL:            "/" + key + "/title/body",
			Body:           "",
			IsJson:         false,
			WantStatusCode: 200,
		},
		{
			Name:           "GET push title subtitle body",
			Method:         "GET",
			URL:            "/" + key + "/title/subtitle/body",
			Body:           "",
			IsJson:         false,
			WantStatusCode: 200,
		},
		{
			Name:           "POST push body",
			Method:         "POST",
			URL:            "/" + key,
			Body:           "body=body",
			IsJson:         false,
			WantStatusCode: 200,
		},
		{
			Name:           "POST push title body",
			Method:         "POST",
			URL:            "/" + key,
			Body:           "title=title&body=body",
			IsJson:         false,
			WantStatusCode: 200,
		},
		{
			Name:           "POST push title subtitle body",
			Method:         "POST",
			URL:            "/" + key,
			Body:           "title=title&subtitle=subtitle&body=body",
			IsJson:         false,
			WantStatusCode: 200,
		},
		{
			Name:           "GET title subtitle body URL parameters",
			Method:         "GET",
			URL:            "/" + key + "?title=title&subtitle=subtitle&body=body",
			Body:           "",
			IsJson:         false,
			WantStatusCode: 200,
		},
		{
			Name:           "POST title subtitle body POST parameters",
			Method:         "GET",
			URL:            "/" + key,
			Body:           "title=title&subtitle=subtitle&body=body",
			IsJson:         false,
			WantStatusCode: 200,
		},
		{
			Name:           "POST title subtitle body JSON parameters",
			Method:         "POST",
			URL:            "/" + key,
			Body:           "{\"title\":\"title\",\"subtitle\":\"subtitle\",\"body\":\"body\"}",
			IsJson:         true,
			WantStatusCode: 200,
		},
		{
			Name:           "POST V2 title subtitle body",
			Method:         "POST",
			URL:            "/push",
			Body:           "device_key=" + key + "&title=title&subtitle=subtitle&body=body",
			IsJson:         false,
			WantStatusCode: 200,
		},
		{
			Name:           "POST title subtitle body JSON parameters V2",
			Method:         "POST",
			URL:            "/push",
			Body:           "{\"title\":\"title\",\"subtitle\":\"subtitle\",\"body\":\"body\",\"device_key\":\"" + key + "\"}",
			IsJson:         true,
			WantStatusCode: 200,
		},
	})

	// Incorrect push
	Endpoint(t, []APITestCase{
		{
			Name:           "GET push without key",
			Method:         "GET",
			URL:            "/body",
			Body:           "",
			IsJson:         false,
			WantStatusCode: 400,
		},
		{
			Name:           "POST push without key",
			Method:         "POST",
			URL:            "/push",
			Body:           "title=title&subtitle=subtitle&body=body",
			IsJson:         false,
			WantStatusCode: 400,
		},
		{
			Name:           "POST JSON push without key",
			Method:         "POST",
			URL:            "/push",
			Body:           "body=body",
			IsJson:         true,
			WantStatusCode: 400,
		},
		{
			Name:           "GET push with too many parameters",
			Method:         "GET",
			URL:            "/" + key + "/title/subtitle/body/extra",
			Body:           "",
			IsJson:         false,
			WantStatusCode: 404,
		},
	})
}

func TestCiphertext(t *testing.T) {
	Endpoint(t, []APITestCase{
		{
			Name:           "Send encrypted push",
			Method:         "GET",
			URL:            "/" + key + "/body?ciphertext=text&iv=01234567890123456",
			Body:           "",
			IsJson:         false,
			WantStatusCode: 200,
		},
		{
			Name:           "Send encrypted push, omit body",
			Method:         "GET",
			URL:            "/" + key + "?ciphertext=text",
			Body:           "",
			IsJson:         false,
			WantStatusCode: 200,
		},
		{
			Name:           "POST send encrypted push",
			Method:         "POST",
			URL:            "/" + key,
			Body:           "ciphertext=text",
			IsJson:         false,
			WantStatusCode: 200,
		},
		{
			Name:           "POST send encrypted push V2",
			Method:         "POST",
			URL:            "/push",
			Body:           "{\"device_key\":\"" + key + "\",\"ciphertext\":\"text\"}",
			IsJson:         true,
			WantStatusCode: 200,
		},
	})
}

func TestBatchPush(t *testing.T) {
	Endpoint(t, []APITestCase{
		{
			Name:           "Batch Push",
			Method:         "POST",
			URL:            "/" + key,
			Body:           "{\"title\":\"title\",\"subtitle\":\"subtitle\",\"body\":\"body\",\"device_keys\":[\"" + key + "\",\"" + key + "\",\"" + key + "\"]}",
			IsJson:         true,
			WantStatusCode: 200,
		},
		{
			Name:           "Batch Push",
			Method:         "POST",
			URL:            "/push",
			Body:           "{\"title\":\"title\",\"subtitle\":\"subtitle\",\"body\":\"body\",\"device_keys\":[\"" + key + "\",\"" + key + "\",\"" + key + "\"]}",
			IsJson:         true,
			WantStatusCode: 200,
		},
		{
			Name:           "Batch Push",
			Method:         "POST",
			URL:            "/push",
			Body:           "{\"title\":\"title\",\"subtitle\":\"subtitle\",\"body\":\"body\",\"device_keys\": \"" + key + "," + key + "," + key + "\"}",
			IsJson:         true,
			WantStatusCode: 200,
		},
	})
}

func TestAndroidPushCanBePolled(t *testing.T) {
	androidHub = newAndroidDeliveryHub()

	registerBody := `{"device_key":"` + key + `","device_token":"` + deviceToken + `"}`
	req, _ := http.NewRequest("POST", "/register", bytes.NewBufferString(registerBody))
	req.Host = "example.com"
	req.Header.Set("Content-Type", "application/json")
	res, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("register: want 200, got %d, res: %s", res.StatusCode, string(body))
	}

	var registerResp CommonResp
	if err := jsoniter.NewDecoder(res.Body).Decode(&registerResp); err != nil {
		t.Fatal(err)
	}
	data, ok := registerResp.Data.(map[string]interface{})
	if !ok {
		t.Fatalf("register: unexpected data shape %#v", registerResp.Data)
	}
	deviceKey, ok := data["device_key"].(string)
	if !ok || deviceKey == "" {
		t.Fatalf("register: missing device_key in %#v", data)
	}

	pushBody := `{"device_key":"` + deviceKey + `","title":"Android title","subtitle":"Android subtitle","body":"Android body","group":"android","url":"https://day.app","sound":"bell","badge":3}`
	req, _ = http.NewRequest("POST", "/push", bytes.NewBufferString(pushBody))
	req.Host = "example.com"
	req.Header.Set("Content-Type", "application/json")
	res, err = app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("push: want 200, got %d, res: %s", res.StatusCode, string(body))
	}

	req, _ = http.NewRequest("GET", "/android/poll/"+deviceKey+"?timeout=1", nil)
	req.Host = "example.com"
	res, err = app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("poll: want 200, got %d, res: %s", res.StatusCode, string(body))
	}

	var pollResp CommonResp
	if err := jsoniter.NewDecoder(res.Body).Decode(&pollResp); err != nil {
		t.Fatal(err)
	}
	payload, ok := pollResp.Data.(map[string]interface{})
	if !ok {
		t.Fatalf("poll: unexpected data shape %#v", pollResp.Data)
	}
	for field, want := range map[string]string{
		"title":    "Android title",
		"subtitle": "Android subtitle",
		"body":     "Android body",
		"group":    "android",
		"url":      "https://day.app",
		"sound":    "bell.caf",
	} {
		if got := payload[field]; got != want {
			t.Fatalf("poll: field %s want %q, got %#v in %#v", field, want, got, payload)
		}
	}
	if got := payload["badge"]; got != "3" {
		t.Fatalf("poll: badge want string 3, got %#v in %#v", got, payload)
	}
}

func TestAndroidBatchPushCanBePolledByEachDevice(t *testing.T) {
	androidHub = newAndroidDeliveryHub()

	previousDB := db
	db = testDeviceDatabase{
		"AndroidBatchKeyOne": "android:batch-device-1",
		"AndroidBatchKeyTwo": "android:batch-device-2",
	}
	defer func() { db = previousDB }()

	deviceKeyOne := "AndroidBatchKeyOne"
	deviceKeyTwo := "AndroidBatchKeyTwo"

	pushBody := `{"device_keys":["` + deviceKeyOne + `","` + deviceKeyTwo + `"],"title":"Android batch","body":"Batch body","group":"batch"}`
	req, _ := http.NewRequest("POST", "/push", bytes.NewBufferString(pushBody))
	req.Host = "example.com"
	req.Header.Set("Content-Type", "application/json")
	res, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("batch push: want 200, got %d, res: %s", res.StatusCode, string(body))
	}

	var batchResp CommonResp
	if err := jsoniter.NewDecoder(res.Body).Decode(&batchResp); err != nil {
		t.Fatal(err)
	}
	results, ok := batchResp.Data.([]interface{})
	if !ok || len(results) != 2 {
		t.Fatalf("batch push: unexpected data shape %#v", batchResp.Data)
	}
	for index, result := range results {
		item, ok := result.(map[string]interface{})
		if !ok {
			t.Fatalf("batch push: result %d has unexpected shape %#v", index, result)
		}
		if got := item["code"]; got != float64(200) {
			t.Fatalf("batch push: result %d code want 200, got %#v in %#v", index, got, item)
		}
	}

	for _, deviceKey := range []string{deviceKeyOne, deviceKeyTwo} {
		req, _ = http.NewRequest("GET", "/android/poll/"+deviceKey+"?timeout=1", nil)
		req.Host = "example.com"
		res, err = app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			body, _ := io.ReadAll(res.Body)
			t.Fatalf("poll %s: want 200, got %d, res: %s", deviceKey, res.StatusCode, string(body))
		}

		var pollResp CommonResp
		if err := jsoniter.NewDecoder(res.Body).Decode(&pollResp); err != nil {
			t.Fatal(err)
		}
		payload, ok := pollResp.Data.(map[string]interface{})
		if !ok {
			t.Fatalf("poll %s: unexpected data shape %#v", deviceKey, pollResp.Data)
		}
		for field, want := range map[string]string{
			"title": "Android batch",
			"body":  "Batch body",
			"group": "batch",
		} {
			if got := payload[field]; got != want {
				t.Fatalf("poll %s: field %s want %q, got %#v in %#v", deviceKey, field, want, got, payload)
			}
		}
	}
}

type testDeviceDatabase map[string]string

func (d testDeviceDatabase) CountAll() (int, error) {
	return len(d), nil
}

func (d testDeviceDatabase) DeviceTokenByKey(key string) (string, error) {
	token, ok := d[key]
	if !ok || token == "" {
		return "", fmt.Errorf("key not found")
	}
	return token, nil
}

func (d testDeviceDatabase) SaveDeviceTokenByKey(key, token string) (string, error) {
	d[key] = token
	return key, nil
}

func (d testDeviceDatabase) DeleteDeviceByKey(key string) error {
	delete(d, key)
	return nil
}

func (d testDeviceDatabase) Close() error {
	return nil
}

type APITestCase struct {
	Name           string
	Method         string
	URL            string
	Body           string
	IsJson         bool
	WantStatusCode int
}

func NewServer() *fiber.App {
	fiberApp := fiber.New(fiber.Config{
		JSONEncoder: jsoniter.Marshal,
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			code := fiber.StatusInternalServerError
			if e, ok := err.(*fiber.Error); ok {
				code = e.Code
			}
			return c.Status(code).JSON(CommonResp{
				Code:      code,
				Message:   err.Error(),
				Timestamp: time.Now().Unix(),
			})
		},
	})

	routerSetup(fiberApp)
	return fiberApp
}

func Endpoint(t *testing.T, tc []APITestCase) {
	for _, tt := range tc {
		t.Run(tt.Name, func(t *testing.T) {
			req, _ := http.NewRequest(tt.Method, tt.URL, bytes.NewBufferString(tt.Body))
			req.Host = "example.com"
			if tt.IsJson {
				req.Header.Set("Content-Type", "application/json")
			} else {
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			}
			res, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()

			if res.StatusCode != tt.WantStatusCode {
				body, _ := io.ReadAll(io.Reader(res.Body))
				t.Fatalf("want %d, got %d, res: %s", tt.WantStatusCode, res.StatusCode, string(body))
			}
		})
		// Prevent rate limiting by sending requests too quickly
		time.Sleep(100 * time.Millisecond)
	}
}
