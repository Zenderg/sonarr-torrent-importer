#!/usr/bin/env python3

import hashlib
import pathlib
import sys


PIECE_LENGTH = 16 * 1024


def bbytes(value: bytes) -> bytes:
    return str(len(value)).encode("ascii") + b":" + value


def bstring(value: str) -> bytes:
    return bbytes(value.encode("utf-8"))


def bint(value: int) -> bytes:
    return b"i" + str(value).encode("ascii") + b"e"


def file_entry(relative: pathlib.Path, size: int) -> bytes:
    components = b"".join(bstring(component) for component in relative.parts)
    return b"".join((b"d", bstring("length"), bint(size), bstring("path"), b"l", components, b"e", b"e"))


def main() -> None:
    if len(sys.argv) != 4:
        raise SystemExit("usage: create_rolling_torrent_fixture.py <source-root> <torrent-path> <webseed-base-url>")
    source_root = pathlib.Path(sys.argv[1])
    torrent_path = pathlib.Path(sys.argv[2])
    files = sorted(item for item in source_root.rglob("*") if item.is_file())
    if not files:
        raise SystemExit("rolling source root is empty")
    payload = b"".join(item.read_bytes() for item in files)
    pieces = b"".join(hashlib.sha1(payload[offset : offset + PIECE_LENGTH]).digest() for offset in range(0, len(payload), PIECE_LENGTH))
    entries = b"".join(file_entry(item.relative_to(source_root), item.stat().st_size) for item in files)
    info = b"".join((
        b"d", bstring("files"), b"l", entries, b"e",
        bstring("name"), bstring(source_root.name),
        bstring("piece length"), bint(PIECE_LENGTH),
        bstring("pieces"), bbytes(pieces), b"e",
    ))
    torrent = b"".join((
        b"d", bstring("announce"), bstring("http://127.0.0.1:9/announce"),
        bstring("info"), info,
        bstring("url-list"), bstring(sys.argv[3]), b"e",
    ))
    torrent_path.write_bytes(torrent)
    print(hashlib.sha1(info).hexdigest())


if __name__ == "__main__":
    main()
