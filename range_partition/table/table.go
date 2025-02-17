package table

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/DataDog/zstd"
	"github.com/dgraph-io/ristretto"
	"github.com/dgraph-io/ristretto/z"
	"github.com/gogo/protobuf/proto"
	"github.com/journeymidnight/autumn/proto/pspb"
	"github.com/journeymidnight/autumn/range_partition/y"
	"github.com/journeymidnight/autumn/streamclient"
	"github.com/journeymidnight/autumn/utils"
	"github.com/journeymidnight/autumn/xlog"
	"github.com/klauspost/compress/snappy" //snappy use S2 compression
	"github.com/pkg/errors"
)

// TableInterface is useful for testing.
type TableInterface interface {
	Smallest() []byte
	Biggest() []byte
	DoesNotHave(hash uint64) bool
}

type Table struct {
	utils.SafeMutex
	streamReader streamclient.StreamClient //only to read
	blockIndex   []*pspb.BlockOffset

	// The following are initialized once and const.
	smallest, biggest []byte // Smallest and largest keys (with timestamps).

	// Stores the total size of key-values in skiplist.
	EstimatedSize uint64
	bf            *z.Bloom
	//cache ??

	Loc     pspb.Location //saved address in rowStream
	LastSeq uint64
	//all data before [vpExtentID, vpOffset] is in rowStream. log replay starts from [vpExtentID, vpOffset]
	VpExtentID uint64
	VpOffset   uint32
	//extentID => discard count
	Discards map[uint64]int64

	blockCache       *ristretto.Cache
	CompressionType  CompressionType
	CompressedSize   uint32
	UncompressedSize uint32
}

func OpenTable(streamReader streamclient.StreamClient,
	extentID uint64, offset uint32) (*Table, error) {

	utils.AssertTrue(xlog.Logger != nil)

	//fmt.Printf("read table from %d, %d\n", extentID, offset)
	blocks, _, err := streamReader.Read(context.Background(), extentID, offset, 1)
	if err != nil {
		fmt.Printf("%+v\n", err)
		return nil, err
	}
	if len(blocks) != 1 {
		return nil, errors.Errorf("len of block is not 1")
	}
	data := blocks[0]

	if len(data) <= 4 {
		return nil, errors.Errorf("meta block should be bigger than 4")
	}

	//fmt.Printf("opentable read size is %d\n", len(data))
	//read checksum
	expected := y.BytesToU32(data[len(data)-4:])

	checksum := utils.NewCRC(data[:len(data)-4]).Value()
	if checksum != expected {
		return nil, errors.Errorf("expected crc is %d, but computed from data is %d", expected, checksum)
	}

	var meta pspb.BlockMeta

	//meta block is not compressed.
	err = meta.Unmarshal(data[:len(data)-4])
	if err != nil {
		return nil, err
	}

	blockCache, err := ristretto.NewCache(&ristretto.Config{
		NumCounters: 1e7,     // number of keys to track frequency of (10M).
		MaxCost:     1 << 30, // maximum cost of cache (1GB).
		BufferItems: 64,      // number of keys per Get buffer.
	})

	utils.AssertTruef(err == nil, "NewCache error: %v", err)

	t := &Table{
		blockIndex:    make([]*pspb.BlockOffset, len(meta.TableIndex.Offsets)),
		streamReader:  streamReader,
		EstimatedSize: meta.TableIndex.EstimatedSize,
		Loc: pspb.Location{
			ExtentID: extentID,
			Offset:   offset,
		},
		LastSeq:          meta.SeqNum,
		VpExtentID:       meta.VpExtentID,
		VpOffset:         meta.VpOffset,
		Discards:         meta.Discards,
		blockCache:       blockCache,
		CompressionType:  CompressionType(meta.CompressionType),
		CompressedSize:   meta.CompressedSize,
		UncompressedSize: meta.UnCompressedSize,
	}

	//read bloom filter
	if t.bf, err = z.JSONUnmarshal(meta.TableIndex.BloomFilter); err != nil {
		return nil, err
	}

	//clone BlockOffset
	for i, offset := range meta.TableIndex.Offsets {
		t.blockIndex[i] = proto.Clone(offset).(*pspb.BlockOffset)
	}

	//get range of table
	if err = t.initBiggestAndSmallest(); err != nil {
		return nil, err
	}
	return t, nil
}

