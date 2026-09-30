#!/usr/bin/env python3
"""Verify an Android APK locally, then publish its immutable bundle without an SDK."""

import argparse
import contextlib
import datetime as dt
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess
import sys
import tempfile
from urllib.parse import urlsplit

PACKAGE_NAME = "day.bark.android"
SIGNING_CERTIFICATE_SHA256 = "60408b509647dc7689bca6435a3eef93d475d868232fe88d848e853c03ffe896"
PUBLIC_BASE_URL = "https://bark.atrl.me/android/releases"
MAX_APK_BYTES = 256 * 1024 * 1024
MAX_MANIFEST_BYTES = 16 * 1024
FILENAME_RE = re.compile(r"bark-android-[A-Za-z0-9][A-Za-z0-9._+\-]{0,100}\.apk\Z")
VERSION_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9._+\-]{0,63}\Z")
DIGEST_RE = re.compile(r"[0-9a-f]{64}\Z")
FIELDS = {
    "schema_version", "package_name", "version_code", "version_name", "min_sdk",
    "apk_url", "sha256", "size_bytes", "signing_certificate_sha256",
    "release_notes", "published_at",
}


def require(condition, message):
    if not condition:
        raise ValueError(message)


@contextlib.contextmanager
def regular_file(path):
    path = Path(path)
    before = path.lstat()
    require(stat.S_ISREG(before.st_mode), "Expected a regular file, not a symlink or directory")
    fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
    with os.fdopen(fd, "rb") as handle:
        opened = os.fstat(handle.fileno())
        require(stat.S_ISREG(opened.st_mode) and (before.st_dev, before.st_ino) ==
                (opened.st_dev, opened.st_ino), "File changed while being opened")
        yield handle


def directory(path):
    path = Path(path)
    path.mkdir(parents=True, exist_ok=True)
    require(path.is_dir() and not path.is_symlink(), "Release directory must not be a symlink")
    return path.resolve()


def sha256_file(path):
    digest, count = hashlib.sha256(), 0
    with regular_file(path) as handle:
        require(os.fstat(handle.fileno()).st_size <= MAX_APK_BYTES, "APK exceeds 256 MiB")
        for block in iter(lambda: handle.read(1024 * 1024), b""):
            count += len(block)
            require(count <= MAX_APK_BYTES, "APK exceeds 256 MiB")
            digest.update(block)
    return digest.hexdigest(), count


def reject_duplicate_keys(pairs):
    result = {}
    for name, value in pairs:
        require(name not in result, "Manifest contains duplicate keys")
        result[name] = value
    return result


def validate_manifest(manifest):
    require(isinstance(manifest, dict) and set(manifest) == FIELDS, "Manifest fields do not match schema 1")
    for name in ("schema_version", "version_code", "min_sdk", "size_bytes"):
        require(type(manifest[name]) is int, f"{name} must be an integer")
    for name in FIELDS - {"schema_version", "version_code", "min_sdk", "size_bytes"}:
        require(type(manifest[name]) is str, f"{name} must be a string")
    require(manifest["schema_version"] == 1, "Unsupported manifest schema")
    require(manifest["package_name"] == PACKAGE_NAME, "Unexpected Android package")
    require(1 <= manifest["version_code"] <= 2100000000, "Invalid Android version code")
    require(VERSION_RE.fullmatch(manifest["version_name"]), "Invalid Android version name")
    require(26 <= manifest["min_sdk"] <= 1000, "Invalid Android minimum SDK")
    require(1 <= manifest["size_bytes"] <= MAX_APK_BYTES, "Invalid APK size")
    require(DIGEST_RE.fullmatch(manifest["sha256"]), "Invalid APK SHA-256")
    require(manifest["signing_certificate_sha256"] == SIGNING_CERTIFICATE_SHA256,
            "APK signer does not match the existing application's pinned certificate")
    require(len(manifest["release_notes"].encode("utf-8")) <= 4096, "Release notes exceed 4096 bytes")
    require(re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})",
                         manifest["published_at"]), "Invalid publication timestamp")
    dt.datetime.fromisoformat(manifest["published_at"].replace("Z", "+00:00"))
    parsed = urlsplit(manifest["apk_url"])
    filename = parsed.path.rsplit("/", 1)[-1]
    require(parsed.scheme == "https" and parsed.netloc == "bark.atrl.me" and
            not parsed.query and not parsed.fragment and
            parsed.path == "/android/releases/" + filename and
            FILENAME_RE.fullmatch(filename) and ".." not in filename,
            "APK URL must be a safe filename at the pinned NAS HTTPS origin")
    require(manifest["apk_url"] == PUBLIC_BASE_URL + "/" + filename, "APK URL must not contain escapes")
    return filename


def load_manifest(path):
    with regular_file(path) as handle:
        raw = handle.read(MAX_MANIFEST_BYTES + 1)
    require(len(raw) <= MAX_MANIFEST_BYTES, "Manifest exceeds 16 KiB")
    manifest = json.loads(raw, object_pairs_hook=reject_duplicate_keys)
    validate_manifest(manifest)
    return manifest


