package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
)

func releaseFixture(t *testing.T, dir, version string, code int64, payload []byte) androidReleaseManifest {
	t.Helper()
	digest := sha256.Sum256(payload)
	manifest := androidReleaseManifest{1, androidReleasePackage, code, version, 26,
		"https://bark.atrl.me/android/releases/bark-android-" + version + ".apk", hex.EncodeToString(digest[:]), int64(len(payload)), androidReleaseSigner, "Release notes", "2026-09-30T12:00:00Z"}
	filename, _ := validateAndroidRelease(manifest)
	raw, _ := json.Marshal(manifest)
	for name, body := range map[string][]byte{filename: payload, filename + ".json": raw, "stable.json": raw} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0644); err != nil {
			t.Fatal(err)
		}
	}
	return manifest
}

func requestRelease(t *testing.T, application *fiber.App, method, target string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	request, _ := http.NewRequest(method, "http://example.com"+target, nil)
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := application.Test(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return response, body
}

func TestAndroidReleaseManifestAndRangeDownload(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("BARK_ANDROID_RELEASES_DIR", directory)
	expected := releaseFixture(t, directory, "0.2.1", 4, []byte("0123456789"))
	response, body := requestRelease(t, app, "GET", "/android/releases/stable.json", nil)
	if response.StatusCode != 200 || !strings.Contains(response.Header.Get("Cache-Control"), "no-store") {
		t.Fatalf("manifest status/cache: %d %s", response.StatusCode, body)
	}
	var manifest androidReleaseManifest
	if err := json.Unmarshal(body, &manifest); err != nil || manifest != expected {
		t.Fatalf("native schema mismatch: %s %v", body, err)
	}
	if bytes.Contains(body, []byte(`"data"`)) {
		t.Fatal("manifest must not use CommonResp")
	}
	response, body = requestRelease(t, app, "GET", "/android/releases/bark-android-0.2.1.apk", map[string]string{"Range": "bytes=2-5"})
	if response.StatusCode != 206 || string(body) != "2345" || response.Header.Get("Content-Range") != "bytes 2-5/10" {
		t.Fatalf("range: %d %q %#v", response.StatusCode, body, response.Header)
	}
	if !strings.Contains(response.Header.Get("Cache-Control"), "immutable") {
		t.Fatal("immutable APK not cached")
	}
	response, body = requestRelease(t, app, "HEAD", "/android/releases/bark-android-0.2.1.apk", nil)
	if response.StatusCode != 200 || len(body) != 0 || response.Header.Get("Content-Length") != "10" {
		t.Fatalf("HEAD: %d %q", response.StatusCode, body)
	}
	response, _ = requestRelease(t, app, "GET", "/android/releases/bark-android-0.2.1.apk", map[string]string{"Range": "bytes=100-200"})
	if response.StatusCode != 416 {
		t.Fatal("invalid range accepted")
	}
	response, _ = requestRelease(t, app, "GET", "/android/releases/bark-android-0.2.1.apk", map[string]string{"If-None-Match": `"` + expected.SHA256 + `"`})
	if response.StatusCode != 304 {
		t.Fatal("immutable ETag not honored")
	}
	releaseFixture(t, directory, "0.2.2", 5, []byte("new-version"))
	response, body = requestRelease(t, app, "GET", "/android/releases/bark-android-0.2.1.apk", nil)
	if response.StatusCode != 200 || string(body) != "0123456789" {
		t.Fatal("switching stable broke an already announced immutable URL")
	}
}

func TestAndroidReleaseRejectsUnsafeOrUnlistedFiles(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("BARK_ANDROID_RELEASES_DIR", directory)
	releaseFixture(t, directory, "0.2.1", 4, []byte("signed-apk-fixture"))
	os.WriteFile(filepath.Join(directory, "secret.json"), []byte("secret"), 0600)
	os.WriteFile(filepath.Join(directory, "bark-android-unlisted.apk"), []byte("unlisted"), 0600)
	for _, target := range []string{
		"/android/releases/secret.json", "/android/releases/bark-android-0.2.1.apk.json",
		"/android/releases/bark-android-unlisted.apk", "/android/releases/../secret.json",
		"/android/releases/%2e%2e/secret.json", "/android/releases/bark-android-..apk",
		"/android/releases/%2fsecret.apk", "/android/releases/stable.json/secret.apk",
	} {
		response, body := requestRelease(t, app, "GET", target, nil)
		if response.StatusCode != 404 || bytes.Contains(body, []byte(`"secret"`)) {
			t.Fatalf("unsafe endpoint exposed: %s %d %s", target, response.StatusCode, body)
		}
	}
	filename := filepath.Join(directory, "bark-android-0.2.1.apk")
	os.Remove(filename)
	target := filepath.Join(t.TempDir(), "external.apk")
	os.WriteFile(target, []byte("signed-apk-fixture"), 0600)
	if err := os.Symlink(target, filename); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"stable.json", "bark-android-0.2.1.apk"} {
		response, _ := requestRelease(t, app, "GET", "/android/releases/"+path, nil)
		if response.StatusCode != 404 {
			t.Fatal("symlink APK was exposed")
		}
	}
}

