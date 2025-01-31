package content

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"io"
	"sort"
	"time"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/repo/blob"
)

const (
	packHeaderSize = 8
	deletedMarker  = 0x80000000

	entryFixedHeaderLength = 20
	randomSuffixSize       = 32
)

// FormatV1 describes a format of a single pack index. The actual structure is not used,
// it's purely for documentation purposes.
// The struct is byte-aligned.
type FormatV1 struct {
	Version    byte   // format version number must be 0x01
	KeySize    byte   // size of each key in bytes
	EntrySize  uint16 // size of each entry in bytes, big-endian
	EntryCount uint32 // number of sorted (key,value) entries that follow

	Entries []struct {
		Key   []byte // key bytes (KeySize)
		Entry indexEntryInfoV1
	}

	ExtraData []byte // extra data
}

type indexEntryInfoV1 struct {
	data      string // basically a byte array, but immutable
	contentID ID
	b         *indexV1
}

func (e indexEntryInfoV1) GetContentID() ID {
	return e.contentID
}

// entry bytes 0..5: 48-bit big-endian timestamp in seconds since 1970/01/01 UTC.
func (e indexEntryInfoV1) GetTimestampSeconds() int64 {
	return decodeBigEndianUint48(e.data)
}

// entry byte 6: format version (currently always == 1).
func (e indexEntryInfoV1) GetFormatVersion() byte {
	return e.data[6]
}

// entry byte 7: length of pack content ID
// entry bytes 8..11: 4 bytes, big endian, offset within index file where pack (blob) ID begins.
func (e indexEntryInfoV1) GetPackBlobID() blob.ID {
	nameLength := int(e.data[7])
	nameOffset := decodeBigEndianUint32(e.data[8:])

	var nameBuf [256]byte

	n, err := e.b.readerAt.ReadAt(nameBuf[0:nameLength], int64(nameOffset))
	if err != nil || n != nameLength {
		return "-invalid-blob-id-"
	}

	return blob.ID(nameBuf[0:nameLength])
}

// entry bytes 12..15 - deleted flag (MSBit), 31 lower bits encode pack offset.
func (e indexEntryInfoV1) GetDeleted() bool {
	return e.data[12]&0x80 != 0
}

func (e indexEntryInfoV1) GetPackOffset() uint32 {
	const packOffsetMask = 1<<31 - 1
	return decodeBigEndianUint32(e.data[12:]) & packOffsetMask
}

// bytes 16..19: 4 bytes, big endian, content length.
func (e indexEntryInfoV1) GetPackedLength() uint32 {
	return decodeBigEndianUint32(e.data[16:])
}

func (e indexEntryInfoV1) GetOriginalLength() uint32 {
	return e.GetPackedLength() - e.b.v1PerContentOverhead
}

func (e indexEntryInfoV1) Timestamp() time.Time {
	return time.Unix(e.GetTimestampSeconds(), 0)
}

var _ Info = indexEntryInfoV1{}

func decodeBigEndianUint48(d string) int64 {
	return int64(d[0])<<40 | int64(d[1])<<32 | int64(d[2])<<24 | int64(d[3])<<16 | int64(d[4])<<8 | int64(d[5])
}

func decodeBigEndianUint32(d string) uint32 {
	return uint32(d[0])<<24 | uint32(d[1])<<16 | uint32(d[2])<<8 | uint32(d[3])
}

type indexV1 struct {
	hdr      headerInfo
	readerAt io.ReaderAt
	// v1 index does not explicitly store per-content length so we compute it from packed length and fixed overhead
	// provided by the encryptor.
	v1PerContentOverhead uint32
}

func (b *indexV1) ApproximateCount() int {
	return b.hdr.entryCount
}

// Iterate invokes the provided callback function for a range of contents in the index, sorted alphabetically.
// The iteration ends when the callback returns an error, which is propagated to the caller or when
// all contents have been visited.
func (b *indexV1) Iterate(r IDRange, cb func(Info) error) error {
	startPos, err := b.findEntryPosition(r.StartID)
	if err != nil {
		return errors.Wrap(err, "could not find starting position")
	}

	stride := b.hdr.keySize + b.hdr.valueSize
	entry := make([]byte, stride)

	for i := startPos; i < b.hdr.entryCount; i++ {
		n, err := b.readerAt.ReadAt(entry, int64(packHeaderSize+stride*i))
		if err != nil || n != len(entry) {
			return errors.Wrap(err, "unable to read from index")
		}

		key := entry[0:b.hdr.keySize]

		contentID := bytesToContentID(key)
		if contentID >= r.EndID {
			break
		}

		i, err := b.entryToInfo(contentID, entry[b.hdr.keySize:])
		if err != nil {
			return errors.Wrap(err, "invalid index data")
		}

		if err := cb(i); err != nil {
			return err
		}
	}

	return nil
}

