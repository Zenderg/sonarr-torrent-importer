// Package metainfo parses the immutable, safety-relevant subset of BitTorrent
// metainfo used to compare rolling torrent revisions.
package metainfo

import (
	"crypto/sha1"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"path"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	MaxMetadataSize = 16 << 20
	maxDepth        = 32
	maxNodes        = 200_000
	maxFiles        = 100_000
	maxPathBytes    = 4096
	maxComponent    = 255
)

// MetaInfo is a validated v1, v2, or hybrid file and piece layout. For v2-only
// torrents RawInfoSHA256 is the torrent identity; RawInfoSHA1 remains an
// artifact digest and must not be used as an identity.
type MetaInfo struct {
	Name          string
	RawInfoSHA1   [sha1.Size]byte
	RawInfoSHA256 [sha256.Size]byte
	PieceLength   int64
	PieceHashes   [][sha1.Size]byte
	Files         []File
	TotalLength   int64
	MultiFile     bool
	Hybrid        bool
	V2Only        bool
}

// File describes one file in qBittorrent manifest order. Offset is its byte
// offset in the v1 concatenated payload or the BEP 52 aligned piece space.
type File struct {
	Index         int
	Path          string
	Length        int64
	Offset        int64
	PiecesRoot    [sha256.Size]byte
	HasPiecesRoot bool
}

// Parse validates and decodes one complete .torrent file.
func Parse(raw []byte) (MetaInfo, error) {
	if len(raw) == 0 {
		return MetaInfo{}, errors.New("metainfo is empty")
	}
	if len(raw) > MaxMetadataSize {
		return MetaInfo{}, fmt.Errorf("metainfo exceeds %d bytes", MaxMetadataSize)
	}

	decoder := bdecoder{raw: raw}
	top, err := decoder.parse(0)
	if err != nil {
		return MetaInfo{}, fmt.Errorf("decode metainfo: %w", err)
	}
	if decoder.offset != len(raw) {
		return MetaInfo{}, fmt.Errorf("decode metainfo: trailing data at byte %d", decoder.offset)
	}
	if top.kind != kindDictionary {
		return MetaInfo{}, errors.New("metainfo root must be a dictionary")
	}
	info, found := top.dictionary["info"]
	if !found || info.kind != kindDictionary {
		return MetaInfo{}, errors.New("metainfo must contain an info dictionary")
	}

	parsed, err := parseInfo(top, info)
	if err != nil {
		return MetaInfo{}, err
	}
	rawInfo := raw[info.start:info.end]
	parsed.RawInfoSHA1 = sha1.Sum(rawInfo)
	parsed.RawInfoSHA256 = sha256.Sum256(rawInfo)
	return parsed, nil
}