func TestAndroidReleaseInvalidMetadataOrPartialUploadUnavailable(t *testing.T) {
	mutations := []func(*androidReleaseManifest){
		func(m *androidReleaseManifest) { m.SchemaVersion = 2 },
		func(m *androidReleaseManifest) { m.PackageName = "other.package" },
		func(m *androidReleaseManifest) { m.VersionCode = 0 },
		func(m *androidReleaseManifest) { m.SHA256 = strings.Repeat("0", 64) },
		func(m *androidReleaseManifest) { m.SizeBytes++ },
		func(m *androidReleaseManifest) { m.SigningCertificateSHA256 = strings.Repeat("0", 64) },
		func(m *androidReleaseManifest) {
			m.APKURL = "https://evil.example/android/releases/bark-android-0.2.1.apk"
		},
		func(m *androidReleaseManifest) { m.APKURL += "?token=secret" },
		func(m *androidReleaseManifest) { m.PublishedAt = "yesterday" },
	}
	for _, mutate := range mutations {
		directory := t.TempDir()
		t.Setenv("BARK_ANDROID_RELEASES_DIR", directory)
		manifest := releaseFixture(t, directory, "0.2.1", 4, []byte("apk"))
		mutate(&manifest)
		raw, _ := json.Marshal(manifest)
		os.WriteFile(filepath.Join(directory, "stable.json"), raw, 0644)
		os.WriteFile(filepath.Join(directory, "bark-android-0.2.1.apk.json"), raw, 0644)
		response, _ := requestRelease(t, app, "GET", "/android/releases/stable.json", nil)
		if response.StatusCode != 404 {
			t.Fatalf("invalid manifest accepted: %#v", manifest)
		}
	}
	directory := t.TempDir()
	t.Setenv("BARK_ANDROID_RELEASES_DIR", directory)
	releaseFixture(t, directory, "0.2.1", 4, []byte("complete APK"))
	os.WriteFile(filepath.Join(directory, "bark-android-0.2.1.apk"), []byte("partial"), 0644)
	response, _ := requestRelease(t, app, "GET", "/android/releases/stable.json", nil)
	if response.StatusCode != 404 {
		t.Fatal("manifest exposed a partial or corrupt APK")
	}
	t.Setenv("BARK_ANDROID_RELEASES_DIR", "")
	response, _ = requestRelease(t, app, "GET", "/android/releases/stable.json", nil)
	if response.StatusCode != 404 {
		t.Fatal("unconfigured releases claimed available")
	}
	response, _ = requestRelease(t, app, "GET", "/ping", nil)
	if response.StatusCode != 200 {
		t.Fatal("unavailable releases affected existing routes")
	}
}

func TestAndroidReleaseDownloadIsPublicWhenPushUsesBasicAuth(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("BARK_ANDROID_RELEASES_DIR", directory)
	releaseFixture(t, directory, "0.2.1", 4, []byte("apk"))
	application := fiber.New()
	routerAuth("user", "password", application, "")
	application.Get("/android/releases/stable.json", routeAndroidReleaseManifest)
	application.Post("/push", func(c *fiber.Ctx) error { return c.SendStatus(200) })
	response, _ := requestRelease(t, application, "GET", "/android/releases/stable.json", nil)
	if response.StatusCode != 200 {
		t.Fatal("public updater requires unrelated Bark credentials")
	}
	response, _ = requestRelease(t, application, "POST", "/push", nil)
	if response.StatusCode != 418 {
		t.Fatal("release exemption weakened push authentication")
	}
}
