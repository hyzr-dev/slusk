#!/usr/bin/env python3
"""Create a deterministic 100-200 album wanted set in an empty Lidarr."""

from __future__ import annotations

from datetime import date
import json
import os
import sys
import time
from urllib.error import HTTPError, URLError
from urllib.parse import urlencode
from urllib.request import Request, urlopen

BASE_URL = os.environ.get("LIDARR_URL", "http://lidarr:8686").rstrip("/")
API_KEY = os.environ["LIDARR_API_KEY"]
TARGET = int(os.environ.get("WANTED_ALBUMS", "150"))
ROOT = "/music/library"

# Stable MusicBrainz IDs avoid ambiguous text lookup. The list deliberately
# mixes prolific and smaller discographies so Lidarr has comfortably more than
# 200 released albums to choose from.
ARTISTS = [
    ("Miles Davis", "561d854a-6a28-4aa7-8c99-323e6ce46c2a"),
    ("Bob Dylan", "72c536dc-7137-4477-a521-567eeb840fa8"),
    ("David Bowie", "5441c29d-3602-4898-b1a1-b77fa23b8e50"),
    ("The Beatles", "b10bbbfc-cf9e-42e0-be17-e2c3e1d2600d"),
    ("Pink Floyd", "83d91898-7763-47d7-b03b-b92132375c47"),
    ("Radiohead", "a74b1b7f-71a5-4011-9441-d0b5e4122711"),
    ("Björk", "87c5dedd-371d-4a53-9f7f-80522fb7f3cb"),
    ("Daft Punk", "056e4f3e-d505-4dad-8ec1-d04f521cbb56"),
    ("Nirvana", "5b11f4ce-a62d-471e-81fc-a69a8278c7da"),
    ("Metallica", "65f4f0c5-ef9e-490c-aee3-909e7ae6b2ab"),
    ("The Cure", "69ee3720-a7cb-4402-b48d-a02c366f2bcf"),
    ("Fleetwood Mac", "bd13909f-1c29-4c27-a874-d4aaf27c5b1a"),
]


def api(method: str, path: str, body: object | None = None, query: dict[str, object] | None = None):
    url = f"{BASE_URL}/api/v1/{path.lstrip('/')}"
    if query:
        url += "?" + urlencode(query)
    data = None if body is None else json.dumps(body).encode()
    request = Request(
        url,
        data=data,
        method=method,
        headers={"X-Api-Key": API_KEY, "Content-Type": "application/json"},
    )
    try:
        with urlopen(request, timeout=30) as response:
            payload = response.read()
            return json.loads(payload) if payload else None
    except HTTPError as exc:
        detail = exc.read().decode(errors="replace")
        raise RuntimeError(f"{method} {url} returned {exc.code}: {detail[:1000]}") from exc


def wait_for_lidarr() -> None:
    deadline = time.monotonic() + 300
    while time.monotonic() < deadline:
        try:
            status = api("GET", "system/status")
            print(f"Lidarr {status.get('version', 'unknown')} is ready", flush=True)
            return
        except (OSError, RuntimeError, URLError):
            time.sleep(2)
    raise RuntimeError("Lidarr did not become ready within five minutes")


def ensure_root_folder(quality_profile_id: int, metadata_profile_id: int) -> None:
    roots = api("GET", "rootfolder")
    if any(root.get("path", "").rstrip("/") == ROOT for root in roots):
        return
    api(
        "POST",
        "rootfolder",
        {
            "name": "PR lab music",
            "path": ROOT,
            "defaultQualityProfileId": quality_profile_id,
            "defaultMetadataProfileId": metadata_profile_id,
            "defaultMonitorOption": "none",
            "defaultNewItemMonitorOption": "none",
            "defaultTags": [],
        },
    )
    print(f"created Lidarr root folder {ROOT}")