func parseInfo(top, info value) (MetaInfo, error) {
	nameValue, ok := info.dictionary["name"]
	if !ok || nameValue.kind != kindBytes {
		return MetaInfo{}, errors.New("info.name must be a byte string")
	}
	name, err := safePathComponent(nameValue.bytes, "info.name")
	if err != nil {
		return MetaInfo{}, err
	}

	pieceLengthValue, ok := info.dictionary["piece length"]
	if !ok || pieceLengthValue.kind != kindInteger || pieceLengthValue.integer <= 0 {
		return MetaInfo{}, errors.New("info.piece length must be a positive integer")
	}

	metaVersion, hasMetaVersion := info.dictionary["meta version"]
	if hasMetaVersion {
		if metaVersion.kind != kindInteger || metaVersion.integer != 2 {
			return MetaInfo{}, errors.New("info.meta version must be integer 2 when present")
		}
		if pieceLengthValue.integer < 16<<10 || pieceLengthValue.integer&(pieceLengthValue.integer-1) != 0 {
			return MetaInfo{}, errors.New("v2 info.piece length must be a power of two and at least 16384")
		}
		if fileTree, ok := info.dictionary["file tree"]; !ok || fileTree.kind != kindDictionary {
			return MetaInfo{}, errors.New("v2 torrent must contain an info.file tree dictionary")
		}
	} else if _, hasFileTree := info.dictionary["file tree"]; hasFileTree {
		return MetaInfo{}, errors.New("info.file tree requires meta version 2")
	}

	if err := rejectUnsafeAttributes(info, "info"); err != nil {
		return MetaInfo{}, err
	}

	piecesValue, hasPieces := info.dictionary["pieces"]
	if !hasMetaVersion && !hasPieces {
		return MetaInfo{}, errors.New("info.pieces is required for a v1 torrent")
	}
	if hasPieces && (piecesValue.kind != kindBytes || len(piecesValue.bytes) == 0 || len(piecesValue.bytes)%sha1.Size != 0) {
		return MetaInfo{}, errors.New("info.pieces must contain one or more ordered 20-byte hashes")
	}

	lengthValue, hasLength := info.dictionary["length"]
	filesValue, hasFiles := info.dictionary["files"]
	hasV1Layout := hasLength != hasFiles
	if hasPieces && !hasV1Layout {
		return MetaInfo{}, errors.New("v1 or hybrid info must contain exactly one of length or files")
	}
	if !hasPieces && (hasLength || hasFiles) {
		return MetaInfo{}, errors.New("v2-only info must not contain an incomplete v1 length or files layout")
	}

	var (
		files       []File
		total       int64
		multiFile   bool
		pieceHashes [][sha1.Size]byte
	)
	if hasPieces {
		v1Files, v1Total, err := parseV1Layout(name, lengthValue, hasLength, filesValue)
		if err != nil {
			return MetaInfo{}, err
		}
		pieceHashes, err = parseV1PieceHashes(piecesValue, v1Total, pieceLengthValue.integer)
		if err != nil {
			return MetaInfo{}, err
		}
		files, total, multiFile = v1Files, v1Total, !hasLength
	}

	if hasMetaVersion {
		v2Files, v2Total, v2MultiFile, err := parseV2FileTree(info.dictionary["file tree"], pieceLengthValue.integer)
		if err != nil {
			return MetaInfo{}, err
		}
		if err := validateV2PieceLayers(top, v2Files, pieceLengthValue.integer); err != nil {
			return MetaInfo{}, err
		}
		if hasPieces {
			if err := validateHybridLayouts(name, multiFile, files, v2Files); err != nil {
				return MetaInfo{}, err
			}
			for index := range files {
				files[index].PiecesRoot = v2Files[index].PiecesRoot
				files[index].HasPiecesRoot = v2Files[index].HasPiecesRoot
			}
		} else {
			files, total, multiFile = v2Files, v2Total, v2MultiFile
		}
	} else if _, hasPieceLayers := top.dictionary["piece layers"]; hasPieceLayers {
		return MetaInfo{}, errors.New("piece layers require a v2 or hybrid torrent")
	}

	return MetaInfo{
		Name: name, PieceLength: pieceLengthValue.integer, PieceHashes: pieceHashes,
		Files: files, TotalLength: total, MultiFile: multiFile,
		Hybrid: hasMetaVersion && hasPieces, V2Only: hasMetaVersion && !hasPieces,
	}, nil
}