func (b *indexV1) findEntryPosition(contentID ID) (int, error) {
	stride := b.hdr.keySize + b.hdr.valueSize

	var entryArr [maxEntrySize]byte

	var entryBuf []byte

	if stride <= len(entryArr) {
		entryBuf = entryArr[0:stride]
	} else {
		entryBuf = make([]byte, stride)
	}

	var readErr error

	pos := sort.Search(b.hdr.entryCount, func(p int) bool {
		if readErr != nil {
			return false
		}
		_, err := b.readerAt.ReadAt(entryBuf, int64(packHeaderSize+stride*p))
		if err != nil {
			readErr = err
			return false
		}

		return bytesToContentID(entryBuf[0:b.hdr.keySize]) >= contentID
	})

	return pos, readErr
}

func (b *indexV1) findEntryPositionExact(idBytes, entryBuf []byte) (int, error) {
	stride := b.hdr.keySize + b.hdr.valueSize

	var readErr error

	pos := sort.Search(b.hdr.entryCount, func(p int) bool {
		if readErr != nil {
			return false
		}
		_, err := b.readerAt.ReadAt(entryBuf, int64(packHeaderSize+stride*p))
		if err != nil {
			readErr = err
			return false
		}

		return contentIDBytesGreaterOrEqual(entryBuf[0:b.hdr.keySize], idBytes)
	})

	return pos, readErr
}

func (b *indexV1) findEntry(output []byte, contentID ID) ([]byte, error) {
	var hashBuf [maxContentIDSize]byte

	key := contentIDToBytes(hashBuf[:0], contentID)

	// empty index blob, this is possible when compaction removes exactly everything
	if b.hdr.keySize == unknownKeySize {
		return nil, nil
	}

	if len(key) != b.hdr.keySize {
		return nil, errors.Errorf("invalid content ID: %q (%v vs %v)", contentID, len(key), b.hdr.keySize)
	}

	stride := b.hdr.keySize + b.hdr.valueSize

	var entryArr [maxEntrySize]byte

	var entryBuf []byte

	if stride <= len(entryArr) {
		entryBuf = entryArr[0:stride]
	} else {
		entryBuf = make([]byte, stride)
	}

	position, err := b.findEntryPositionExact(key, entryBuf)
	if err != nil {
		return nil, err
	}

	if position >= b.hdr.entryCount {
		return nil, nil
	}

	if _, err := b.readerAt.ReadAt(entryBuf, int64(packHeaderSize+stride*position)); err != nil {
		return nil, errors.Wrap(err, "error reading header")
	}

	if bytes.Equal(entryBuf[0:len(key)], key) {
		return append(output, entryBuf[len(key):]...), nil
	}

	return nil, nil
}

// GetInfo returns information about a given content. If a content is not found, nil is returned.
func (b *indexV1) GetInfo(contentID ID) (Info, error) {
	var entryBuf [maxEntrySize]byte

	e, err := b.findEntry(entryBuf[:0], contentID)
	if err != nil {
		return nil, err
	}

	if e == nil {
		return nil, nil
	}

	return b.entryToInfo(contentID, e)
}

func (b *indexV1) entryToInfo(contentID ID, entryData []byte) (Info, error) {
	if len(entryData) < entryFixedHeaderLength {
		return nil, errors.Errorf("invalid entry length: %v", len(entryData))
	}

	// convert to 'entryData' string to make it read-only
	return indexEntryInfoV1{string(entryData), contentID, b}, nil
}

// Close closes the index and the underlying reader.
func (b *indexV1) Close() error {
	if closer, ok := b.readerAt.(io.Closer); ok {
		return closer.Close()
	}

	return nil
}

type indexBuilderV1 struct {
	packBlobIDOffsets map[blob.ID]uint32
	entryCount        int
	keyLength         int
	entryLength       int
	extraDataOffset   uint32
}

