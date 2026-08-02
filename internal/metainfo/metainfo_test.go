package metainfo

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

func TestParseSingleFileAndRawInfoHashes(t *testing.T) {
	pieces := bytes.Repeat([]byte{0x11}, sha1.Size)
	info := bdict(
		bentry("length", bint(4)),
		bentry("name", bstr([]byte("file.mkv"))),
		bentry("piece length", bint(4)),
		bentry("pieces", bstr(pieces)),
	)
	parsed, err := Parse(torrent(info))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Name != "file.mkv" || parsed.PieceLength != 4 || parsed.TotalLength != 4 || parsed.MultiFile || parsed.Hybrid {
		t.Fatalf("unexpected metainfo: %+v", parsed)
	}
	if len(parsed.Files) != 1 || parsed.Files[0] != (File{Index: 0, Path: "file.mkv", Length: 4, Offset: 0}) {
		t.Fatalf("unexpected files: %+v", parsed.Files)
	}
	if len(parsed.PieceHashes) != 1 || !bytes.Equal(parsed.PieceHashes[0][:], pieces) {
		t.Fatalf("unexpected piece hashes: %x", parsed.PieceHashes)
	}
	if parsed.RawInfoSHA1 != sha1.Sum(info) || parsed.RawInfoSHA256 != sha256.Sum256(info) {
		t.Fatal("raw info hashes were not calculated over the exact info dictionary bytes")
	}
}

func TestParseAppendRevisionFixture(t *testing.T) {
	firstHash := bytes.Repeat([]byte{0x21}, sha1.Size)
	secondHash := bytes.Repeat([]byte{0x22}, sha1.Size)
	oldInfo := multiInfo("Release", 4, firstHash,
		fileEntry(4, "[01].mkv"),
	)
	newInfo := multiInfo("Release", 4, append(append([]byte(nil), firstHash...), secondHash...),
		fileEntry(4, "[01].mkv"),
		fileEntry(4, "[02].mkv"),
	)

	oldRevision, err := Parse(torrent(oldInfo))
	if err != nil {
		t.Fatal(err)
	}
	newRevision, err := Parse(torrent(newInfo))
	if err != nil {
		t.Fatal(err)
	}
	if len(oldRevision.Files) != 1 || len(newRevision.Files) != 2 {
		t.Fatalf("unexpected append manifests: old=%+v new=%+v", oldRevision.Files, newRevision.Files)
	}
	if !oldRevision.MultiFile || !newRevision.MultiFile {
		t.Fatal("one-file multi-file torrent was confused with a single-file torrent")
	}
	if newRevision.Files[0] != oldRevision.Files[0] || newRevision.Files[1] != (File{Index: 1, Path: "Release/[02].mkv", Length: 4, Offset: 4}) {
		t.Fatalf("append offsets or prefix changed: old=%+v new=%+v", oldRevision.Files, newRevision.Files)
	}
	if newRevision.PieceHashes[0] != oldRevision.PieceHashes[0] || newRevision.TotalLength != 8 {
		t.Fatalf("append piece prefix changed: old=%x new=%x", oldRevision.PieceHashes, newRevision.PieceHashes)
	}
}

func TestParseHybridV1Layer(t *testing.T) {
	pieceLength := int64(16 << 10)
	piece := bytes.Repeat([]byte{0x31}, sha1.Size)
	root := bytes.Repeat([]byte{0x32}, sha256.Size)
	fileTree := bdict(bentry("[01].mkv", v2File(pieceLength, root)))
	info := bdict(
		bentry("file tree", fileTree),
		bentry("files", blist(fileEntry(pieceLength, "[01].mkv"))),
		bentry("meta version", bint(2)),
		bentry("name", bstr([]byte("Release"))),
		bentry("piece length", bint(pieceLength)),
		bentry("pieces", bstr(piece)),
	)
	parsed, err := Parse(torrent(info))
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.Hybrid || parsed.V2Only || len(parsed.Files) != 1 || parsed.Files[0].Path != "Release/[01].mkv" {
		t.Fatalf("hybrid v1 layer was not decoded: %+v", parsed)
	}
	if !parsed.Files[0].HasPiecesRoot || !bytes.Equal(parsed.Files[0].PiecesRoot[:], root) {
		t.Fatalf("hybrid v2 pieces root was not retained: %+v", parsed.Files[0])
	}
}