func parseV1Layout(name string, lengthValue value, hasLength bool, filesValue value) ([]File, int64, error) {
	if hasLength {
		if lengthValue.kind != kindInteger || lengthValue.integer < 0 {
			return nil, 0, errors.New("info.length must be a non-negative integer")
		}
		if drivePath(name) {
			return nil, 0, fmt.Errorf("info.name produces unsafe path %q", name)
		}
		if lengthValue.integer == 0 {
			return nil, 0, errors.New("torrent payload must contain at least one byte")
		}
		return []File{{Index: 0, Path: name, Length: lengthValue.integer}}, lengthValue.integer, nil
	}
	if filesValue.kind != kindList || len(filesValue.list) == 0 {
		return nil, 0, errors.New("info.files must be a non-empty list")
	}
	if len(filesValue.list) > maxFiles {
		return nil, 0, fmt.Errorf("info.files exceeds %d entries", maxFiles)
	}
	files := make([]File, 0, len(filesValue.list))
	var offset int64
	seen := make(map[string]string, len(filesValue.list))
	for index, item := range filesValue.list {
		file, err := parseV1File(item, name, index, offset)
		if err != nil {
			return nil, 0, err
		}
		if err := recordPath(seen, file.Path); err != nil {
			return nil, 0, err
		}
		files = append(files, file)
		if file.Length > math.MaxInt64-offset {
			return nil, 0, errors.New("torrent total length overflows int64")
		}
		offset += file.Length
	}
	if offset <= 0 {
		return nil, 0, errors.New("torrent payload must contain at least one byte")
	}
	return files, offset, nil
}

func parseV1PieceHashes(piecesValue value, total, pieceLength int64) ([][sha1.Size]byte, error) {
	expectedPieces := total / pieceLength
	if total%pieceLength != 0 {
		expectedPieces++
	}
	actualPieces := int64(len(piecesValue.bytes) / sha1.Size)
	if actualPieces != expectedPieces {
		return nil, fmt.Errorf("info.pieces contains %d hashes, expected %d for payload length %d", actualPieces, expectedPieces, total)
	}
	pieceHashes := make([][sha1.Size]byte, actualPieces)
	for index := range pieceHashes {
		copy(pieceHashes[index][:], piecesValue.bytes[index*sha1.Size:(index+1)*sha1.Size])
	}
	return pieceHashes, nil
}

func parseV1File(item value, root string, index int, offset int64) (File, error) {
	if item.kind != kindDictionary {
		return File{}, fmt.Errorf("info.files[%d] must be a dictionary", index)
	}
	if err := rejectUnsafeAttributes(item, fmt.Sprintf("info.files[%d]", index)); err != nil {
		return File{}, err
	}
	length, ok := item.dictionary["length"]
	if !ok || length.kind != kindInteger || length.integer < 0 {
		return File{}, fmt.Errorf("info.files[%d].length must be a non-negative integer", index)
	}
	pathValue, ok := item.dictionary["path"]
	if !ok || pathValue.kind != kindList || len(pathValue.list) == 0 {
		return File{}, fmt.Errorf("info.files[%d].path must be a non-empty list", index)
	}
	components := make([]string, 0, len(pathValue.list)+1)
	components = append(components, root)
	for componentIndex, componentValue := range pathValue.list {
		if componentValue.kind != kindBytes {
			return File{}, fmt.Errorf("info.files[%d].path[%d] must be a byte string", index, componentIndex)
		}
		component, err := safePathComponent(componentValue.bytes, fmt.Sprintf("info.files[%d].path[%d]", index, componentIndex))
		if err != nil {
			return File{}, err
		}
		components = append(components, component)
	}
	filePath := strings.Join(components, "/")
	if len(filePath) > maxPathBytes || path.Clean(filePath) != filePath || drivePath(filePath) {
		return File{}, fmt.Errorf("info.files[%d] produces unsafe path %q", index, filePath)
	}
	return File{Index: index, Path: filePath, Length: length.integer, Offset: offset}, nil
}