type entriesBlock struct {
	offset            int
	data              []byte
	checksum          uint32
	entriesIndexStart int      // start index of entryOffsets list
	entryOffsets      []uint32 // used to binary search an entry in the block.
}

func (t *Table) decompress(data []byte) ([]byte, error) {
	switch t.CompressionType {
	case Snappy:
		sz, err := snappy.DecodedLen(data)
		if err != nil {
			return nil, err
		}
		decompressed := make([]byte, sz, sz)
		if _, err := snappy.Decode(decompressed, data); err != nil {
			return nil, err
		}
		return decompressed, nil
	case ZSTD:
		return zstd.Decompress(nil, data)
	case None:
		return data, nil
	default:
		return nil, errors.Errorf("unknown compression type: %d", t.CompressionType)
	}
}

func blockCacheID(extentID uint64, offset uint32) []byte {
	buf := make([]byte, 12)
	binary.BigEndian.PutUint64(buf[0:8], extentID)
	binary.BigEndian.PutUint32(buf[8:12], offset)
	return buf
}

func (t *Table) Close() {
	t.blockCache.Close()
}

func (t *Table) block(idx int) (*entriesBlock, error) {

	extentID := t.blockIndex[idx].ExtentID
	offset := t.blockIndex[idx].Offset

	var data []byte
	dataInCache, ok := t.blockCache.Get(blockCacheID(extentID, offset))
	//if cache miss, read data and decompress
	if !ok {
		//fmt.Printf("cache miss for %d, %d\n", extentID, offset)
		blocks, _, err := t.streamReader.Read(context.Background(), extentID, offset, 1)
		if err != nil {
			return nil, err
		}
		if len(blocks) != 1 {
			return nil, errors.Errorf("len of blocks is not 1")
		}
		if len(blocks[0]) < 8 {
			return nil, errors.Errorf("block data should be bigger than 8")
		}
		data, err = t.decompress(blocks[0])
		if err != nil {
			return nil, err
		}
		//set cache
		t.blockCache.Set(blockCacheID(extentID, offset), data, 0)
	} else {
		//fmt.Printf("hit cache for %d, %d\n", extentID, offset)
		data = dataInCache.([]byte)
	}

	expected := y.BytesToU32(data[len(data)-4:])
	checksum := utils.NewCRC(data[:len(data)-4]).Value()
	if checksum != expected {
		return nil, errors.Errorf("expected crc is %d, but computed from data is %d", expected, checksum)
	}

	numEntries := y.BytesToU32(data[len(data)-8:])
	entriesIndexStart := len(data) - 4 - 4 - int(numEntries)*4
	if entriesIndexStart < 0 {
		return nil, errors.Errorf("entriesIndexStart cannot be less than 0")
	}
	entriesIndexEnd := entriesIndexStart + 4*int(numEntries)

	return &entriesBlock{
		offset:            int(offset),
		data:              data[:len(data)-4], //exclude checksum
		entryOffsets:      y.BytesToU32Slice(data[entriesIndexStart:entriesIndexEnd]),
		entriesIndexStart: entriesIndexStart,
		checksum:          checksum,
	}, nil
}

// Smallest is its smallest key, or nil if there are none
func (t *Table) Smallest() []byte { return t.smallest }

// Biggest is its biggest key, or nil if there are none
func (t *Table) Biggest() []byte { return t.biggest }

//the first key of the block with a zero-based index of (n) / 2, where the total number of table is n
func (t *Table) MidKey() []byte {
	n := len(t.blockIndex)
	return t.blockIndex[(n)/2].Key
}

func (t *Table) initBiggestAndSmallest() error {
	t.smallest = t.blockIndex[0].Key

	it2 := t.NewIterator(true)
	defer it2.Close()
	it2.Rewind()
	if !it2.Valid() {
		return errors.Wrapf(it2.err, "failed to initialize biggest for table")
	}
	t.biggest = it2.Key()
	return nil
}

// NewIterator returns a new iterator of the Table
func (t *Table) NewIterator(reversed bool) *Iterator {
	ti := &Iterator{t: t, reversed: reversed}
	ti.next()
	return ti
}

func (t *Table) DoesNotHave(hash uint64) bool {
	return !t.bf.Has(hash)
}

func (t *Table) FirstOccurrence() uint64 {
	return t.blockIndex[0].ExtentID
}