func TestParsePureV2FileTreeAndPieceLayers(t *testing.T) {
	pieceLength := int64(16 << 10)
	smallRoot := bytes.Repeat([]byte{0x51}, sha256.Size)
	firstPiece := bytes.Repeat([]byte{0x52}, sha256.Size)
	secondPiece := bytes.Repeat([]byte{0x53}, sha256.Size)
	layer := append(append([]byte(nil), firstPiece...), secondPiece...)
	largeRoot := sha256.Sum256(layer)
	fileTree := bdict(
		bentry("A.mkv", v2File(10, smallRoot)),
		bentry("dir", bdict(bentry("B.mkv", v2File(pieceLength*2, largeRoot[:])))),
	)
	info := v2Info("Release", pieceLength, fileTree)
	raw := torrentWithPieceLayers(info, bdict(bentry(string(largeRoot[:]), bstr(layer))))

	parsed, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.V2Only || parsed.Hybrid || parsed.Name != "Release" || parsed.PieceLength != pieceLength {
		t.Fatalf("unexpected pure-v2 metadata: %+v", parsed)
	}
	if parsed.PieceHashes != nil {
		t.Fatalf("pure-v2 torrent exposed v1 piece hashes: %x", parsed.PieceHashes)
	}
	if parsed.TotalLength != 10+pieceLength*2 || !parsed.MultiFile || len(parsed.Files) != 2 {
		t.Fatalf("unexpected pure-v2 manifest summary: %+v", parsed)
	}
	wantFiles := []File{
		{Index: 0, Path: "A.mkv", Length: 10, Offset: 0, PiecesRoot: bytesToSHA256(smallRoot), HasPiecesRoot: true},
		{Index: 1, Path: "dir/B.mkv", Length: pieceLength * 2, Offset: pieceLength, PiecesRoot: largeRoot, HasPiecesRoot: true},
	}
	for index := range wantFiles {
		if parsed.Files[index] != wantFiles[index] {
			t.Fatalf("file %d = %+v, want %+v", index, parsed.Files[index], wantFiles[index])
		}
	}
	if parsed.RawInfoSHA256 != sha256.Sum256(info) {
		t.Fatal("pure-v2 torrent identity was not calculated from the exact raw info dictionary")
	}
	if parsed.RawInfoSHA1 != sha1.Sum(info) {
		t.Fatal("pure-v2 SHA-1 artifact digest was not calculated from the exact raw info dictionary")
	}
}