func parseV2FileTree(tree value, pieceLength int64) ([]File, int64, bool, error) {
	if tree.kind != kindDictionary {
		return nil, 0, false, errors.New("info.file tree must be a dictionary")
	}
	if _, rootIsFile := tree.dictionary[""]; rootIsFile {
		return nil, 0, false, errors.New("info.file tree root must not be a file")
	}
	files := make([]File, 0)
	if err := walkV2Tree(tree, nil, &files); err != nil {
		return nil, 0, false, err
	}
	if len(files) == 0 {
		return nil, 0, false, errors.New("info.file tree contains no files")
	}
	if len(files) > maxFiles {
		return nil, 0, false, fmt.Errorf("info.file tree exceeds %d files", maxFiles)
	}

	seen := make(map[string]string, len(files))
	var payloadLength, pieceOffset int64
	for index := range files {
		files[index].Index = index
		if err := recordPath(seen, files[index].Path); err != nil {
			return nil, 0, false, err
		}
		if files[index].Length > 0 {
			aligned, err := alignPieceOffset(pieceOffset, pieceLength)
			if err != nil {
				return nil, 0, false, err
			}
			pieceOffset = aligned
		}
		files[index].Offset = pieceOffset
		if files[index].Length > math.MaxInt64-pieceOffset || files[index].Length > math.MaxInt64-payloadLength {
			return nil, 0, false, errors.New("torrent length or v2 piece offset overflows int64")
		}
		pieceOffset += files[index].Length
		payloadLength += files[index].Length
	}
	if payloadLength <= 0 {
		return nil, 0, false, errors.New("torrent payload must contain at least one byte")
	}
	multiFile := len(files) != 1 || strings.Contains(files[0].Path, "/")
	return files, payloadLength, multiFile, nil
}

func walkV2Tree(node value, components []string, files *[]File) error {
	if node.kind != kindDictionary {
		return errors.New("info.file tree child must be a dictionary")
	}
	if properties, isFile := node.dictionary[""]; isFile {
		if len(components) == 0 {
			return errors.New("info.file tree root must not be a file")
		}
		if len(node.dictionary) != 1 {
			return fmt.Errorf("v2 file %q has sibling child entries", strings.Join(components, "/"))
		}
		file, err := parseV2FileProperties(properties, components)
		if err != nil {
			return err
		}
		*files = append(*files, file)
		if len(*files) > maxFiles {
			return fmt.Errorf("info.file tree exceeds %d files", maxFiles)
		}
		return nil
	}
	if len(node.dictionary) == 0 && len(components) > 0 {
		return fmt.Errorf("v2 directory %q is empty", strings.Join(components, "/"))
	}
	keys := make([]string, 0, len(node.dictionary))
	for key := range node.dictionary {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		component, err := safePathComponent([]byte(key), "info.file tree path")
		if err != nil {
			return err
		}
		next := append(append([]string(nil), components...), component)
		if err := walkV2Tree(node.dictionary[key], next, files); err != nil {
			return err
		}
	}
	return nil
}

func parseV2FileProperties(properties value, components []string) (File, error) {
	field := fmt.Sprintf("info.file tree file %q", strings.Join(components, "/"))
	if properties.kind != kindDictionary {
		return File{}, fmt.Errorf("%s properties must be a dictionary", field)
	}
	if err := rejectUnsafeAttributes(properties, field); err != nil {
		return File{}, err
	}
	length, ok := properties.dictionary["length"]
	if !ok || length.kind != kindInteger || length.integer < 0 {
		return File{}, fmt.Errorf("%s length must be a non-negative integer", field)
	}
	filePath := strings.Join(components, "/")
	if len(filePath) > maxPathBytes || path.Clean(filePath) != filePath || drivePath(filePath) {
		return File{}, fmt.Errorf("%s produces unsafe path %q", field, filePath)
	}
	rootValue, hasRoot := properties.dictionary["pieces root"]
	if length.integer == 0 {
		if hasRoot {
			return File{}, fmt.Errorf("%s zero-length file must not have a pieces root", field)
		}
		return File{Path: filePath}, nil
	}
	if !hasRoot || rootValue.kind != kindBytes || len(rootValue.bytes) != sha256.Size {
		return File{}, fmt.Errorf("%s non-empty file must have a 32-byte pieces root", field)
	}
	file := File{Path: filePath, Length: length.integer, HasPiecesRoot: true}
	copy(file.PiecesRoot[:], rootValue.bytes)
	return file, nil
}

