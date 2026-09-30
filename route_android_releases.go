package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gofiber/fiber/v2"
)

const androidReleasePackage = "day.bark.android"
const androidReleaseSigner = "60408b509647dc7689bca6435a3eef93d475d868232fe88d848e853c03ffe896"
const androidReleaseHost = "bark.atrl.me"
const androidReleaseMaxBytes = 256 * 1024 * 1024
const androidReleaseManifestMaxBytes = 16 * 1024

var androidReleaseFilename = regexp.MustCompile(`^bark-android-[A-Za-z0-9][A-Za-z0-9._+-]{0,100}\.apk$`)
var androidReleaseVersion = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
var androidReleaseDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

type androidReleaseManifest struct {
	SchemaVersion            int    `json:"schema_version"`
	PackageName              string `json:"package_name"`
	VersionCode              int64  `json:"version_code"`
	VersionName              string `json:"version_name"`
	MinSDK                   int    `json:"min_sdk"`
	APKURL                   string `json:"apk_url"`
	SHA256                   string `json:"sha256"`
	SizeBytes                int64  `json:"size_bytes"`
	SigningCertificateSHA256 string `json:"signing_certificate_sha256"`
	ReleaseNotes             string `json:"release_notes"`
	PublishedAt              string `json:"published_at"`
}

func init() {
	registerRouteWithWeight("android_releases", 90, func(router fiber.Router) {
		router.Get("/android/releases/stable.json", routeAndroidReleaseManifest)
		router.Get("/android/releases/:filename", routeAndroidReleaseAPK)
		// Reserve the entire namespace, including encoded/traversal/non-GET paths.
		router.All("/android/releases/*", func(c *fiber.Ctx) error { return androidReleaseUnavailable(c) })
	})
}

func androidReleaseUnavailable(c *fiber.Ctx) error {
	c.Set("Cache-Control", "no-store")
	return c.Status(404).JSON(fiber.Map{"error": "Android release is unavailable"})
}

func openAndroidReleaseRoot() (*os.Root, error) {
	directory := os.Getenv("BARK_ANDROID_RELEASES_DIR")
	if directory == "" {
		return nil, fmt.Errorf("release directory is not configured")
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("invalid release directory")
	}
	return os.OpenRoot(directory)
}

func openAndroidReleaseFile(root *os.Root, filename string) (*os.File, error) {
	if path.Base(filename) != filename || strings.ContainsAny(filename, "/\\\x00") {
		return nil, fmt.Errorf("invalid release filename")
	}
	info, err := root.Lstat(filename)
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("release is not a regular file")
	}
	file, err := root.Open(filename)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		file.Close()
		return nil, fmt.Errorf("release file changed during open")
	}
	return file, nil
}

func validateAndroidRelease(manifest androidReleaseManifest) (string, error) {
	if manifest.SchemaVersion != 1 || manifest.PackageName != androidReleasePackage || manifest.VersionCode < 1 || manifest.VersionCode > 2100000000 || !androidReleaseVersion.MatchString(manifest.VersionName) || manifest.MinSDK < 26 || manifest.MinSDK > 1000 {
		return "", fmt.Errorf("invalid release package/version metadata")
	}
	if !androidReleaseDigest.MatchString(manifest.SHA256) || manifest.SigningCertificateSHA256 != androidReleaseSigner || manifest.SizeBytes < 1 || manifest.SizeBytes > androidReleaseMaxBytes {
		return "", fmt.Errorf("invalid release artifact identity")
	}
	if !utf8.ValidString(manifest.ReleaseNotes) || len(manifest.ReleaseNotes) > 4096 {
		return "", fmt.Errorf("invalid release notes")
	}
	if _, err := time.Parse(time.RFC3339, manifest.PublishedAt); err != nil {
		return "", fmt.Errorf("invalid release publication time")
	}
	parsed, err := url.Parse(manifest.APKURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host != androidReleaseHost || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" {
		return "", fmt.Errorf("invalid release download origin")
	}
	filename := path.Base(parsed.Path)
	if !androidReleaseFilename.MatchString(filename) || strings.Contains(filename, "..") || parsed.Path != "/android/releases/"+filename {
		return "", fmt.Errorf("invalid release download path")
	}
	return filename, nil
}

