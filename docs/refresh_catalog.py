#!/usr/bin/env python3
"""Refresh the GitHub Pages catalog from the local OfflineVideos service."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import tempfile
from urllib.request import Request, urlopen

ROOT = Path(__file__).resolve().parent
CATALOG = ROOT / "data" / "catalog.json"
API_URL = os.environ.get("OFFLINEVIDEOS_CATALOG_API", "http://127.0.0.1:8732/api/catalog")


def cache_thumbnail(item: dict) -> None:
    name = item.get("thumbnail_file")
    if name:
        path = (CATALOG.parent / name).resolve()
        if CATALOG.parent.resolve() not in path.parents:
            raise ValueError("thumbnail path escapes the static data directory")
        if path.is_file() and path.stat().st_size:
            return
    else:
        url = item.get("thumbnail_url")
        if not url:
            raise ValueError(f"catalog item {item['source']}/{item['id']} has no thumbnail URL")
        name = f"thumbnails/{hashlib.sha256(url.encode()).hexdigest()}.jpg"
        item["thumbnail_file"] = name
        path = CATALOG.parent / name
        if path.is_file() and path.stat().st_size:
            return

    url = item.get("thumbnail_url")
    if not url:
        raise ValueError(f"thumbnail file is missing and no URL is available for {item['id']}")
    request = Request(
        url,
        headers={
            "User-Agent": "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/132.0 Safari/537.36",
            "Referer": "https://www.bilibili.com/",
        },
    )
    with urlopen(request, timeout=30) as response:
        image = response.read(10 * 1024 * 1024 + 1)
        content_type = response.headers.get("Content-Type", "").lower()
    if len(image) > 10 * 1024 * 1024 or not content_type.startswith("image/"):
        raise ValueError(f"thumbnail response is not a valid image for {item['id']}")
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=".thumbnail-", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as output:
            output.write(image)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def load_catalog(raw: bytes, require_source_url: bool = True) -> dict:
    catalog = json.loads(raw)
    if not isinstance(catalog, dict) or not isinstance(catalog.get("items"), list):
        raise ValueError("catalog response must contain an items array")
    fields = (
        "id",
        "source",
        "source_url",
        "title",
        "uploader",
        "channel",
        "duration_seconds",
        "downloaded_at",
        "thumbnail_file",
        "thumbnail_url",
    )
    items = []
    for original in catalog["items"]:
        if not isinstance(original, dict) or not original.get("id") or not original.get("source"):
            raise ValueError("catalog contains an item without source or id")
        if not original.get("thumbnail_file") and not original.get("thumbnail_url"):
            raise ValueError(f"catalog item {original['source']}/{original['id']} has no thumbnail")
        if require_source_url and not original.get("source_url"):
            raise ValueError(f"catalog item {original['source']}/{original['id']} has no source URL")
        item = {key: original[key] for key in fields if key in original}
        cache_thumbnail(item)
        items.append(item)
    return {"version": catalog.get("version", 1), "updated_at": catalog.get("updated_at", ""), "items": items}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true", help="validate the API catalog without writing it")
    args = parser.parse_args()

    with urlopen(API_URL, timeout=30) as response:
        current = load_catalog(response.read())

    previous = None
    try:
        previous = load_catalog(CATALOG.read_bytes(), require_source_url=False)
    except FileNotFoundError:
        pass

    # The service rebuilds the catalog hourly. Avoid publishing timestamp-only
    # commits when the actual video records have not changed.
    if previous and current.get("items") == previous.get("items"):
        current["updated_at"] = previous.get("updated_at", current.get("updated_at", ""))

    encoded = (json.dumps(current, ensure_ascii=False, separators=(",", ":")) + "\n").encode()
    if args.check:
        print(f"Valid catalog: {len(current['items'])} items")
        return 0
    if CATALOG.exists() and CATALOG.read_bytes() == encoded:
        print("Catalog is already current")
        return 0

    CATALOG.parent.mkdir(parents=True, exist_ok=True)
    fd, name = tempfile.mkstemp(prefix=".catalog-", suffix=".json", dir=CATALOG.parent)
    try:
        with os.fdopen(fd, "wb") as output:
            output.write(encoded)
            output.flush()
            os.fsync(output.fileno())
        os.replace(name, CATALOG)
    finally:
        if os.path.exists(name):
            os.unlink(name)
    print(f"Updated catalog: {len(current['items'])} items")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