func validateHybridLayouts(name string, v1MultiFile bool, v1, v2 []File) error {
	if len(v1) != len(v2) {
		return fmt.Errorf("hybrid v1 and v2 file counts differ: %d and %d", len(v1), len(v2))
	}
	for index := range v1 {
		v1Path := v1[index].Path
		if v1MultiFile {
			v1Path = strings.TrimPrefix(v1Path, name+"/")
		}
		if v1Path != v2[index].Path || v1[index].Length != v2[index].Length || v1[index].Offset != v2[index].Offset {
			return fmt.Errorf("hybrid v1 and v2 layouts differ at file %d", index)
		}
	}
	return nil
}

func validateV2PieceLayers(top value, files []File, pieceLength int64) error {
	required := make(map[[sha256.Size]byte]int64)
	for _, file := range files {
		if file.Length <= pieceLength {
			continue
		}
		pieceCount := file.Length / pieceLength
		if file.Length%pieceLength != 0 {
			pieceCount++
		}
		if previous, exists := required[file.PiecesRoot]; exists && previous != pieceCount {
			return fmt.Errorf("pieces root %x is reused with incompatible piece counts", file.PiecesRoot)
		}
		required[file.PiecesRoot] = pieceCount
	}

	layers, hasLayers := top.dictionary["piece layers"]
	if !hasLayers {
		if len(required) > 0 {
			return errors.New("piece layers are required for v2 files larger than piece length")
		}
		return nil
	}
	if layers.kind != kindDictionary {
		return errors.New("piece layers must be a dictionary")
	}
	seen := make(map[[sha256.Size]byte]struct{}, len(layers.dictionary))
	for rawRoot, layer := range layers.dictionary {
		if len(rawRoot) != sha256.Size {
			return errors.New("piece layer key must be a 32-byte pieces root")
		}
		var root [sha256.Size]byte
		copy(root[:], rawRoot)
		pieceCount, expected := required[root]
		if !expected {
			return fmt.Errorf("piece layer %x is not referenced by a large file", root)
		}
		if layer.kind != kindBytes || len(layer.bytes)%sha256.Size != 0 || int64(len(layer.bytes)/sha256.Size) != pieceCount {
			return fmt.Errorf("piece layer %x must contain exactly %d ordered 32-byte hashes", root, pieceCount)
		}
		if calculated := pieceLayerRoot(layer.bytes, pieceLength); calculated != root {
			return fmt.Errorf("piece layer %x does not match its pieces root", root)
		}
		seen[root] = struct{}{}
	}
	for root := range required {
		if _, exists := seen[root]; !exists {
			return fmt.Errorf("piece layer for pieces root %x is missing", root)
		}
	}
	return nil
}

func pieceLayerRoot(encoded []byte, pieceLength int64) [sha256.Size]byte {
	hashes := make([][sha256.Size]byte, len(encoded)/sha256.Size)
	for index := range hashes {
		copy(hashes[index][:], encoded[index*sha256.Size:(index+1)*sha256.Size])
	}
	width := 1
	for width < len(hashes) {
		width *= 2
	}
	zero := zeroHashForPieceLength(pieceLength)
	for len(hashes) < width {
		hashes = append(hashes, zero)
	}
	for len(hashes) > 1 {
		parents := make([][sha256.Size]byte, len(hashes)/2)
		for index := range parents {
			var pair [sha256.Size * 2]byte
			copy(pair[:sha256.Size], hashes[index*2][:])
			copy(pair[sha256.Size:], hashes[index*2+1][:])
			parents[index] = sha256.Sum256(pair[:])
		}
		hashes = parents
	}
	return hashes[0]
}

func zeroHashForPieceLength(pieceLength int64) [sha256.Size]byte {
	var zero [sha256.Size]byte
	for blockSize := int64(16 << 10); blockSize < pieceLength; blockSize *= 2 {
		var pair [sha256.Size * 2]byte
		copy(pair[:sha256.Size], zero[:])
		copy(pair[sha256.Size:], zero[:])
		zero = sha256.Sum256(pair[:])
	}
	return zero
}

