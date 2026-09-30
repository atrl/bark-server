import hashlib
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import unittest
from urllib.request import Request

spec = importlib.util.spec_from_file_location("release_sync", Path(__file__).with_name("sync-android-release.py"))
sync = importlib.util.module_from_spec(spec)
spec.loader.exec_module(sync)


class Response(io.BytesIO):
    def __init__(self, content, headers=None):
        super().__init__(content)
        self.status = 200
        self.headers = headers or {}

    def geturl(self):
        return "https://release-assets.githubusercontent.com/asset"


class Opener:
    def __init__(self, response):
        self.response = response

    def open(self, request, timeout):
        return self.response


def manifest(payload=b"apk"):
    return {
        "schema_version": 1, "package_name": "day.bark.android", "version_code": 4,
        "version_name": "0.2.1", "min_sdk": 26,
        "apk_url": "https://bark.atrl.me/android/releases/bark-android-0.2.1.apk",
        "sha256": hashlib.sha256(payload).hexdigest(), "size_bytes": len(payload),
        "signing_certificate_sha256": sync.CERTIFICATE,
    }


class ReleaseSyncTest(unittest.TestCase):
    def test_every_redirect_must_stay_on_approved_https_hosts(self):
        handler = sync.SafeRedirects()
        request = Request("https://github.com/atrl/bark-android/releases/download/v0.2.1/stable.json")
        for target in (
            "http://github.com/file", "https://github.com.evil.example/file",
            "https://attacker.example/file", "https://user:pass@github.com/file",
            "https://127.0.0.1/file", "https://github.com:8443/file",
        ):
            with self.subTest(target=target), self.assertRaises(ValueError):
                handler.redirect_request(request, None, 302, "Found", {}, target)
        redirect = handler.redirect_request(request, None, 302, "Found", {},
                                            "https://release-assets.githubusercontent.com/file?token=temporary")
        self.assertEqual(redirect.host, "release-assets.githubusercontent.com")

    def test_metadata_cannot_supply_another_repositories_asset(self):
        release = {"assets": [{"name": "stable.json", "size": 100,
            "browser_download_url": "https://github.com/attacker/repo/releases/download/v1/stable.json"}]}
        with self.assertRaises(ValueError):
            sync.asset_url(release, "stable.json")

    def test_manifest_must_match_version_origin_and_signing_identity(self):
        good = manifest()
        self.assertEqual(sync.manifest_filename(good, "v0.2.1"), "bark-android-0.2.1.apk")
        for field, value in (
            ("package_name", "other.app"), ("signing_certificate_sha256", "0" * 64),
            ("apk_url", "https://attacker.example/evil.apk"),
            ("apk_url", "https://bark.atrl.me/android/releases/../secret.apk"),
            ("apk_url", "https://bark.atrl.me/android/releases/x.apk?redirect=evil"),
            ("size_bytes", True), ("size_bytes", sync.APK_LIMIT + 1),
        ):
            with self.subTest(field=field, value=value), self.assertRaises(ValueError):
                sync.manifest_filename(dict(good, **{field: value}), "v0.2.1")
        with self.assertRaises(ValueError):
            sync.manifest_filename(good, "v0.2.0")

    def test_download_rejects_oversized_and_truncated_data(self):
        cases = [(b"12345", {}, 4), (b"12", {"Content-Length": "3"}, 4),
                 (b"1", {"Content-Length": "9999999"}, 4)]
        for body, headers, limit in cases:
            with self.subTest(headers=headers), tempfile.TemporaryDirectory() as directory:
                with self.assertRaises(ValueError):
                    sync.download("https://github.com/file", Path(directory) / "file", limit,
                                  Opener(Response(body, headers)))

    def test_current_requires_matching_bytes_and_sidecar_not_just_existence(self):
        with tempfile.TemporaryDirectory() as directory:
            directory = Path(directory)
            item = manifest(b"apk")
            name = "bark-android-0.2.1.apk"
            (directory / "stable.json").write_text(json.dumps(item))
            (directory / (name + ".json")).write_text(json.dumps(item))
            (directory / name).write_bytes(b"apk")
            self.assertTrue(sync.current_is_valid(directory, item, name))
            (directory / name).write_bytes(b"bad")
            self.assertFalse(sync.current_is_valid(directory, item, name))
            (directory / name).unlink()
            outside = directory / "outside"
            outside.write_bytes(b"apk")
            (directory / name).symlink_to(outside)
            self.assertFalse(sync.current_is_valid(directory, item, name))


if __name__ == "__main__":
    unittest.main()
