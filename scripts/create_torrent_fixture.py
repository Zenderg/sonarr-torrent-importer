#!/usr/bin/env python3

import hashlib
import pathlib
import sys


PIECE_LENGTH = 16 * 1024


def bencode_bytes(value: bytes) -> bytes:
    return str(len(value)).encode("ascii") + b":" + value


def bencode_string(value: str) -> bytes:
    return bencode_bytes(value.encode("utf-8"))


def bencode_integer(value: int) -> bytes:
    return b"i" + str(value).encode("ascii") + b"e"


def bencode_file_entry(relative_path: pathlib.Path, size: int) -> bytes:
    path_items = b"".join(bencode_string(item) for item in relative_path.parts)
    return b"".join(
        (
            b"d",
            bencode_string("length"),
            bencode_integer(size),
            bencode_string("path"),
            b"l",
            path_items,
            b"e",
            b"e",
        )
    )


def main() -> None:
    if len(sys.argv) != 3:
        raise SystemExit(
            "usage: create_torrent_fixture.py <media-path> <torrent-path>"
        )

    media_path = pathlib.Path(sys.argv[1])
    torrent_path = pathlib.Path(sys.argv[2])
    media = media_path.read_bytes()
    if not media:
        raise SystemExit("media fixture is empty")

    pieces = b"".join(
        hashlib.sha1(media[offset : offset + PIECE_LENGTH]).digest()
        for offset in range(0, len(media), PIECE_LENGTH)
    )
    relative_path = pathlib.Path(media_path.name)
    info = b"".join(
        (
            b"d",
            bencode_string("files"),
            b"l",
            bencode_file_entry(relative_path, len(media)),
            b"e",
            bencode_string("name"),
            bencode_string(media_path.parent.name),
            bencode_string("piece length"),
            bencode_integer(PIECE_LENGTH),
            bencode_string("pieces"),
            bencode_bytes(pieces),
            b"e",
        )
    )
    torrent = b"".join(
        (
            b"d",
            bencode_string("announce"),
            bencode_string("http://127.0.0.1:9/announce"),
            bencode_string("info"),
            info,
            b"e",
        )
    )
    torrent_path.write_bytes(torrent)
    print(hashlib.sha1(info).hexdigest())


if __name__ == "__main__":
    main()