// buildV1 writes the pack index to the provided output.
func (b packIndexBuilder) buildV1(output io.Writer) error {
	allContents := b.sortedContents()
	b1 := &indexBuilderV1{
		packBlobIDOffsets: map[blob.ID]uint32{},
		keyLength:         -1,
		entryLength:       entryFixedHeaderLength,
		entryCount:        len(allContents),
	}

	w := bufio.NewWriter(output)

	// prepare extra data to be appended at the end of an index.
	extraData := b1.prepareExtraData(allContents)

	// write header
	header := make([]byte, packHeaderSize)
	header[0] = 1 // version
	header[1] = byte(b1.keyLength)
	binary.BigEndian.PutUint16(header[2:4], uint16(b1.entryLength))
	binary.BigEndian.PutUint32(header[4:8], uint32(b1.entryCount))

	if _, err := w.Write(header); err != nil {
		return errors.Wrap(err, "unable to write header")
	}

	// write all sorted contents.
	entry := make([]byte, b1.entryLength)

	for _, it := range allContents {
		if err := b1.writeEntry(w, it, entry); err != nil {
			return errors.Wrap(err, "unable to write entry")
		}
	}

	if _, err := w.Write(extraData); err != nil {
		return errors.Wrap(err, "error writing extra data")
	}

	randomSuffix := make([]byte, randomSuffixSize)
	if _, err := rand.Read(randomSuffix); err != nil {
		return errors.Wrap(err, "error getting random bytes for suffix")
	}

	if _, err := w.Write(randomSuffix); err != nil {
		return errors.Wrap(err, "error writing extra random suffix to ensure indexes are always globally unique")
	}

	return w.Flush()
}

func (b *indexBuilderV1) prepareExtraData(allContents []Info) []byte {
	var extraData []byte

	var hashBuf [maxContentIDSize]byte

	for i, it := range allContents {
		if i == 0 {
			b.keyLength = len(contentIDToBytes(hashBuf[:0], it.GetContentID()))
		}

		if it.GetPackBlobID() != "" {
			if _, ok := b.packBlobIDOffsets[it.GetPackBlobID()]; !ok {
				b.packBlobIDOffsets[it.GetPackBlobID()] = uint32(len(extraData))
				extraData = append(extraData, []byte(it.GetPackBlobID())...)
			}
		}
	}

	b.extraDataOffset = uint32(packHeaderSize + b.entryCount*(b.keyLength+b.entryLength))

	return extraData
}

func (b *indexBuilderV1) writeEntry(w io.Writer, it Info, entry []byte) error {
	var hashBuf [maxContentIDSize]byte

	k := contentIDToBytes(hashBuf[:0], it.GetContentID())

	if len(k) != b.keyLength {
		return errors.Errorf("inconsistent key length: %v vs %v", len(k), b.keyLength)
	}

	if err := b.formatEntry(entry, it); err != nil {
		return errors.Wrap(err, "unable to format entry")
	}

	if _, err := w.Write(k); err != nil {
		return errors.Wrap(err, "error writing entry key")
	}

	if _, err := w.Write(entry); err != nil {
		return errors.Wrap(err, "error writing entry")
	}

	return nil
}

func (b *indexBuilderV1) formatEntry(entry []byte, it Info) error {
	entryTimestampAndFlags := entry[0:8]
	entryPackFileOffset := entry[8:12]
	entryPackedOffset := entry[12:16]
	entryPackedLength := entry[16:20]
	timestampAndFlags := uint64(it.GetTimestampSeconds()) << 16 // nolint:gomnd

	packBlobID := it.GetPackBlobID()
	if len(packBlobID) == 0 {
		return errors.Errorf("empty pack content ID for %v", it.GetContentID())
	}

	binary.BigEndian.PutUint32(entryPackFileOffset, b.extraDataOffset+b.packBlobIDOffsets[packBlobID])

	if it.GetDeleted() {
		binary.BigEndian.PutUint32(entryPackedOffset, it.GetPackOffset()|deletedMarker)
	} else {
		binary.BigEndian.PutUint32(entryPackedOffset, it.GetPackOffset())
	}

	binary.BigEndian.PutUint32(entryPackedLength, it.GetPackedLength())
	timestampAndFlags |= uint64(it.GetFormatVersion()) << 8 // nolint:gomnd
	timestampAndFlags |= uint64(len(packBlobID))
	binary.BigEndian.PutUint64(entryTimestampAndFlags, timestampAndFlags)

	return nil
}
