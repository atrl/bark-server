import contextlib
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import types
import unittest
from unittest import mock

SPEC = importlib.util.spec_from_file_location("android_release", Path(__file__).with_name("prepare-android-release.py"))
release = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(release)


class ReleasePublicationTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.destination = self.root / "published"

    def bundle(self, code=4, version="0.2.1", payload=b"signed-APK-fixture", suffix=""):
        directory = self.root / ("bundle-" + str(code) + suffix)
        directory.mkdir()
        filename = "bark-android-" + version + ".apk"
        (directory / filename).write_bytes(payload)
        digest, size = release.sha256_file(directory / filename)
        manifest = {
            "schema_version": 1, "package_name": release.PACKAGE_NAME,
            "version_code": code, "version_name": version, "min_sdk": 26,
            "apk_url": release.PUBLIC_BASE_URL + "/" + filename,
            "sha256": digest, "size_bytes": size,
            "signing_certificate_sha256": release.SIGNING_CERTIFICATE_SHA256,
            "release_notes": "Notes", "published_at": "2026-09-30T12:00:00Z",
        }
        self.write_manifests(directory, filename, manifest)
        return directory, filename, manifest

    def write_manifests(self, directory, filename, manifest):
        raw = json.dumps(manifest).encode()
        (directory / "stable.json").write_bytes(raw)
        (directory / (filename + ".json")).write_bytes(raw)

    def publish(self, directory):
        with contextlib.redirect_stdout(io.StringIO()):
            release.publish(types.SimpleNamespace(bundle_dir=directory, release_dir=self.destination))

    def test_atomic_publication_retains_previous_and_is_idempotent(self):
        first, filename, manifest = self.bundle()
        self.publish(first)
        original = (self.destination / "stable.json").read_bytes()
        self.publish(first)
        self.assertEqual(original, (self.destination / "stable.json").read_bytes())
        second, newer_filename, newer = self.bundle(5, "0.2.2", b"new signed fixture")
        self.publish(second)
        self.assertEqual(newer, release.load_manifest(self.destination / "stable.json"))
        self.assertEqual(manifest, release.load_manifest(self.destination / (filename + ".json")))
        self.assertTrue((self.destination / newer_filename).is_file())
        self.assertTrue((self.destination / filename).is_file())
        with self.assertRaisesRegex(ValueError, "lower version"):
            self.publish(first)
        self.assertEqual(newer, release.load_manifest(self.destination / "stable.json"))

    def test_same_version_code_different_artifact_or_metadata_is_rejected(self):
        first, _, original = self.bundle()
        self.publish(first)
        changed, _, _ = self.bundle(payload=b"different bytes", suffix="changed")
        with self.assertRaisesRegex(ValueError, "Same version code"):
            self.publish(changed)
        self.assertEqual(original, release.load_manifest(self.destination / "stable.json"))

    def test_invalid_metadata_or_apk_does_not_change_stable(self):
        first, _, original = self.bundle()
        self.publish(first)
        for index, (field, value) in enumerate([
            ("sha256", "0" * 64), ("size_bytes", 1),
            ("package_name", "wrong.package"), ("signing_certificate_sha256", "0" * 64),
            ("version_code", 0), ("min_sdk", 0),
            ("apk_url", "https://evil.example/app.apk"),
            ("apk_url", release.PUBLIC_BASE_URL + "/../secret.apk"),
        ]):
            with self.subTest(field=field, value=value):
                candidate, filename, metadata = self.bundle(5, "0.2.2", suffix=str(index))
                metadata[field] = value
                self.write_manifests(candidate, filename, metadata)
                with self.assertRaises(ValueError):
                    self.publish(candidate)
                self.assertEqual(original, release.load_manifest(self.destination / "stable.json"))

    def test_partial_failure_never_switches_stable_to_missing_sidecar(self):
        first, _, original = self.bundle()
        self.publish(first)
        second, filename, newer = self.bundle(5, "0.2.2", b"new bytes")
        actual_write = release.write_atomic
        def fail_sidecar(path, **kwargs):
            if str(path).endswith(".apk.json"):
                raise OSError("simulated interruption")
            return actual_write(path, **kwargs)
        with mock.patch.object(release, "write_atomic", side_effect=fail_sidecar):
            with self.assertRaises(OSError):
                self.publish(second)
        self.assertEqual(original, release.load_manifest(self.destination / "stable.json"))
        self.assertFalse((self.destination / (filename + ".json")).exists())
        self.publish(second)
        self.assertEqual(newer, release.load_manifest(self.destination / "stable.json"))

    def test_symlinks_and_immutable_overwrites_are_rejected(self):
        candidate, filename, _ = self.bundle()
        external = self.root / "external.apk"
        external.write_bytes((candidate / filename).read_bytes())
        (candidate / filename).unlink()
        (candidate / filename).symlink_to(external)
        with self.assertRaises(ValueError):
            self.publish(candidate)
        candidate2, filename2, _ = self.bundle(5, "0.2.2")
        self.destination.mkdir()
        (self.destination / filename2).write_bytes(b"existing-different-immutable-file")
        with self.assertRaisesRegex(ValueError, "immutable"):
            self.publish(candidate2)
        self.assertFalse((self.destination / "stable.json").exists())
        self.assertEqual(b"existing-different-immutable-file", (self.destination / filename2).read_bytes())

    def test_prepare_uses_actual_sdk_metadata_and_signature(self):
        apk = self.root / "input.apk"
        apk.write_bytes(b"fixture APK")
        results = [
            types.SimpleNamespace(stdout="package: name='day.bark.android' versionCode='4' versionName='0.2.1'\nsdkVersion:'26'\n"),
            types.SimpleNamespace(stdout="Signer #1 certificate SHA-256 digest: " + release.SIGNING_CERTIFICATE_SHA256 + "\n"),
        ]
        args = types.SimpleNamespace(apk=apk, output_dir=self.root / "prepared", release_notes_file=None,
                                     aapt="aapt", apksigner="apksigner")
        with mock.patch.object(release, "sdk_tool", side_effect=lambda name, explicit: explicit):
            with mock.patch.object(release.subprocess, "run", side_effect=results) as invoked:
                with contextlib.redirect_stdout(io.StringIO()):
                    release.prepare(args)
        self.assertEqual(2, invoked.call_count)
        manifest = release.load_manifest(self.root / "prepared/stable.json")
        self.assertEqual(4, manifest["version_code"])
        self.assertEqual(26, manifest["min_sdk"])
        self.assertEqual(release.sha256_file(apk)[0], manifest["sha256"])

    def test_prepare_rejects_wrong_actual_certificate(self):
        apk = self.root / "input.apk"
        apk.write_bytes(b"fixture APK")
        results = [types.SimpleNamespace(stdout="package: name='day.bark.android' versionCode='4' versionName='0.2.1'\nsdkVersion:'26'\n"),
                   types.SimpleNamespace(stdout="Signer #1 certificate SHA-256 digest: " + "0" * 64 + "\n")]
        with mock.patch.object(release.subprocess, "run", side_effect=results):
            with self.assertRaisesRegex(ValueError, "Actual APK signer"):
                release.apk_metadata(apk, "aapt", "apksigner")


if __name__ == "__main__":
    unittest.main()
