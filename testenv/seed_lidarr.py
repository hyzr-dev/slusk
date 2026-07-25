#!/usr/bin/env python3
"""Create a deterministic 100-200 album wanted set in an empty Lidarr."""

from __future__ import annotations

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
        ids.append(int(artist["id"]))
    return ids


def wait_for_idle() -> None:
    """Block until Lidarr has no queued or running artist/album refresh.

    POST /artist returns the artist as requested, before the asynchronous
    RefreshArtist command has run. Reading monitored state from that response —
    or inferring readiness from how many albums are visible — races the refresh,
    which re-applies the artist's monitor policy and can undo whatever the seed
    just set. Ask Lidarr what it is doing instead of guessing from its output.
    """
    deadline = time.monotonic() + 900
    watched = {"RefreshArtist", "RefreshAlbum", "RescanFolders"}
    while time.monotonic() < deadline:
        commands = api("GET", "command") or []
        busy = [
            command
            for command in commands
            if command.get("name") in watched
            and command.get("status") in {"queued", "started"}
        ]
        if not busy:
            return
        print(f"waiting for {len(busy)} Lidarr refresh command(s) to finish", flush=True)
        time.sleep(5)
    raise RuntimeError("Lidarr refresh commands did not finish within fifteen minutes")


def ensure_artists_monitored(artist_ids: list[int]) -> None:
    """Re-read every artist and monitor it if the refresh left it unmonitored.

    wanted/missing requires BOTH the album and its artist to be monitored, so an
    artist Lidarr quietly left unmonitored silently removes all of its albums
    from the wanted set. Must run after wait_for_idle, or it reads the same
    pre-refresh state it is meant to correct.
    """
    fixed = 0
    for artist_id in artist_ids:
        artist = api("GET", f"artist/{artist_id}")
        if artist.get("monitored"):
            continue
        artist["monitored"] = True
        artist["monitorNewItems"] = "none"
        api("PUT", f"artist/{artist_id}", artist)
        fixed += 1
    if fixed:
        print(f"monitored {fixed} artist(s) the refresh had left unmonitored")


def set_album_monitored(album_ids: list[int], monitored: bool) -> None:
    if album_ids:
        api("PUT", "album/monitor", {"albumIds": album_ids, "monitored": monitored})


def fetch_wanted_missing() -> list[dict]:
    """Page through everything Lidarr itself considers missing."""
    records: list[dict] = []
    page = 1
    while True:
        result = api(
            "GET",
            "wanted/missing",
            query={
                "page": page,
                "pageSize": 1000,
                "sortKey": "releaseDate",
                "sortDirection": "ascending",
            },
        )
        batch = result.get("records") or []
        records.extend(batch)
        if not batch or len(records) >= int(result.get("totalRecords", 0)):
            return records
        page += 1


def select_wanted() -> list[int]:
    """Monitor everything, then keep exactly TARGET of Lidarr's own missing set.

    The seed used to reimplement Lidarr's definition of missing (releaseDate in
    the past AND totalTrackCount > trackFileCount) and then assert that
    wanted/missing agreed. It cannot agree in general: an album whose files
    already sit in the library is missing by the seed's definition and not by
    Lidarr's, and the assertion demanded an exact count, so the seed failed with
    no way to converge. Asking wanted/missing which albums qualify — and only
    then trimming to TARGET — makes the count correct by construction, whatever
    Lidarr's rules happen to be.

    This is a dedicated destructive lab, so every album is fair game, including
    those of any manually added artist. Repeated seeding therefore cannot
    inflate the wanted set.
    """
    all_ids = [int(album["id"]) for album in api("GET", "album")]
    set_album_monitored(all_ids, True)
    wait_for_idle()

    candidates = fetch_wanted_missing()
    if len(candidates) < TARGET:
        raise RuntimeError(
            f"Lidarr reports only {len(candidates)} missing albums; need {TARGET}"
        )
    candidates.sort(
        key=lambda item: (item.get("releaseDate") or "9999", item.get("title") or "", item["id"])
    )
    selected = [int(album["id"]) for album in candidates[:TARGET]]
    set_album_monitored([i for i in all_ids if i not in set(selected)], False)
    print(f"kept {len(selected)} of Lidarr's {len(candidates)} missing albums monitored")
    return selected


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
        # Order matters: nothing may read or write monitor state until the
        # refresh that Lidarr queues on artist add has finished, because that
        # refresh re-applies addOptions.monitor ("none") and would undo it.
        wait_for_idle()
        ensure_artists_monitored(artist_ids)
        select_wanted()
        report_wanted()
    except (KeyError, OSError, RuntimeError, URLError, ValueError) as exc:
        print(f"seed failed: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