def ensure_artists(quality_profile_id: int, metadata_profile_id: int) -> list[int]:
    existing = {artist.get("foreignArtistId"): artist for artist in api("GET", "artist")}
    ids: list[int] = []
    for name, mbid in ARTISTS:
        artist = existing.get(mbid)
        if artist is None:
            matches = api("GET", "artist/lookup", query={"term": f"lidarr:{mbid}"})
            if not matches:
                print(f"warning: Lidarr could not look up {name} ({mbid})", file=sys.stderr)
                continue
            payload = matches[0]
            payload.update(
                {
                    "qualityProfileId": quality_profile_id,
                    "metadataProfileId": metadata_profile_id,
                    "rootFolderPath": ROOT,
                    "monitored": True,
                    "monitorNewItems": "none",
                    "addOptions": {"monitor": "none", "searchForMissingAlbums": False},
                }
            )
            artist = api("POST", "artist", payload)
            print(f"added {name}")
        if not artist.get("monitored"):
            artist["monitored"] = True
            artist["monitorNewItems"] = "none"
            artist = api("PUT", f"artist/{artist['id']}", artist)
        ids.append(int(artist["id"]))
    return ids


def get_released_albums(artist_ids: list[int]) -> list[dict]:
    albums: list[dict] = []
    today = date.today().isoformat()
    for artist_id in artist_ids:
        for album in api("GET", "album", query={"artistId": artist_id}):
            release_date = album.get("releaseDate") or "9999-12-31"
            statistics = album.get("statistics") or {}
            # trackCount only counts tracks of monitored albums, and everything
            # starts unmonitored here, so it is always 0 at this point. Use
            # totalTrackCount, which is monitor-independent and therefore also
            # correct when reseeding a lab that is already monitored.
            total_track_count = int(statistics.get("totalTrackCount", 0))
            track_file_count = int(statistics.get("trackFileCount", 0))
            if release_date[:10] <= today and total_track_count > track_file_count:
                albums.append(album)
    albums.sort(key=lambda item: (item.get("releaseDate") or "9999", item.get("title") or "", item["id"]))
    return albums


def wait_for_albums(artist_ids: list[int]) -> list[dict]:
    deadline = time.monotonic() + 600
    albums: list[dict] = []
    while time.monotonic() < deadline:
        albums = get_released_albums(artist_ids)
        print(f"Lidarr has loaded {len(albums)} released albums", flush=True)
        if len(albums) >= TARGET:
            return albums
        time.sleep(10)
    raise RuntimeError(f"only {len(albums)} released albums loaded; need {TARGET}")


def set_monitored(albums: list[dict]) -> None:
    # This is a dedicated destructive lab: clear every album, including any
    # manually-added artist, so repeated seeding cannot inflate wanted/missing.
    all_ids = [int(album["id"]) for album in api("GET", "album")]
    selected_ids = [int(album["id"]) for album in albums[:TARGET]]
    if all_ids:
        api("PUT", "album/monitor", {"albumIds": all_ids, "monitored": False})
    api("PUT", "album/monitor", {"albumIds": selected_ids, "monitored": True})
    print(f"marked exactly {len(selected_ids)} released, missing albums as monitored")


def report_wanted() -> None:
    deadline = time.monotonic() + 120
    total = 0
    while time.monotonic() < deadline:
        result = api(
            "GET",
            "wanted/missing",
            query={"page": 1, "pageSize": 1, "sortKey": "releaseDate", "sortDirection": "ascending"},
        )
        total = int(result.get("totalRecords", 0))
        if total == TARGET:
            print(f"Lidarr wanted/missing now reports exactly {total} albums")
            return
        time.sleep(2)
    raise RuntimeError(f"wanted/missing reports {total} albums; expected exactly {TARGET}")


def main() -> int:
    if not 1 <= TARGET <= 500:
        print("WANTED_ALBUMS must be between 1 and 500", file=sys.stderr)
        return 2
    try:
        wait_for_lidarr()
        quality_profiles = api("GET", "qualityprofile")
        metadata_profiles = api("GET", "metadataprofile")
        if not quality_profiles or not metadata_profiles:
            raise RuntimeError("Lidarr has no quality or metadata profile")
        quality_profile_id = int(quality_profiles[0]["id"])
        metadata_profile_id = int(metadata_profiles[0]["id"])
        ensure_root_folder(quality_profile_id, metadata_profile_id)
        artist_ids = ensure_artists(quality_profile_id, metadata_profile_id)
        albums = wait_for_albums(artist_ids)
        set_monitored(albums)
        report_wanted()
    except (KeyError, OSError, RuntimeError, URLError, ValueError) as exc:
        print(f"seed failed: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