def manifest_bytes(manifest):
    validate_manifest(manifest)
    raw = (json.dumps(manifest, ensure_ascii=False, indent=2, sort_keys=True) + "\n").encode("utf-8")
    require(len(raw) <= MAX_MANIFEST_BYTES, "Manifest exceeds 16 KiB")
    return raw


def sync_directory(path):
    fd = os.open(path, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def write_atomic(path, *, data=None, source=None, immutable=False, expected_hash=None, expected_size=None):
    """Write fully, fsync, then link without overwrite or atomically replace stable."""
    path = Path(path)
    require((data is None) != (source is None), "Specify exactly one content source")
    fd, temporary = tempfile.mkstemp(prefix=".release-", dir=path.parent)
    try:
        digest, count = hashlib.sha256(), 0
        with os.fdopen(fd, "wb") as output:
            os.fchmod(output.fileno(), 0o644)
            if source is not None:
                with regular_file(source) as incoming:
                    for block in iter(lambda: incoming.read(1024 * 1024), b""):
                        count += len(block)
                        require(count <= MAX_APK_BYTES, "APK grew beyond the size limit while copying")
                        digest.update(block)
                        output.write(block)
            else:
                count, digest = len(data), hashlib.sha256(data)
                output.write(data)
            if expected_hash is not None:
                require(digest.hexdigest() == expected_hash and count == expected_size,
                        "Source APK changed while copying; previous release retained")
            output.flush()
            os.fsync(output.fileno())
        if immutable:
            try:
                os.link(temporary, path, follow_symlinks=False)
            except FileExistsError:
                existing_hash, existing_size = sha256_file(path)
                require(existing_hash == digest.hexdigest() and existing_size == count,
                        "Refusing to overwrite an immutable release file with different content")
        else:
            # Refuse an existing symlink even though replace itself would not follow it.
            if path.exists() or path.is_symlink():
                with regular_file(path):
                    pass
            os.replace(temporary, path)
        sync_directory(path.parent)
    finally:
        with contextlib.suppress(FileNotFoundError):
            os.unlink(temporary)


def sdk_tool(name, explicit=None):
    if explicit:
        require(Path(explicit).is_file(), f"{name} executable was not found")
        return explicit
    found = shutil.which(name)
    if found:
        return found
    roots = [os.environ.get("ANDROID_HOME"), os.environ.get("ANDROID_SDK_ROOT"),
             str(Path.home() / "Library/Android/sdk")]
    for root in filter(None, roots):
        candidates = list((Path(root) / "build-tools").glob("*/" + name))
        candidates.sort(key=lambda p: tuple(int(n) for n in re.findall(r"\d+", p.parent.name)), reverse=True)
        if candidates:
            return str(candidates[0])
    raise ValueError(f"Set --{name} or ANDROID_HOME to a verified Android SDK")



def verified_signing_certificate(output):
    """Accept one pinned signer, including apksigner's official v3.1 SDK labels.

    ApkSignerTool prints SDK-scoped v3.1/v3.0 certificates instead of numbered
    signers when v3.1 is present. The single-current-signer summary and every
    printed SDK certificate must agree with the existing application pin.
    """
    summaries = [line for line in output.splitlines() if line.startswith("Number of signers:")]
    require(len(summaries) == 1 and summaries[0] == "Number of signers: 1",
            "APK must report exactly one current signer")
    records = [line for line in output.splitlines()
               if line.startswith("Signer") and "certificate SHA-256 digest" in line]
    require(records, "APK signer certificate records are missing or unrecognized")
    labels = set()
    for record in records:
        numbered = re.fullmatch(r"Signer #(\d+) certificate SHA-256 digest: ([0-9a-fA-F]{64})", record)
        scoped = re.fullmatch(
            r"Signer \(minSdkVersion=(\d+)(?: \(dev release=true\))?, maxSdkVersion=(\d+)\) "
            r"certificate SHA-256 digest: ([0-9a-fA-F]{64})", record)
        if numbered:
            require(numbered[1] == "1" and len(records) == 1,
                    "APK must contain exactly one numbered signer certificate")
            labels.add("numbered")
            certificate = numbered[2].lower()
        elif scoped:
            require(1 <= int(scoped[1]) <= int(scoped[2]) <= 2147483647,
                    "APK signer SDK interval is invalid")
            labels.add("scoped")
            certificate = scoped[3].lower()
        else:
            raise ValueError("APK signer certificate output format is unrecognized")
        require(certificate == SIGNING_CERTIFICATE_SHA256,
                "Actual APK signer differs from the existing application's certificate")
    require(len(labels) == 1, "APK signer certificate output mixes incompatible formats")
    return SIGNING_CERTIFICATE_SHA256


def apk_metadata(apk, aapt, apksigner):
    with regular_file(apk):
        pass
    badging = subprocess.run([aapt, "dump", "badging", str(apk)], check=True,
                             capture_output=True, text=True, timeout=60).stdout
    package = re.search(r"^package: name='([^']+)' versionCode='(\d+)' versionName='([^']*)'", badging, re.M)
    minimum = re.search(r"^sdkVersion:'(\d+)'", badging, re.M)
    require(package and minimum, "Unable to read package/version/minimum SDK from actual APK")
    verified = subprocess.run([apksigner, "verify", "--verbose", "--print-certs", str(apk)],
                              check=True, capture_output=True, text=True, timeout=60).stdout
    certificate = verified_signing_certificate(verified)
    return {"package_name": package[1], "version_code": int(package[2]),
            "version_name": package[3], "min_sdk": int(minimum[1]),
            "signing_certificate_sha256": certificate}


def prepare(args):
    apk = Path(args.apk).absolute()
    first_hash, first_size = sha256_file(apk)
    metadata = apk_metadata(apk, sdk_tool("aapt", args.aapt), sdk_tool("apksigner", args.apksigner))
    digest, size = sha256_file(apk)
    require((digest, size) == (first_hash, first_size), "APK changed during Android SDK verification")
    notes = Path(args.release_notes_file).read_text(encoding="utf-8") if args.release_notes_file else ""
    manifest = {"schema_version": 1, **metadata, "apk_url": PUBLIC_BASE_URL + "/bark-android-" + metadata["version_name"] + ".apk",
                "sha256": digest, "size_bytes": size, "release_notes": notes,
                "published_at": dt.datetime.now(dt.timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z")}
    filename = validate_manifest(manifest)
    output = directory(args.output_dir)
    # Re-running preparation on an identical bundle preserves its publication identity.
    if (output / "stable.json").exists():
        previous = load_manifest(output / "stable.json")
        identity = FIELDS - {"published_at"}
        require(all(previous[k] == manifest[k] for k in identity), "Output directory already contains a different bundle")
        manifest = previous
    raw = manifest_bytes(manifest)
    write_atomic(output / filename, source=apk, immutable=True, expected_hash=digest, expected_size=size)
    write_atomic(output / (filename + ".json"), data=raw, immutable=True)
    write_atomic(output / "stable.json", data=raw)
    print(json.dumps({"status": "prepared", "version_code": manifest["version_code"],
                      "apk_filename": filename, "sha256": digest, "size_bytes": size}))


def publish(args):
    bundle = Path(args.bundle_dir).resolve()
    manifest = load_manifest(bundle / "stable.json")
    filename = validate_manifest(manifest)
    require(load_manifest(bundle / (filename + ".json")) == manifest, "Version sidecar differs from stable manifest")
    digest, size = sha256_file(bundle / filename)
    require(digest == manifest["sha256"] and size == manifest["size_bytes"], "APK size or SHA-256 does not match manifest")
    output = directory(args.release_dir)
    lock_path = output / ".publish.lock"
    lock_fd = os.open(lock_path, os.O_CREAT | os.O_RDWR | getattr(os, "O_NOFOLLOW", 0), 0o600)
    with os.fdopen(lock_fd, "a+b") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        if (output / "stable.json").exists() or (output / "stable.json").is_symlink():
            current = load_manifest(output / "stable.json")
            require(manifest["version_code"] >= current["version_code"], "Refusing to publish a lower version code")
            if manifest["version_code"] == current["version_code"]:
                require(manifest == current, "Same version code already identifies a different active release")
        # Every retained sidecar owns its version code and its filename forever.
        for prior_path in output.glob("*.apk.json"):
            prior = load_manifest(prior_path)
            if prior["version_code"] == manifest["version_code"]:
                require(prior == manifest, "Same version code already identifies a different release")
        raw = manifest_bytes(manifest)
        write_atomic(output / filename, source=bundle / filename, immutable=True,
                     expected_hash=manifest["sha256"], expected_size=manifest["size_bytes"])
        write_atomic(output / (filename + ".json"), data=raw, immutable=True)
        # Nothing references the new package until both immutable files are durable.
        write_atomic(output / "stable.json", data=raw)
    print(json.dumps({"status": "published", "version_code": manifest["version_code"],
                      "apk_filename": filename, "sha256": digest}))


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    build = commands.add_parser("prepare", help="Verify the actual signed APK using Android SDK tools")
    build.add_argument("--apk", required=True)
    build.add_argument("--output-dir", required=True)
    build.add_argument("--release-notes-file")
    build.add_argument("--aapt")
    build.add_argument("--apksigner")
    build.set_defaults(run=prepare)
    activate = commands.add_parser("publish", help="Verify a CI-produced bundle and atomically publish it without an Android SDK")
    activate.add_argument("--bundle-dir", required=True)
    activate.add_argument("--release-dir", required=True)
    activate.set_defaults(run=publish)
    args = parser.parse_args(argv)
    try:
        args.run(args)
    except (ValueError, OSError, subprocess.SubprocessError) as error:
        # SDK subprocess output can include local paths; never dump its output.
        if isinstance(error, subprocess.SubprocessError):
            print("Release failed: Android SDK verification did not complete successfully", file=sys.stderr)
        else:
            print(f"Release failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