func alignPieceOffset(offset, pieceLength int64) (int64, error) {
	remainder := offset % pieceLength
	if remainder == 0 {
		return offset, nil
	}
	padding := pieceLength - remainder
	if padding > math.MaxInt64-offset {
		return 0, errors.New("v2 piece alignment overflows int64")
	}
	return offset + padding, nil
}

func recordPath(seen map[string]string, filePath string) error {
	folded := strings.ToLower(filePath)
	if previous, exists := seen[folded]; exists {
		return fmt.Errorf("torrent paths %q and %q are duplicates or differ only by case", previous, filePath)
	}
	seen[folded] = filePath
	return nil
}

func rejectUnsafeAttributes(dictionary value, field string) error {
	if _, exists := dictionary.dictionary["symlink path"]; exists {
		return fmt.Errorf("%s declares a symlink", field)
	}
	attribute, exists := dictionary.dictionary["attr"]
	if !exists {
		return nil
	}
	if attribute.kind != kindBytes {
		return fmt.Errorf("%s.attr must be a byte string", field)
	}
	if strings.ContainsAny(string(attribute.bytes), "pl") {
		return fmt.Errorf("%s declares a pad file or symlink", field)
	}
	return nil
}

func safePathComponent(raw []byte, field string) (string, error) {
	if len(raw) == 0 || len(raw) > maxComponent || !utf8.Valid(raw) {
		return "", fmt.Errorf("%s is not a valid UTF-8 path component", field)
	}
	component := string(raw)
	if component == "." || component == ".." || strings.ContainsAny(component, `/\`) {
		return "", fmt.Errorf("%s contains an unsafe path component", field)
	}
	for _, character := range component {
		if unicode.IsControl(character) {
			return "", fmt.Errorf("%s contains a control character", field)
		}
	}
	return component, nil
}

func drivePath(value string) bool {
	return len(value) >= 2 && ((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) && value[1] == ':'
}

type valueKind byte

const (
	kindInteger valueKind = iota + 1
	kindBytes
	kindList
	kindDictionary
)

type value struct {
	kind       valueKind
	integer    int64
	bytes      []byte
	list       []value
	dictionary map[string]value
	start      int
	end        int
}

type bdecoder struct {
	raw    []byte
	offset int
	nodes  int
}

func (d *bdecoder) parse(depth int) (value, error) {
	if depth > maxDepth {
		return value{}, fmt.Errorf("nesting exceeds %d levels", maxDepth)
	}
	if d.offset >= len(d.raw) {
		return value{}, errors.New("unexpected end of input")
	}
	d.nodes++
	if d.nodes > maxNodes {
		return value{}, fmt.Errorf("value count exceeds %d", maxNodes)
	}
	start := d.offset
	switch d.raw[d.offset] {
	case 'i':
		return d.parseInteger(start)
	case 'l':
		return d.parseList(start, depth)
	case 'd':
		return d.parseDictionary(start, depth)
	default:
		if d.raw[d.offset] < '0' || d.raw[d.offset] > '9' {
			return value{}, fmt.Errorf("invalid token %q at byte %d", d.raw[d.offset], d.offset)
		}
		return d.parseBytes(start)
	}
}

func (d *bdecoder) parseInteger(start int) (value, error) {
	d.offset++
	numberStart := d.offset
	for d.offset < len(d.raw) && d.raw[d.offset] != 'e' {
		d.offset++
	}
	if d.offset >= len(d.raw) {
		return value{}, errors.New("unterminated integer")
	}
	token := d.raw[numberStart:d.offset]
	integer, err := strictInt(token)
	if err != nil {
		return value{}, fmt.Errorf("invalid integer at byte %d: %w", start, err)
	}
	d.offset++
	return value{kind: kindInteger, integer: integer, start: start, end: d.offset}, nil
}

func (d *bdecoder) parseBytes(start int) (value, error) {
	lengthStart := d.offset
	for d.offset < len(d.raw) && d.raw[d.offset] >= '0' && d.raw[d.offset] <= '9' {
		d.offset++
	}
	if d.offset >= len(d.raw) || d.raw[d.offset] != ':' {
		return value{}, fmt.Errorf("invalid byte string length at byte %d", start)
	}
	lengthToken := d.raw[lengthStart:d.offset]
	if len(lengthToken) > 1 && lengthToken[0] == '0' {
		return value{}, fmt.Errorf("byte string length has a leading zero at byte %d", start)
	}
	length, err := strictUnsigned(lengthToken)
	if err != nil {
		return value{}, fmt.Errorf("invalid byte string length at byte %d: %w", start, err)
	}
	d.offset++
	if length > uint64(len(d.raw)-d.offset) {
		return value{}, fmt.Errorf("byte string at byte %d exceeds input", start)
	}
	end := d.offset + int(length)
	bytesValue := d.raw[d.offset:end]
	d.offset = end
	return value{kind: kindBytes, bytes: bytesValue, start: start, end: end}, nil
}

func (d *bdecoder) parseList(start, depth int) (value, error) {
	d.offset++
	items := make([]value, 0)
	for {
		if d.offset >= len(d.raw) {
			return value{}, errors.New("unterminated list")
		}
		if d.raw[d.offset] == 'e' {
			d.offset++
			return value{kind: kindList, list: items, start: start, end: d.offset}, nil
		}
		item, err := d.parse(depth + 1)
		if err != nil {
			return value{}, err
		}
		items = append(items, item)
	}
}

func (d *bdecoder) parseDictionary(start, depth int) (value, error) {
	d.offset++
	items := make(map[string]value)
	var previous []byte
	for {
		if d.offset >= len(d.raw) {
			return value{}, errors.New("unterminated dictionary")
		}
		if d.raw[d.offset] == 'e' {
			d.offset++
			return value{kind: kindDictionary, dictionary: items, start: start, end: d.offset}, nil
		}
		if d.raw[d.offset] < '0' || d.raw[d.offset] > '9' {
			return value{}, fmt.Errorf("dictionary key at byte %d is not a byte string", d.offset)
		}
		keyValue, err := d.parseBytes(d.offset)
		if err != nil {
			return value{}, err
		}
		if previous != nil && string(previous) >= string(keyValue.bytes) {
			return value{}, fmt.Errorf("dictionary keys are duplicate or unsorted at %q", keyValue.bytes)
		}
		previous = keyValue.bytes
		item, err := d.parse(depth + 1)
		if err != nil {
			return value{}, err
		}
		items[string(keyValue.bytes)] = item
	}
}

func strictInt(token []byte) (int64, error) {
	if len(token) == 0 {
		return 0, errors.New("empty integer")
	}
	negative := token[0] == '-'
	digits := token
	if negative {
		digits = token[1:]
		if len(digits) == 0 {
			return 0, errors.New("missing integer digits")
		}
	}
	if len(digits) > 1 && digits[0] == '0' {
		return 0, errors.New("leading zero")
	}
	if negative && len(digits) == 1 && digits[0] == '0' {
		return 0, errors.New("negative zero")
	}
	unsigned, err := strictUnsigned(digits)
	if err != nil {
		return 0, err
	}
	if negative {
		if unsigned > uint64(math.MaxInt64)+1 {
			return 0, errors.New("integer underflow")
		}
		if unsigned == uint64(math.MaxInt64)+1 {
			return math.MinInt64, nil
		}
		return -int64(unsigned), nil
	}
	if unsigned > math.MaxInt64 {
		return 0, errors.New("integer overflow")
	}
	return int64(unsigned), nil
}

func strictUnsigned(token []byte) (uint64, error) {
	if len(token) == 0 {
		return 0, errors.New("empty number")
	}
	var value uint64
	for _, digit := range token {
		if digit < '0' || digit > '9' {
			return 0, errors.New("non-decimal digit")
		}
		next := uint64(digit - '0')
		if value > (math.MaxUint64-next)/10 {
			return 0, errors.New("number overflow")
		}
		value = value*10 + next
	}
	return value, nil
}