func readAndroidReleaseManifest(root *os.Root, name string) (androidReleaseManifest, error) {
	var manifest androidReleaseManifest
	file, err := openAndroidReleaseFile(root, name)
	if err != nil {
		return manifest, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() > androidReleaseManifestMaxBytes {
		return manifest, fmt.Errorf("release manifest is too large")
	}
	decoder := json.NewDecoder(io.LimitReader(file, androidReleaseManifestMaxBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return manifest, err
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); err != io.EOF {
		return manifest, fmt.Errorf("trailing data in release manifest")
	}
	_, err = validateAndroidRelease(manifest)
	return manifest, err
}

func openVerifiedAndroidAPK(root *os.Root, filename string, manifest androidReleaseManifest) (*os.File, error) {
	expected, err := validateAndroidRelease(manifest)
	if err != nil || filename != expected {
		return nil, fmt.Errorf("release filename does not match manifest")
	}
	file, err := openAndroidReleaseFile(root, filename)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || info.Size() != manifest.SizeBytes {
		file.Close()
		return nil, fmt.Errorf("release size mismatch")
	}
	digest := sha256.New()
	count, err := io.Copy(digest, io.LimitReader(file, manifest.SizeBytes+1))
	if err != nil || count != manifest.SizeBytes || hex.EncodeToString(digest.Sum(nil)) != manifest.SHA256 {
		file.Close()
		return nil, fmt.Errorf("release checksum mismatch")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func routeAndroidReleaseManifest(c *fiber.Ctx) error {
	root, err := openAndroidReleaseRoot()
	if err != nil {
		return androidReleaseUnavailable(c)
	}
	defer root.Close()
	manifest, err := readAndroidReleaseManifest(root, "stable.json")
	if err != nil {
		return androidReleaseUnavailable(c)
	}
	filename, _ := validateAndroidRelease(manifest)
	immutable, err := readAndroidReleaseManifest(root, filename+".json")
	if err != nil || immutable != manifest {
		return androidReleaseUnavailable(c)
	}
	file, err := openVerifiedAndroidAPK(root, filename, manifest)
	if err != nil {
		return androidReleaseUnavailable(c)
	}
	file.Close()
	c.Set("Cache-Control", "no-store, max-age=0")
	c.Set("X-Content-Type-Options", "nosniff")
	return c.JSON(manifest)
}

func routeAndroidReleaseAPK(c *fiber.Ctx) error {
	filename := c.Params("filename")
	if !androidReleaseFilename.MatchString(filename) || strings.Contains(filename, "..") {
		return androidReleaseUnavailable(c)
	}
	root, err := openAndroidReleaseRoot()
	if err != nil {
		return androidReleaseUnavailable(c)
	}
	defer root.Close()
	manifest, err := readAndroidReleaseManifest(root, filename+".json")
	if err != nil {
		return androidReleaseUnavailable(c)
	}
	file, err := openVerifiedAndroidAPK(root, filename, manifest)
	if err != nil {
		return androidReleaseUnavailable(c)
	}
	etag := `"` + manifest.SHA256 + `"`
	c.Set("Content-Type", "application/vnd.android.package-archive")
	c.Set("X-Content-Type-Options", "nosniff")
	c.Set("Cache-Control", "public, max-age=31536000, immutable")
	c.Set("ETag", etag)
	c.Set("Accept-Ranges", "bytes")
	c.Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	if c.Get("If-None-Match") == etag {
		file.Close()
		return c.SendStatus(304)
	}
	length := int(manifest.SizeBytes)
	if c.Get("Range") != "" && (c.Get("If-Range") == "" || c.Get("If-Range") == etag) {
		ranges, err := c.Range(length)
		if err != nil || ranges.Type != "bytes" || len(ranges.Ranges) != 1 {
			file.Close()
			c.Set("Cache-Control", "no-store")
			c.Set("Content-Range", fmt.Sprintf("bytes */%d", length))
			return c.SendStatus(416)
		}
		selected := ranges.Ranges[0]
		if _, err := file.Seek(int64(selected.Start), io.SeekStart); err != nil {
			file.Close()
			return androidReleaseUnavailable(c)
		}
		c.Status(206)
		c.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", selected.Start, selected.End, length))
		length = selected.End - selected.Start + 1
	}
	if c.Method() == fiber.MethodHead {
		file.Close()
		c.Set("Content-Length", fmt.Sprint(length))
		return nil
	}
	// fasthttp closes the reader after streaming; keep this validated descriptor
	// instead of reopening a pathname after the symlink/hash checks.
	return c.SendStream(&androidReleaseStream{Reader: io.LimitReader(file, int64(length)), Closer: file}, length)
}

type androidReleaseStream struct {
	io.Reader
	io.Closer
}
