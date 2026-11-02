#!/usr/bin/env python3
"""Refresh the GitHub Pages catalog from the local OfflineVideos service."""

import argparse
import json
import os
from pathlib import Path
import tempfile
from urllib.request import urlopen

ROOT = Path(__file__).resolve().parent
CATALOG = ROOT / "data" / "catalog.json"
API_URL = os.environ.get("OFFLINEVIDEOS_CATALOG_API", "http://127.0.0.1:8732/api/catalog")


def load_catalog(raw: bytes) -> dict:
    catalog = json.loads(raw)
    if not isinstance(catalog, dict) or not isinstance(catalog.get("items"), list):
        raise ValueError("catalog response must contain an items array")
    fields = (
        "id",
        "source",
        "title",
        "uploader",
        "channel",
        "duration_seconds",
        "thumbnail_file",
        "thumbnail_url",
    )
    items = []
    for item in catalog["items"]:
        if not isinstance(item, dict) or not item.get("id") or not item.get("source"):
            raise ValueError("catalog contains an item without source or id")
        if not item.get("thumbnail_file") and not item.get("thumbnail_url"):
            raise ValueError(f"catalog item {item['source']}/{item['id']} has no thumbnail")
        items.append({key: item[key] for key in fields if key in item})
    return {"version": catalog.get("version", 1), "updated_at": catalog.get("updated_at", ""), "items": items}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true", help="validate the API catalog without writing it")
    args = parser.parse_args()

    with urlopen(API_URL, timeout=30) as response:
        current = load_catalog(response.read())

    previous = None
    try:
        previous = load_catalog(CATALOG.read_bytes())
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
