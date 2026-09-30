#!/usr/bin/env python3
"""Mirror the official stable GitHub release into Bark's NAS release directory.

Network/size checks happen here; the shared publisher checks the manifest, APK
digest, pinned certificate metadata, and monotonic activation. APK signatures
are inspected by the CI prepare step and again by the Android installer.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import time
from urllib.error import HTTPError, URLError
from urllib.parse import urlsplit
from urllib.request import HTTPRedirectHandler, Request, build_opener

RELEASE_API = "https://api.github.com/repos/atrl/bark-android/releases/latest"
REPOSITORY_PATH = "/atrl/bark-android/releases/download/"
DOWNLOAD_HOSTS = frozenset({
    "github.com", "api.github.com", "objects.githubusercontent.com",
    "release-assets.githubusercontent.com", "github-releases.githubusercontent.com",
})
MANIFEST_LIMIT = 64 * 1024
METADATA_LIMIT = 1024 * 1024
APK_LIMIT = 256 * 1024 * 1024
CERTIFICATE = "60408b509647dc7689bca6435a3eef93d475d868232fe88d848e853c03ffe896"


def validate_download_url(value):
    if not isinstance(value, str):
        raise ValueError("release URL must be a string")
    parsed = urlsplit(value)
    if (parsed.scheme != "https" or parsed.hostname not in DOWNLOAD_HOSTS
            or parsed.username is not None or parsed.password is not None
            or parsed.port not in (None, 443) or parsed.fragment):
        raise ValueError("release download must use an approved HTTPS host")
    return value


class SafeRedirects(HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, message, headers, new_url):
        validate_download_url(new_url)
        return super().redirect_request(request, fp, code, message, headers, new_url)


def download(url, destination, maximum, opener=None):
    validate_download_url(url)
    request = Request(url, headers={
        "User-Agent": "Bark-Release-Sync/1.0", "Accept": "application/json, */*",
    })
    opener = opener or build_opener(SafeRedirects())
    size = 0
    deadline = time.monotonic() + 600
    with opener.open(request, timeout=30) as response:
        validate_download_url(response.geturl())
        if response.status != 200:
            raise ValueError("release download did not return HTTP 200")
        length = response.headers.get("Content-Length")
        if length is not None and (not length.isdecimal() or int(length) > maximum):
            raise ValueError("release download is too large")
        with destination.open("xb") as output:
            while chunk := response.read(64 * 1024):
                if time.monotonic() > deadline:
                    raise ValueError("release download exceeded its time limit")
                size += len(chunk)
                if size > maximum:
                    raise ValueError("release download exceeds its size limit")
                output.write(chunk)
        if length is not None and size != int(length):
            raise ValueError("release download was truncated")
    return size


def asset_url(release, name):
    assets = release.get("assets", [])
    if not isinstance(assets, list) or not all(isinstance(asset, dict) for asset in assets):
        raise ValueError("release assets must be an array of objects")
    matches = [asset for asset in assets if asset.get("name") == name]
    if len(matches) != 1:
        raise ValueError("stable release must contain exactly one required asset")
    url = validate_download_url(matches[0].get("browser_download_url"))
    parsed = urlsplit(url)
    if parsed.hostname != "github.com" or not parsed.path.startswith(REPOSITORY_PATH):
        raise ValueError("release asset belongs to an unexpected repository")
    if parsed.query or parsed.path.rsplit("/", 1)[-1] != name:
        raise ValueError("release asset filename is invalid")
    size = matches[0].get("size")
    if type(size) is not int or not 0 < size <= APK_LIMIT:
        raise ValueError("release asset exceeds the size limit")
    return url


def manifest_filename(manifest, tag):
    if (not isinstance(manifest, dict) or type(manifest.get("schema_version")) is not int
            or manifest.get("schema_version") != 1
            or manifest.get("package_name") != "day.bark.android"
            or manifest.get("signing_certificate_sha256") != CERTIFICATE
            or tag != "v" + str(manifest.get("version_name", ""))):
        raise ValueError("stable manifest identity does not match the official release")
    if not isinstance(manifest.get("apk_url"), str):
        raise ValueError("manifest APK URL must be a string")
    url = urlsplit(manifest["apk_url"])
    if (url.scheme != "https" or url.hostname != "bark.atrl.me"
            or url.port not in (None, 443) or url.username is not None
            or url.password is not None or url.query or url.fragment):
        raise ValueError("manifest does not use the official NAS update origin")
    match = re.fullmatch(r"/android/releases/(bark-android-[A-Za-z0-9][A-Za-z0-9._+\-]{0,100}\.apk)", url.path)
    if not match or ".." in match.group(1):
        raise ValueError("manifest APK path is invalid")
    size = manifest.get("size_bytes")
    if type(size) is not int or not 0 < size <= APK_LIMIT:
        raise ValueError("manifest APK size is invalid")
    return match.group(1)


def current_is_valid(release_dir, manifest, filename):
    for name in ("stable.json", filename + ".json"):
        path = release_dir / name
        if path.is_symlink() or not path.is_file() or path.stat().st_size > MANIFEST_LIMIT:
            return False
        if json.loads(path.read_bytes()) != manifest:
            return False
    artifact = release_dir / filename
    if (artifact.is_symlink() or not artifact.is_file()
            or artifact.stat().st_size != manifest["size_bytes"]):
        return False
    digest = hashlib.sha256()
    with artifact.open("rb") as source:
        for chunk in iter(lambda: source.read(64 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest() == manifest.get("sha256")


def sync_once(release_dir, publisher, opener=None):
    release_dir.mkdir(parents=True, exist_ok=True)
    # Stage APK bytes on disk, not memory-backed /tmp inside the NAS container.
    with tempfile.TemporaryDirectory(prefix=".sync-", dir=release_dir) as temporary:
        temporary = Path(temporary)
        metadata_path = temporary / "github-release.json"
        try:
            download(RELEASE_API, metadata_path, METADATA_LIMIT, opener)
        except HTTPError as error:
            if error.code == 404:
                print("No stable GitHub release published yet.", flush=True)
                return
            raise
        release = json.loads(metadata_path.read_bytes())
        if not isinstance(release, dict):
            raise ValueError("GitHub release metadata must be an object")
        if release.get("draft") or release.get("prerelease"):
            raise ValueError("draft and prerelease builds are not stable updates")
        bundle = temporary / "bundle"
        bundle.mkdir(mode=0o700)
        stable = bundle / "stable.json"
        download(asset_url(release, "stable.json"), stable, MANIFEST_LIMIT, opener)
        manifest = json.loads(stable.read_bytes())
        filename = manifest_filename(manifest, release.get("tag_name"))
        current_path = release_dir / "stable.json"
        if (current_path.is_file() and not current_path.is_symlink()
                and current_path.stat().st_size <= MANIFEST_LIMIT):
            current = json.loads(current_path.read_bytes())
            if (isinstance(current, dict) and type(current.get("version_code")) is int
                    and type(manifest.get("version_code")) is int
                    and current["version_code"] > manifest["version_code"]):
                print("GitHub release is older than the installed NAS release; retained current.", flush=True)
                return
            if current == manifest and current_is_valid(release_dir, manifest, filename):
                print("NAS stable release is current.", flush=True)
                return
        download(asset_url(release, filename), bundle / filename, manifest["size_bytes"], opener)
        (bundle / (filename + ".json")).write_bytes(stable.read_bytes())
        # This invokes the same publisher used by the locally prepared release.
        result = subprocess.run([
            sys.executable, str(publisher), "publish", "--bundle-dir", str(bundle),
            "--release-dir", str(release_dir),
        ], capture_output=True, text=True, timeout=120)
        if result.returncode:
            raise ValueError("release publisher rejected the downloaded bundle")
        print("Activated Android stable release " + str(manifest["version_name"]), flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--release-dir", type=Path, required=True)
    parser.add_argument("--interval", type=int, default=1800)
    parser.add_argument("--once", action="store_true")
    args = parser.parse_args()
    if args.interval < 60:
        parser.error("--interval must be at least 60 seconds")
    publisher = Path(__file__).with_name("prepare-android-release.py")
    if not publisher.is_file():
        parser.error("the shared release publisher is missing")
    os.umask(0o077)
    while True:
        success = False
        try:
            sync_once(args.release_dir, publisher)
            success = True
        except (OSError, ValueError, KeyError, TypeError, subprocess.SubprocessError) as error:
            # Never log redirect URLs containing temporary CDN credentials.
            suffix = " HTTP " + str(error.code) if isinstance(error, HTTPError) else ""
            print("Release sync failed: " + type(error).__name__ + suffix, file=sys.stderr, flush=True)
        if args.once:
            return 0 if success else 1
        time.sleep(args.interval)


if __name__ == "__main__":
    sys.exit(main())
