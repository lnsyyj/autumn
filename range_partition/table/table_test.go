/*
 * Copyright 2017 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package table

import (
	"fmt"
	"math"
	"sort"
	"testing"

	"github.com/journeymidnight/autumn/range_partition/y"
	"github.com/journeymidnight/autumn/utils"

	"github.com/journeymidnight/autumn/streamclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func key(prefix string, i int) string {
	return prefix + fmt.Sprintf("%04d", i)
}

func buildTestTable(t *testing.T, prefix string, n int) (streamclient.StreamClient, uint64, uint32) {
	utils.AssertTrue(n <= 10000)
	keyValues := make([][]string, n)
	for i := 0; i < n; i++ {
		k := key(prefix, i)
		v := fmt.Sprintf("%d", i)
		keyValues[i] = []string{k, v}
	}
	return buildTable(t, keyValues)
}

// keyValues is n by 2 where n is number of pairs.
func buildTable(t *testing.T, keyValues [][]string) (streamclient.StreamClient, uint64, uint32) {
	//open local stream

	stream := streamclient.NewMockStreamClient("log")
	b := NewTableBuilder(stream, None)
	defer b.Close()

	sort.Slice(keyValues, func(i, j int) bool {
		return keyValues[i][0] < keyValues[j][0]
	})
	for _, kv := range keyValues {
		utils.AssertTrue(len(kv) == 2)
		//sizeInMemory is 0
		b.Add(y.KeyWithTs([]byte(kv[0]), 0), y.ValueStruct{Value: []byte(kv[1]), Meta: 'A'})
	}
	b.FinishBlock()                                  //finish current block
	ex, offset, err := b.FinishAll(0, 0, 10, nil, 0) //finish all table
	assert.Nil(t, err)
	return stream, ex, offset
}

func TestMidKey(t *testing.T) {
	stream, id, offset := buildTestTable(t, "key", 5000)
	defer stream.Close()

	table, err := OpenTable(stream, id, offset)

	require.NoError(t, err)

	require.Equal(t, []byte("key2002"), y.ParseKey(table.MidKey()))
}
func TestSeekToFirst(t *testing.T) {
	for _, n := range []int{101, 199, 200, 250, 9999, 10000} {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {

			stream, id, offset := buildTestTable(t, "key", n)
			defer stream.Close()

			table, err := OpenTable(stream, id, offset)

			require.NoError(t, err)
			it := table.NewIterator(false)
			defer it.Close()
			it.seekToFirst()
			require.True(t, it.Valid())
			v := it.Value()
			require.EqualValues(t, "0", string(v.Value))
			require.EqualValues(t, 'A', v.Meta)
		})
	}
}

func TestSeekToLast(t *testing.T) {
	for _, n := range []int{101, 199, 200, 250, 9999, 10000} {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {

			stream, id, offset := buildTestTable(t, "key", n)
			defer stream.Close()
			table, err := OpenTable(stream, id, offset)
			require.NoError(t, err)
			it := table.NewIterator(false)
			defer it.Close()
			it.seekToLast()
			require.True(t, it.Valid())
			v := it.Value()
			require.EqualValues(t, fmt.Sprintf("%d", n-1), string(v.Value))
			require.EqualValues(t, 'A', v.Meta)
			it.prev()
			require.True(t, it.Valid())
			v = it.Value()
			require.EqualValues(t, fmt.Sprintf("%d", n-2), string(v.Value))
			require.EqualValues(t, 'A', v.Meta)
		})
	}
}

func TestSeek(t *testing.T) {
	stream, id, offset := buildTestTable(t, "k", 10000)
	defer stream.Close()

	table, err := OpenTable(stream, id, offset)

	require.NoError(t, err)

	it := table.NewIterator(false)
	defer it.Close()

	var data = []struct {
		in    string
		valid bool
		out   string
	}{
		{"abc", true, "k0000"},
		{"k0100", true, "k0100"},
		{"k0100b", true, "k0101"}, // Test case where we jump to next block.
		{"k1234", true, "k1234"},
		{"k1234b", true, "k1235"},
		{"k9999", true, "k9999"},
		{"z", false, ""},
	}

	for _, tt := range data {
		it.seek(y.KeyWithTs([]byte(tt.in), 0))

		if !tt.valid {
			require.False(t, it.Valid())
			continue
		}
		require.True(t, it.Valid())
		k := it.Key()
		require.EqualValues(t, tt.out, string(y.ParseKey(k)))
		//require.EqualValues(t, tt.out, string(k))

	}
}

func TestSeekForPrev(t *testing.T) {
	stream, id, offset := buildTestTable(t, "k", 10000)
	defer stream.Close()

	table, err := OpenTable(stream, id, offset)

	require.NoError(t, err)

	it := table.NewIterator(false)
	defer it.Close()

	var data = []struct {
		in    string
		valid bool
		out   string
	}{
		{"abc", false, ""},
		{"k0100", true, "k0100"},
		{"k0100b", true, "k0100"}, // Test case where we jump to next block.
		{"k1234", true, "k1234"},
		{"k1234b", true, "k1234"},
		{"k9999", true, "k9999"},
		{"z", true, "k9999"},
	}

	for _, tt := range data {
		it.seekForPrev(y.KeyWithTs([]byte(tt.in), 0))

		if !tt.valid {
			require.False(t, it.Valid())
			continue
		}
		require.True(t, it.Valid())
		k := it.Key()
		require.EqualValues(t, tt.out, string(y.ParseKey(k)))
	}
}

func TestIterateFromStart(t *testing.T) {
	// Vary the number of elements added.
	for _, n := range []int{101, 199, 200, 250, 9999, 10000} {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			stream, id, offset := buildTestTable(t, "k", n)
			defer stream.Close()

			table, err := OpenTable(stream, id, offset)

			require.NoError(t, err)
			ti := table.NewIterator(false)
			defer ti.Close()
			ti.reset()
			ti.seek(y.KeyWithTs([]byte(""), math.MaxUint64))
			//ti.seek([]byte(""))
			require.True(t, ti.Valid())
			// No need to do a Next.
			// ti.Seek brings us to the first key >= "". Essentially a SeekToFirst.
			var count int
			for ; ti.Valid(); ti.next() {
				v := ti.Value()
				require.EqualValues(t, fmt.Sprintf("%d", count), string(v.Value))
				require.EqualValues(t, 'A', v.Meta)
				count++
			}
			require.EqualValues(t, n, count)
		})
	}
}

func TestIterateFromEnd(t *testing.T) {
	// Vary the number of elements added.
	for _, n := range []int{101, 199, 200, 250, 9999, 10000} {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			stream, id, offset := buildTestTable(t, "k", n)
			defer stream.Close()

			table, err := OpenTable(stream, id, offset)

			require.NoError(t, err)

			ti := table.NewIterator(false)
			defer ti.Close()
			ti.reset()
			ti.Seek(y.KeyWithTs([]byte("zzzzzz"), 0))
			//ti.seek([]byte("zzzzzz")) // Seek to end, an invalid element.
			require.False(t, ti.Valid())
			for i := n - 1; i >= 0; i-- {
				ti.prev()
				require.True(t, ti.Valid())
				v := ti.Value()
				require.EqualValues(t, fmt.Sprintf("%d", i), string(v.Value))
				require.EqualValues(t, 'A', v.Meta)
			}
			ti.prev()
			require.False(t, ti.Valid())
		})
	}
}

func TestTable(t *testing.T) {

	stream, id, offset := buildTestTable(t, "key", 10000)
	defer stream.Close()

	table, err := OpenTable(stream, id, offset)

	require.NoError(t, err)
	ti := table.NewIterator(false)
	defer ti.Close()
	kid := 1010
	seek := y.KeyWithTs([]byte(key("key", kid)), 0)

	for ti.seek(seek); ti.Valid(); ti.next() {
		k := ti.Key()
		require.EqualValues(t, string(y.ParseKey(k)), key("key", kid))
		kid++
	}

	if kid != 10000 {
		t.Errorf("Expected kid: 10000. Got: %v", kid)
	}

	ti.seek(y.KeyWithTs([]byte(key("key", 99999)), 0))
	require.False(t, ti.Valid())

	ti.seek(y.KeyWithTs([]byte(key("key", -1)), 0))
	require.True(t, ti.Valid())
	k := ti.Key()
	require.EqualValues(t, string(y.ParseKey(k)), key("key", 0))

}

func TestIterateBackAndForth(t *testing.T) {

	stream, id, offset := buildTestTable(t, "key", 10000)
	defer stream.Close()

	table, err := OpenTable(stream, id, offset)

	require.NoError(t, err)

	seek := y.KeyWithTs([]byte(key("key", 1010)), 0)

	it := table.NewIterator(false)
	defer it.Close()
	it.seek(seek)
	require.True(t, it.Valid())
	k := it.Key()
	require.EqualValues(t, seek, k)

	it.prev()
	it.prev()
	require.True(t, it.Valid())
	k = it.Key()
	require.EqualValues(t, key("key", 1008), string(y.ParseKey(k)))

	it.next()
	it.next()
	require.True(t, it.Valid())
	k = it.Key()
	require.EqualValues(t, key("key", 1010), y.ParseKey(k))

	it.seek(y.KeyWithTs([]byte(key("key", 2000)), 0))

	require.True(t, it.Valid())
	k = it.Key()
	require.EqualValues(t, key("key", 2000), string(y.ParseKey(k)))

	it.prev()
	require.True(t, it.Valid())
	k = it.Key()
	require.EqualValues(t, key("key", 1999), string(y.ParseKey(k)))

	it.seekToFirst()
	k = it.Key()
	require.EqualValues(t, key("key", 0), string(y.ParseKey(k)))

}

func TestUniIterator(t *testing.T) {
	stream, id, offset := buildTestTable(t, "key", 10000)
	defer stream.Close()

	table, err := OpenTable(stream, id, offset)

	require.NoError(t, err)

	{
		it := table.NewIterator(false)
		defer it.Close()
		var count int
		for it.Rewind(); it.Valid(); it.Next() {
			v := it.Value()
			require.EqualValues(t, fmt.Sprintf("%d", count), string(v.Value))
			require.EqualValues(t, 'A', v.Meta)
			count++
		}
		require.EqualValues(t, 10000, count)
	}
	{
		it := table.NewIterator(true)
		defer it.Close()
		var count int
		for it.Rewind(); it.Valid(); it.Next() {
			v := it.Value()
			require.EqualValues(t, fmt.Sprintf("%d", 10000-1-count), string(v.Value))
			require.EqualValues(t, 'A', v.Meta)
			count++
		}
		require.EqualValues(t, 10000, count)
	}
}

// Try having only one table.
func TestConcatIteratorOneTable(t *testing.T) {
	stream, id, offset := buildTable(t, [][]string{
		{"k1", "a1"},
		{"k2", "a2"},
	})
	defer stream.Close()
	tbl, err := OpenTable(stream, id, offset)

	require.NoError(t, err)

	it := NewConcatIterator([]*Table{tbl}, false)
	defer it.Close()

	it.Rewind()
	require.True(t, it.Valid())
	k := it.Key()
	require.EqualValues(t, "k1", string(y.ParseKey(k)))
	vs := it.Value()
	require.EqualValues(t, "a1", string(vs.Value))
	require.EqualValues(t, 'A', vs.Meta)
}

func TestConcatIterator(t *testing.T) {
	f, id, offset := buildTestTable(t, "keya", 10000)
	defer f.Close()
	f2, id2, offset2 := buildTestTable(t, "keyb", 10000)
	defer f2.Close()

	f3, id3, offset3 := buildTestTable(t, "keyc", 10000)
	defer f3.Close()

	tbl, err := OpenTable(f, id, offset)
	require.NoError(t, err)
	tbl2, err := OpenTable(f2, id2, offset2)
	require.NoError(t, err)
	tbl3, err := OpenTable(f3, id3, offset3)
	require.NoError(t, err)

	{
		it := NewConcatIterator([]*Table{tbl, tbl2, tbl3}, false)
		defer it.Close()
		it.Rewind()
		require.True(t, it.Valid())
		var count int
		for ; it.Valid(); it.Next() {
			vs := it.Value()
			require.EqualValues(t, fmt.Sprintf("%d", count%10000), string(vs.Value))
			require.EqualValues(t, 'A', vs.Meta)
			count++
		}
		require.EqualValues(t, 30000, count)

		it.Seek(y.KeyWithTs([]byte("a"), 0))
		require.EqualValues(t, "keya0000", string(y.ParseKey(it.Key())))
		vs := it.Value()
		require.EqualValues(t, "0", string(vs.Value))

		it.Seek(y.KeyWithTs([]byte("keyb"), 0))
		require.EqualValues(t, "keyb0000", string(y.ParseKey(it.Key())))
		vs = it.Value()
		require.EqualValues(t, "0", string(vs.Value))

		it.Seek(y.KeyWithTs([]byte("keyb9999b"), 0))
		require.EqualValues(t, "keyc0000", string(y.ParseKey(it.Key())))
		vs = it.Value()
		require.EqualValues(t, "0", string(vs.Value))

		it.Seek(y.KeyWithTs([]byte("keyd"), 0))
		require.False(t, it.Valid())
	}
	{
		it := NewConcatIterator([]*Table{tbl, tbl2, tbl3}, true)
		defer it.Close()
		it.Rewind()
		require.True(t, it.Valid())
		var count int
		for ; it.Valid(); it.Next() {
			vs := it.Value()
			require.EqualValues(t, fmt.Sprintf("%d", 10000-(count%10000)-1), string(vs.Value))
			require.EqualValues(t, 'A', vs.Meta)
			count++
		}
		require.EqualValues(t, 30000, count)

		it.Seek(y.KeyWithTs([]byte("a"), 0))
		require.False(t, it.Valid())

		it.Seek(y.KeyWithTs([]byte("keyb"), 0))
		require.EqualValues(t, "keya9999", string(y.ParseKey(it.Key())))
		vs := it.Value()
		require.EqualValues(t, "9999", string(vs.Value))

		it.Seek(y.KeyWithTs([]byte("keyb9999b"), 0))
		require.EqualValues(t, "keyb9999", string(y.ParseKey(it.Key())))
		vs = it.Value()
		require.EqualValues(t, "9999", string(vs.Value))

		it.Seek(y.KeyWithTs([]byte("keyd"), 0))
		require.EqualValues(t, "keyc9999", string(y.ParseKey(it.Key())))
		vs = it.Value()
		require.EqualValues(t, "9999", string(vs.Value))
	}
}

func TestMergingIterator(t *testing.T) {
	f1, id1, offset1 := buildTable(t, [][]string{
		{"k1", "a1"},
		{"k4", "a4"},
		{"k5", "a5"},
	})
	defer f1.Close()
	f2, id2, offset2 := buildTable(t, [][]string{
		{"k2", "b2"},
		{"k3", "b3"},
		{"k4", "b4"},
	})
	defer f2.Close()

	expected := []struct {
		key   string
		value string
	}{
		{"k1", "a1"},
		{"k2", "b2"},
		{"k3", "b3"},
		{"k4", "a4"},
		{"k5", "a5"},
	}
	tbl1, err := OpenTable(f1, id1, offset1)
	require.NoError(t, err)
	tbl2, err := OpenTable(f2, id2, offset2)
	require.NoError(t, err)
	it1 := tbl1.NewIterator(false)
	it2 := NewConcatIterator([]*Table{tbl2}, false)
	it := NewMergeIterator([]y.Iterator{it1, it2}, false)
	defer it.Close()

	var i int
	for it.Rewind(); it.Valid(); it.Next() {
		k := it.Key()
		vs := it.Value()
		require.EqualValues(t, expected[i].key, string(y.ParseKey(k)))
		require.EqualValues(t, expected[i].value, string(vs.Value))
		require.EqualValues(t, 'A', vs.Meta)
		i++
	}
	require.Equal(t, i, len(expected))
	require.False(t, it.Valid())
}

func TestMergingIteratorReversed(t *testing.T) {
	f1, id1, offset1 := buildTable(t, [][]string{
		{"k1", "a1"},
		{"k2", "a2"},
		{"k4", "a4"},
		{"k5", "a5"},
	})
	defer f1.Close()
	f2, id2, offset2 := buildTable(t, [][]string{
		{"k1", "b2"},
		{"k3", "b3"},
		{"k4", "b4"},
		{"k5", "b5"},
	})
	defer f2.Close()

	expected := []struct {
		key   string
		value string
	}{
		{"k5", "a5"},
		{"k4", "a4"},
		{"k3", "b3"},
		{"k2", "a2"},
		{"k1", "a1"},
	}
	tbl1, err := OpenTable(f1, id1, offset1)
	require.NoError(t, err)
	tbl2, err := OpenTable(f2, id2, offset2)
	require.NoError(t, err)
	it1 := tbl1.NewIterator(true)
	it2 := NewConcatIterator([]*Table{tbl2}, true)
	it := NewMergeIterator([]y.Iterator{it1, it2}, true)
	defer it.Close()

	var i int
	for it.Rewind(); it.Valid(); it.Next() {
		k := it.Key()
		vs := it.Value()
		require.EqualValues(t, expected[i].key, string(y.ParseKey(k)))
		require.EqualValues(t, expected[i].value, string(vs.Value))
		require.EqualValues(t, 'A', vs.Meta)
		i++
	}

	require.Equal(t, i, len(expected))
	require.False(t, it.Valid())
}

// Take only the first iterator.
func TestMergingIteratorTakeOne(t *testing.T) {
	f1, id1, offset1 := buildTable(t, [][]string{
		{"k1", "a1"},
		{"k2", "a2"},
	})
	defer f1.Close()

	f2, id2, offset2 := buildTable(t, [][]string{{"l1", "b1"}})
	defer f2.Close()
	t1, err := OpenTable(f1, id1, offset1)
	require.NoError(t, err)
	t2, err := OpenTable(f2, id2, offset2)
	require.NoError(t, err)

	it1 := NewConcatIterator([]*Table{t1}, false)
	it2 := NewConcatIterator([]*Table{t2}, false)
	it := NewMergeIterator([]y.Iterator{it1, it2}, false)
	defer it.Close()

	it.Rewind()
	require.True(t, it.Valid())
	k := it.Key()
	require.EqualValues(t, "k1", string(y.ParseKey(k)))
	vs := it.Value()
	require.EqualValues(t, "a1", string(vs.Value))
	require.EqualValues(t, 'A', vs.Meta)
	it.Next()

	require.True(t, it.Valid())
	k = it.Key()
	require.EqualValues(t, "k2", string(y.ParseKey(k)))
	vs = it.Value()
	require.EqualValues(t, "a2", string(vs.Value))
	require.EqualValues(t, 'A', vs.Meta)
	it.Next()

	k = it.Key()
	require.EqualValues(t, "l1", string(y.ParseKey(k)))
	vs = it.Value()
	require.EqualValues(t, "b1", string(vs.Value))
	require.EqualValues(t, 'A', vs.Meta)
	it.Next()

	require.False(t, it.Valid())
}

// Take only the second iterator.
func TestMergingIteratorTakeTwo(t *testing.T) {
	f1, id1, offset1 := buildTable(t, [][]string{{"l1", "b1"}})
	defer f1.Close()
	f2, id2, offset2 := buildTable(t, [][]string{
		{"k1", "a1"},
		{"k2", "a2"},
	})
	defer f2.Close()

	t1, err := OpenTable(f1, id1, offset1)
	require.NoError(t, err)
	t2, err := OpenTable(f2, id2, offset2)
	require.NoError(t, err)

	it1 := NewConcatIterator([]*Table{t1}, false)
	it2 := NewConcatIterator([]*Table{t2}, false)
	it := NewMergeIterator([]y.Iterator{it1, it2}, false)
	defer it.Close()

	it.Rewind()
	require.True(t, it.Valid())
	k := it.Key()
	require.EqualValues(t, "k1", string(y.ParseKey(k)))
	vs := it.Value()
	require.EqualValues(t, "a1", string(vs.Value))
	require.EqualValues(t, 'A', vs.Meta)
	it.Next()

	require.True(t, it.Valid())
	k = it.Key()
	require.EqualValues(t, "k2", string(y.ParseKey(k)))
	vs = it.Value()
	require.EqualValues(t, "a2", string(vs.Value))
	require.EqualValues(t, 'A', vs.Meta)
	it.Next()
	require.True(t, it.Valid())

	k = it.Key()
	require.EqualValues(t, "l1", string(y.ParseKey(k)))
	vs = it.Value()
	require.EqualValues(t, "b1", string(vs.Value))
	require.EqualValues(t, 'A', vs.Meta)
	it.Next()

	require.False(t, it.Valid())
}

func TestTableBigValues(t *testing.T) {
	value := func(i int) []byte {
		return []byte(fmt.Sprintf("%01048576d", i)) // Return 1MB value which is > math.MaxUint16.
	}

	stream := streamclient.NewMockStreamClient("log")
	defer stream.Close()

	n := 100 // Insert 100 keys.

	builder := NewTableBuilder(stream, Snappy)
	for i := 0; i < n; i++ {
		key := y.KeyWithTs([]byte(key("", i)), 0)
		vs := y.ValueStruct{Value: value(i)}

		builder.Add(key, vs)
	}
	builder.FinishBlock()
	id, offset, err := builder.FinishAll(0, 0, 0, map[uint64]int64{10: 10}, 0)
	require.NoError(t, err)

	tbl, err := OpenTable(stream, id, offset)
	require.NoError(t, err, "unable to open table")
	require.Equal(t, map[uint64]int64{10: 10}, tbl.Discards)

	itr := tbl.NewIterator(false)
	require.True(t, itr.Valid())

	count := 0
	for itr.Rewind(); itr.Valid(); itr.Next() {
		require.Equal(t, []byte(key("", count)), y.ParseKey(itr.Key()), "keys are not equal")

		require.Equal(t, len(value(count)), len(itr.Value().Value), "values are not equal")
		count++
	}
	require.False(t, itr.Valid(), "table iterator should be invalid now")
	require.Equal(t, n, count)
}

/*
var cacheConfig = ristretto.Config{
	NumCounters: 1000000 * 10,
	MaxCost:     1000000,
	BufferItems: 64,
	Metrics:     true,
}

func BenchmarkRead(b *testing.B) {
	n := int(5 * 1e6)
	tbl := getTableForBenchmarks(b, n, nil)

	b.ResetTimer()
	// Iterate b.N times over the entire table.
	for i := 0; i < b.N; i++ {
		func() {
			it := tbl.NewIterator(false)
			defer it.Close()
			for it.seekToFirst(); it.Valid(); it.next() {
			}
		}()
	}
}

func BenchmarkReadAndBuild(b *testing.B) {
	n := int(5 * 1e6)

	var cache, _ = ristretto.NewCache(&cacheConfig)
	tbl := getTableForBenchmarks(b, n, cache)

	b.ResetTimer()
	// Iterate b.N times over the entire table.
	for i := 0; i < b.N; i++ {
		func() {
			opts := Options{Compression: options.ZSTD, BlockSize: 4 * 0124, BloomFalsePositive: 0.01}
			opts.Cache = cache
			newBuilder := NewTableBuilder(opts)
			it := tbl.NewIterator(false)
			defer it.Close()
			for it.seekToFirst(); it.Valid(); it.next() {
				vs := it.Value()
				newBuilder.Add(it.Key(), vs, 0)
			}
			newBuilder.Finish()
		}()
	}
}

func BenchmarkReadMerged(b *testing.B) {
	n := int(5 * 1e6)
	m := 5 // Number of tables.
	y.AssertTrue((n % m) == 0)
	tableSize := n / m
	var tables []*Table

	var cache, err = ristretto.NewCache(&cacheConfig)
	require.NoError(b, err)

	for i := 0; i < m; i++ {
		filename := fmt.Sprintf("%s%s%d.sst", os.TempDir(), string(os.PathSeparator), rand.Int63())
		opts := Options{Compression: options.ZSTD, BlockSize: 4 * 1024, BloomFalsePositive: 0.01}
		opts.Cache = cache
		builder := NewTableBuilder(opts)
		f, err := y.OpenSyncedFile(filename, true)
		y.Check(err)
		for j := 0; j < tableSize; j++ {
			id := j*m + i // Arrays are interleaved.
			// id := i*tableSize+j (not interleaved)
			k := fmt.Sprintf("%016x", id)
			v := fmt.Sprintf("%d", id)
			builder.Add([]byte(k), y.ValueStruct{Value: []byte(v), Meta: 123}, 0)
		}
		_, err = f.Write(builder.Finish())
		require.NoError(b, err, "unable to write to file")
		tbl, err := OpenTable(f, opts)
		y.Check(err)
		tables = append(tables, tbl)
	}

	b.ResetTimer()
	// Iterate b.N times over the entire table.
	for i := 0; i < b.N; i++ {
		func() {
			var iters []y.Iterator
			for _, tbl := range tables {
				iters = append(iters, tbl.NewIterator(false))
			}
			it := NewMergeIterator(iters, false)
			defer it.Close()
			for it.Rewind(); it.Valid(); it.Next() {
			}
		}()
	}
}

func BenchmarkChecksum(b *testing.B) {
	keySz := []int{KB, 2 * KB, 4 * KB, 8 * KB, 16 * KB, 32 * KB, 64 * KB, 128 * KB, 256 * KB, MB}
	for _, kz := range keySz {
		key := make([]byte, kz)
		b.Run(fmt.Sprintf("CRC %d", kz), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				crc32.ChecksumIEEE(key)
			}
		})
		b.Run(fmt.Sprintf("xxHash64 %d", kz), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				xxhash.Sum64(key)
			}
		})
		b.Run(fmt.Sprintf("SHA256 %d", kz), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				sha256.Sum256(key)
			}
		})
	}
}

func BenchmarkRandomRead(b *testing.B) {
	n := int(5 * 1e6)
	tbl := getTableForBenchmarks(b, n, nil)

	r := rand.New(rand.NewSource(time.Now().Unix()))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		itr := tbl.NewIterator(false)
		no := r.Intn(n)
		k := []byte(fmt.Sprintf("%016x", no))
		v := []byte(fmt.Sprintf("%d", no))
		itr.Seek(k)
		if !itr.Valid() {
			b.Fatal("itr should be valid")
		}
		v1 := itr.Value().Value

		if !bytes.Equal(v, v1) {
			fmt.Println("value does not match")
			b.Fatal()
		}
		itr.Close()
	}
}

func getTableForBenchmarks(b *testing.B, count int, cache *ristretto.Cache) *Table {
	rand.Seed(time.Now().Unix())
	opts := Options{Compression: options.ZSTD, BlockSize: 4 * 1024, BloomFalsePositive: 0.01}
	if cache == nil {
		var err error
		cache, err = ristretto.NewCache(&cacheConfig)
		require.NoError(b, err)
	}
	opts.Cache = cache
	builder := NewTableBuilder(opts)
	filename := fmt.Sprintf("%s%s%d.sst", os.TempDir(), string(os.PathSeparator), rand.Int63())
	f, err := y.OpenSyncedFile(filename, true)
	require.NoError(b, err)
	for i := 0; i < count; i++ {
		k := fmt.Sprintf("%016x", i)
		v := fmt.Sprintf("%d", i)
		builder.Add([]byte(k), y.ValueStruct{Value: []byte(v)}, 0)
	}

	_, err = f.Write(builder.Finish())
	require.NoError(b, err, "unable to write to file")
	tbl, err := OpenTable(f, opts)
	require.NoError(b, err, "unable to open table")
	return tbl
}

func TestMain(m *testing.M) {
	rand.Seed(time.Now().UTC().UnixNano())
	os.Exit(m.Run())
}
*/

// func TestOpenKVSize(t *testing.T) {
// 	opts := getTestTableOptions()
// 	table, err := OpenTable(buildTestTable(t, "foo", 1, opts), opts)
// 	require.NoError(t, err)

// 	// The following values might change if the table/header structure is changed.
// 	//var entrySize uint64 = 15 /* DiffKey len */ + 4 /* Header Size */ + 4 /* Encoded vp */
// 	require.Equal(t, entrySize, table.EstimatedSize())
// }