func TestRejectsMalformedV2Metainfo(t *testing.T) {
	pieceLength := int64(16 << 10)
	root := bytes.Repeat([]byte{0x61}, sha256.Size)
	largeTree := bdict(bentry("large.mkv", v2File(pieceLength*2, root)))
	validTree := bdict(bentry("file.mkv", v2File(pieceLength, root)))
	properties := func(entries ...[]byte) []byte {
		return bdict(entries...)
	}
	node := func(entries ...[]byte) []byte {
		return bdict(bentry("", properties(entries...)))
	}

	tests := []struct {
		name string
		raw  []byte
		want string
	}{
		{name: "meta version is not two", raw: torrent(bdict(
			bentry("file tree", validTree),
			bentry("meta version", bint(1)),
			bentry("name", bstr([]byte("Release"))),
			bentry("piece length", bint(pieceLength)),
		)), want: "meta version"},
		{name: "piece length below minimum", raw: torrent(v2Info("Release", 8<<10, validTree)), want: "at least 16384"},
		{name: "piece length not power of two", raw: torrent(v2Info("Release", 24<<10, validTree)), want: "power of two"},
		{name: "empty file tree", raw: torrent(v2Info("Release", pieceLength, bdict())), want: "contains no files"},
		{name: "root is file", raw: torrent(v2Info("Release", pieceLength, v2File(pieceLength, root))), want: "root must not be a file"},
		{name: "file has sibling", raw: torrent(v2Info("Release", pieceLength, bdict(bentry("file", bdict(
			bentry("", properties(bentry("length", bint(pieceLength)), bentry("pieces root", bstr(root)))),
			bentry("child", v2File(pieceLength, root)),
		))))), want: "sibling child"},
		{name: "missing pieces root", raw: torrent(v2Info("Release", pieceLength, bdict(
			bentry("file.mkv", node(bentry("length", bint(pieceLength)))),
		))), want: "32-byte pieces root"},
		{name: "pieces root wrong size", raw: torrent(v2Info("Release", pieceLength, bdict(
			bentry("file.mkv", v2File(pieceLength, root[:31])),
		))), want: "32-byte pieces root"},
		{name: "zero length has pieces root", raw: torrent(v2Info("Release", pieceLength, bdict(
			bentry("file.mkv", v2File(0, root)),
		))), want: "zero-length file"},
		{name: "unsafe path", raw: torrent(v2Info("Release", pieceLength, bdict(
			bentry("..", v2File(pieceLength, root)),
		))), want: "unsafe path component"},
		{name: "case-folded duplicate path", raw: torrent(v2Info("Release", pieceLength, bdict(
			bentry("A.mkv", v2File(pieceLength, root)),
			bentry("a.mkv", v2File(pieceLength, root)),
		))), want: "differ only by case"},
		{name: "pad attribute", raw: torrent(v2Info("Release", pieceLength, bdict(
			bentry("file.mkv", node(
				bentry("attr", bstr([]byte("p"))),
				bentry("length", bint(pieceLength)),
				bentry("pieces root", bstr(root)),
			)),
		))), want: "pad file or symlink"},
		{name: "symlink path", raw: torrent(v2Info("Release", pieceLength, bdict(
			bentry("file.mkv", node(
				bentry("length", bint(pieceLength)),
				bentry("pieces root", bstr(root)),
				bentry("symlink path", blist(bstr([]byte("target.mkv")))),
			)),
		))), want: "declares a symlink"},
		{name: "missing piece layers", raw: torrent(v2Info("Release", pieceLength, largeTree)), want: "piece layers are required"},
		{name: "piece layer key wrong size", raw: torrentWithPieceLayers(
			v2Info("Release", pieceLength, largeTree),
			bdict(bentry("short", bstr(bytes.Repeat([]byte{0x71}, sha256.Size*2)))),
		), want: "32-byte pieces root"},
		{name: "piece layer hash count mismatch", raw: torrentWithPieceLayers(
			v2Info("Release", pieceLength, largeTree),
			bdict(bentry(string(root), bstr(bytes.Repeat([]byte{0x72}, sha256.Size)))),
		), want: "exactly 2"},
		{name: "piece layer root mismatch", raw: torrentWithPieceLayers(
			v2Info("Release", pieceLength, largeTree),
			bdict(bentry(string(root), bstr(bytes.Repeat([]byte{0x73}, sha256.Size*2)))),
		), want: "does not match"},
		{name: "unreferenced piece layer", raw: torrentWithPieceLayers(
			v2Info("Release", pieceLength, validTree),
			bdict(bentry(string(root), bstr(bytes.Repeat([]byte{0x74}, sha256.Size)))),
		), want: "not referenced"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse(test.raw)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Parse error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestRejectsMalformedAndUnsafeMetainfo(t *testing.T) {
	piece := bytes.Repeat([]byte{0x41}, sha1.Size)
	validSingle := func(extra ...[]byte) []byte {
		entries := [][]byte{
			bentry("length", bint(4)),
			bentry("name", bstr([]byte("file.mkv"))),
			bentry("piece length", bint(4)),
			bentry("pieces", bstr(piece)),
		}
		entries = append(entries, extra...)
		return torrent(bdict(entries...))
	}
	tests := []struct {
		name string
		raw  []byte
		want string
	}{
		{name: "empty", raw: nil, want: "empty"},
		{name: "trailing data", raw: append(validSingle(), 'x'), want: "trailing data"},
		{name: "root list", raw: []byte("le"), want: "root must be a dictionary"},
		{name: "missing info", raw: []byte("de"), want: "info dictionary"},
		{name: "unsorted dictionary", raw: []byte("d4:info" + string(bdict(
			bentry("pieces", bstr(piece)),
			bentry("name", bstr([]byte("file.mkv"))),
		)) + "e"), want: "unsorted"},
		{name: "duplicate dictionary key", raw: []byte("d4:infod6:lengthi4e6:lengthi4eee"), want: "duplicate or unsorted"},
		{name: "leading-zero integer", raw: []byte("d4:infod6:lengthi04e4:name8:file.mkv12:piece lengthi4e6:pieces20:" + string(piece) + "ee"), want: "leading zero"},
		{name: "pieces not hashes", raw: torrent(bdict(
			bentry("length", bint(4)),
			bentry("name", bstr([]byte("file.mkv"))),
			bentry("piece length", bint(4)),
			bentry("pieces", bstr([]byte("short"))),
		)), want: "20-byte"},
		{name: "piece count mismatch", raw: torrent(bdict(
			bentry("length", bint(8)),
			bentry("name", bstr([]byte("file.mkv"))),
			bentry("piece length", bint(4)),
			bentry("pieces", bstr(piece)),
		)), want: "expected 2"},
		{name: "single and multi layouts", raw: torrent(bdict(
			bentry("files", blist(fileEntry(4, "[01].mkv"))),
			bentry("length", bint(4)),
			bentry("name", bstr([]byte("Release"))),
			bentry("piece length", bint(4)),
			bentry("pieces", bstr(piece)),
		)), want: "exactly one"},
		{name: "traversal path", raw: torrent(multiInfo("Release", 4, piece, fileEntry(4, ".."))), want: "unsafe path component"},
		{name: "slash in component", raw: torrent(multiInfo("Release", 4, piece, fileEntry(4, "dir/file.mkv"))), want: "unsafe path component"},
		{name: "backslash in component", raw: torrent(multiInfo("Release", 4, piece, fileEntry(4, `dir\file.mkv`))), want: "unsafe path component"},
		{name: "invalid utf8 component", raw: torrent(multiInfo("Release", 4, piece, fileEntry(4, string([]byte{0xff})))), want: "valid UTF-8"},
		{name: "drive path", raw: torrent(multiInfo("C:", 4, piece, fileEntry(4, "file.mkv"))), want: "unsafe path"},
		{name: "single-file drive path", raw: torrent(bdict(
			bentry("length", bint(4)),
			bentry("name", bstr([]byte("C:episode.mkv"))),
			bentry("piece length", bint(4)),
			bentry("pieces", bstr(piece)),
		)), want: "unsafe path"},
		{name: "duplicate path", raw: torrent(multiInfo("Release", 8, piece,
			fileEntry(4, "[01].mkv"), fileEntry(4, "[01].mkv"),
		)), want: "duplicates"},
		{name: "case-folded duplicate path", raw: torrent(multiInfo("Release", 8, piece,
			fileEntry(4, "A.mkv"), fileEntry(4, "a.mkv"),
		)), want: "differ only by case"},
		{name: "pad attribute", raw: torrent(multiInfo("Release", 4, piece, fileEntryWithAttribute(4, "padding", "p"))), want: "pad file or symlink"},
		{name: "symlink attribute", raw: torrent(multiInfo("Release", 4, piece, fileEntryWithAttribute(4, "link.mkv", "l"))), want: "pad file or symlink"},
		{name: "symlink path", raw: torrent(multiInfo("Release", 4, piece, bdict(
			bentry("length", bint(4)),
			bentry("path", blist(bstr([]byte("link.mkv")))),
			bentry("symlink path", blist(bstr([]byte("target.mkv")))),
		))), want: "declares a symlink"},
		{name: "negative file length", raw: torrent(multiInfo("Release", 4, piece, fileEntry(-1, "file.mkv"))), want: "non-negative"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse(test.raw)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Parse error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestRejectsOversizedMetainfo(t *testing.T) {
	_, err := Parse(make([]byte, MaxMetadataSize+1))
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized Parse error = %v", err)
	}
}

func torrent(info []byte) []byte {
	return bdict(bentry("info", info))
}

func torrentWithPieceLayers(info, pieceLayers []byte) []byte {
	return bdict(
		bentry("info", info),
		bentry("piece layers", pieceLayers),
	)
}

func v2Info(name string, pieceLength int64, fileTree []byte) []byte {
	return bdict(
		bentry("file tree", fileTree),
		bentry("meta version", bint(2)),
		bentry("name", bstr([]byte(name))),
		bentry("piece length", bint(pieceLength)),
	)
}

func v2File(length int64, piecesRoot []byte) []byte {
	return bdict(bentry("", bdict(
		bentry("length", bint(length)),
		bentry("pieces root", bstr(piecesRoot)),
	)))
}

func bytesToSHA256(value []byte) [sha256.Size]byte {
	var result [sha256.Size]byte
	copy(result[:], value)
	return result
}

func multiInfo(name string, pieceLength int64, pieces []byte, files ...[]byte) []byte {
	return bdict(
		bentry("files", blist(files...)),
		bentry("name", bstr([]byte(name))),
		bentry("piece length", bint(pieceLength)),
		bentry("pieces", bstr(pieces)),
	)
}

func fileEntry(length int64, components ...string) []byte {
	pathValues := make([][]byte, 0, len(components))
	for _, component := range components {
		pathValues = append(pathValues, bstr([]byte(component)))
	}
	return bdict(
		bentry("length", bint(length)),
		bentry("path", blist(pathValues...)),
	)
}

func fileEntryWithAttribute(length int64, filePath, attribute string) []byte {
	return bdict(
		bentry("attr", bstr([]byte(attribute))),
		bentry("length", bint(length)),
		bentry("path", blist(bstr([]byte(filePath)))),
	)
}

func bdict(entries ...[]byte) []byte {
	result := []byte{'d'}
	for _, entry := range entries {
		result = append(result, entry...)
	}
	return append(result, 'e')
}

func blist(values ...[]byte) []byte {
	result := []byte{'l'}
	for _, value := range values {
		result = append(result, value...)
	}
	return append(result, 'e')
}

func bentry(key string, value []byte) []byte {
	return append(bstr([]byte(key)), value...)
}

func bstr(value []byte) []byte {
	return append([]byte(fmt.Sprintf("%d:", len(value))), value...)
}

func bint(value int64) []byte {
	return []byte(fmt.Sprintf("i%de", value))
}
