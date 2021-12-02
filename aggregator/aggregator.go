/*
   Copyright 2021 Erigon contributors

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package aggregator

import (
	"bufio"
	"bytes"
	"container/heap"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"regexp"
	"strconv"

	"github.com/google/btree"
	"github.com/holiman/uint256"
	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/compress"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon-lib/recsplit"
	"github.com/ledgerwatch/log/v3"
)

// Aggregator of multiple state files to support state reader and state writer
// The convension for the file names are as follows
// State is composed of three types of files:
// 1. Accounts. keys are addresses (20 bytes), values are encoding of accounts
// 2. Contract storage. Keys are concatenation of addresses (20 bytes) and storage locations (32 bytes), values have their leading zeroes removed
// 3. Contract codes. Keys are addresses (20 bytes), values are bycodes
// Within each type, any file can cover an interval of block numbers, for example, `accounts.1-16` represents changes in accounts
// that were effected by the blocks from 1 to 16, inclusively. The second component of the interval will be called "end block" for the file.
// Finally, for each type and interval, there are two files - one with the compressed data (extension `dat`),
// and another with the index (extension `idx`) consisting of the minimal perfect hash table mapping keys to the offsets of corresponding keys
// in the data file
// Aggregator consists (apart from the file it is aggregating) of the 4 parts:
// 1. Persistent table of expiration time for each of the files. Key - name of the file, value - timestamp, at which the file can be removed
// 2. Transient (in-memory) mapping the "end block" of each file to the objects required for accessing the file (compress.Decompressor and resplit.Index)
// 3. Persistent tables (one for accounts, one for contract storage, and one for contract code) summarising all the 1-block state diff files
//    that were not yet merged together to form larger files. In these tables, keys are the same as keys in the state diff files, but values are also
//    augemented by the number of state diff files this key is present. This number gets decremented every time when a 1-block state diff files is removed
//    from the summary table (due to being merged). And when this number gets to 0, the record is deleted from the summary table.
//    This number is encoded into first 4 bytes of the value
// 4. Aggregating persistent hash table. Maps state keys to the block numbers for the use in the part 2 (which is not necessarily the block number where
//    the item last changed, but it is guaranteed to find correct element in the Transient mapping of part 2

type Aggregator struct {
	diffDir         string // Directory where the state diff files are stored
	byEndBlock      *btree.BTree
	unwindLimit     uint64              // How far the chain may unwind
	aggregationStep uint64              // How many items (block, but later perhaps txs or changes) are required to form one state diff file
	changeFileNum   uint64              // Block number associated with the current change files. It is the last block number whose changes will go into that file
	accountChanges  Changes             // Change files for accounts
	codeChanges     Changes             // Change files for contract code
	storageChanges  Changes             // Change files for contract storage
	commChanges     Changes             // Change files for commitment
	changesBtree    *btree.BTree        // btree of ChangesItem
	trace           bool                // Turns on tracing for specific accounts and locations
	tracedKeys      map[string]struct{} // Set of keys being traced during aggregations
}

type ChangeFile struct {
	dir         string
	step        uint64
	namebase    string
	path        string
	file        *os.File
	w           *bufio.Writer
	r           *bufio.Reader
	numBuf      [8]byte
	sizeCounter uint64
	blockPos    int64 // Position of the last block iterated upon
	blockNum    uint64
	blockSize   uint64
	words       []byte // Words pending for the next block record, in the same slice
	wordOffsets []int  // Offsets of words in the `words` slice
}

func (cf *ChangeFile) closeFile() error {
	if len(cf.wordOffsets) > 0 {
		return fmt.Errorf("closeFile without finish")
	}
	if cf.w != nil {
		if err := cf.w.Flush(); err != nil {
			return err
		}
		cf.w = nil
	}
	if cf.file != nil {
		if err := cf.file.Close(); err != nil {
			return err
		}
		cf.file = nil
	}
	return nil
}

func (cf *ChangeFile) openFile(blockNum uint64, write bool) error {
	if len(cf.wordOffsets) > 0 {
		return fmt.Errorf("openFile without finish")
	}
	rem := blockNum % cf.step
	startBlock := blockNum - rem
	endBlock := startBlock + cf.step - 1
	if cf.w == nil {
		cf.path = path.Join(cf.dir, fmt.Sprintf("%s.%d-%d.chg", cf.namebase, startBlock, endBlock))
		var err error
		if write {
			if cf.file, err = os.OpenFile(cf.path, os.O_RDWR|os.O_CREATE, 0755); err != nil {
				return err
			}
			cf.w = bufio.NewWriter(cf.file)
		} else {
			if cf.file, err = os.Open(cf.path); err != nil {
				return err
			}
			if cf.blockPos, err = cf.file.Seek(0, 2 /* relative to the end of the file */); err != nil {
				return err
			}
		}
		cf.r = bufio.NewReader(cf.file)
	}
	return nil
}

func (cf *ChangeFile) add(word []byte) {
	cf.words = append(cf.words, word...)
	cf.wordOffsets = append(cf.wordOffsets, len(cf.words))
}

func (cf *ChangeFile) finish(blockNum uint64) error {
	// Write out words
	lastOffset := 0
	for _, offset := range cf.wordOffsets {
		word := cf.words[lastOffset:offset]
		n := binary.PutUvarint(cf.numBuf[:], uint64(len(word)))
		if _, err := cf.w.Write(cf.numBuf[:n]); err != nil {
			return err
		}
		if len(word) > 0 {
			if _, err := cf.w.Write(word); err != nil {
				return err
			}
		}
		cf.sizeCounter += uint64(n + len(word))
		lastOffset = offset
	}
	cf.words = cf.words[:0]
	cf.wordOffsets = cf.wordOffsets[:0]
	// Write out block number and then size of changes in this block
	binary.BigEndian.PutUint64(cf.numBuf[:], blockNum)
	if _, err := cf.w.Write(cf.numBuf[:]); err != nil {
		return err
	}
	binary.BigEndian.PutUint64(cf.numBuf[:], cf.sizeCounter)
	if _, err := cf.w.Write(cf.numBuf[:]); err != nil {
		return err
	}
	cf.sizeCounter = 0
	return nil
}

// prevBlock positions the reader to the beginning
// of the block
func (cf *ChangeFile) prevBlock() (bool, error) {
	if cf.blockPos == 0 {
		return false, nil
	}
	// Move back 16 bytes to read block number and block size
	pos, err := cf.file.Seek(cf.blockPos-16, 0 /* relative to the beginning */)
	if err != nil {
		return false, err
	}
	cf.r.Reset(cf.file)
	if _, err = io.ReadFull(cf.r, cf.numBuf[:8]); err != nil {
		return false, err
	}
	cf.blockNum = binary.BigEndian.Uint64(cf.numBuf[:])
	if _, err = io.ReadFull(cf.r, cf.numBuf[:8]); err != nil {
		return false, err
	}
	cf.blockSize = binary.BigEndian.Uint64(cf.numBuf[:])
	cf.blockPos, err = cf.file.Seek(pos-int64(cf.blockSize), 0)
	if err != nil {
		return false, err
	}
	cf.r.Reset(cf.file)
	return true, nil
}

func (cf *ChangeFile) nextWord(wordBuf []byte) ([]byte, bool, error) {
	if cf.blockSize == 0 {
		return wordBuf, false, nil
	}
	ws, err := binary.ReadUvarint(cf.r)
	if err != nil {
		return wordBuf, false, fmt.Errorf("word size: %w", err)
	}
	var buf []byte
	if total := len(wordBuf) + int(ws); cap(wordBuf) >= total {
		buf = wordBuf[:total] // Reuse the space in wordBuf, is it has enough capacity
	} else {
		buf = make([]byte, total)
		copy(buf, wordBuf)
	}
	if _, err = io.ReadFull(cf.r, buf[len(wordBuf):]); err != nil {
		return wordBuf, false, fmt.Errorf("read word (%d %d): %w", ws, len(buf[len(wordBuf):]), err)
	}
	n := binary.PutUvarint(cf.numBuf[:], ws)
	cf.blockSize -= uint64(n) + ws
	return buf, true, nil
}

func (cf *ChangeFile) deleteFile() error {
	return os.Remove(cf.path)
}

type Changes struct {
	namebase string
	keys     ChangeFile
	before   ChangeFile
	after    ChangeFile
	step     uint64
	dir      string
}

func (c *Changes) Init(namebase string, step uint64, dir string) {
	c.namebase = namebase
	c.step = step
	c.dir = dir
	c.keys.namebase = namebase + ".keys"
	c.keys.dir = dir
	c.keys.step = step
	c.before.namebase = namebase + ".before"
	c.before.dir = dir
	c.before.step = step
	c.after.namebase = namebase + ".after"
	c.after.dir = dir
	c.after.step = step
}

func (c *Changes) closeFiles() error {
	if err := c.keys.closeFile(); err != nil {
		return err
	}
	if err := c.before.closeFile(); err != nil {
		return err
	}
	if err := c.after.closeFile(); err != nil {
		return err
	}
	return nil
}

func (c *Changes) openFiles(blockNum uint64, write bool) error {
	if err := c.keys.openFile(blockNum, write); err != nil {
		return err
	}
	if err := c.before.openFile(blockNum, write); err != nil {
		return err
	}
	if err := c.after.openFile(blockNum, write); err != nil {
		return err
	}
	return nil
}

func (c *Changes) insert(key, after []byte) {
	c.keys.add(key)
	c.before.add(nil)
	c.after.add(after)
}

func (c *Changes) update(key, before, after []byte) {
	c.keys.add(key)
	c.before.add(before)
	c.after.add(after)
}

func (c *Changes) delete(key, before []byte) {
	c.keys.add(key)
	c.before.add(before)
	c.after.add(nil)
}

func (c *Changes) finish(blockNum uint64) error {
	if err := c.keys.finish(blockNum); err != nil {
		return err
	}
	if err := c.before.finish(blockNum); err != nil {
		return err
	}
	if err := c.after.finish(blockNum); err != nil {
		return err
	}
	return nil
}

func (c *Changes) prevBlock() (bool, error) {
	bkeys, err := c.keys.prevBlock()
	if err != nil {
		return false, err
	}
	var bbefore, bafter bool
	if bbefore, err = c.before.prevBlock(); err != nil {
		return false, err
	}
	if bafter, err = c.after.prevBlock(); err != nil {
		return false, err
	}
	if bkeys != bbefore || bkeys != bafter {
		return false, fmt.Errorf("inconsistent block iteration")
	}
	return bkeys, nil
}

func (c *Changes) nextTriple(keyBuf, beforeBuf []byte, afterBuf []byte) ([]byte, []byte, []byte, bool, error) {
	key, bkeys, err := c.keys.nextWord(keyBuf)
	if err != nil {
		return keyBuf, beforeBuf, afterBuf, false, fmt.Errorf("next key: %w", err)
	}
	var before, after []byte
	var bbefore, bafter bool
	if before, bbefore, err = c.before.nextWord(beforeBuf); err != nil {
		return keyBuf, beforeBuf, afterBuf, false, fmt.Errorf("next before: %w", err)
	}
	if after, bafter, err = c.after.nextWord(afterBuf); err != nil {
		return keyBuf, beforeBuf, afterBuf, false, fmt.Errorf("next after: %w", err)
	}
	if bkeys != bbefore || bkeys != bafter {
		return keyBuf, beforeBuf, afterBuf, false, fmt.Errorf("inconsistent word iteration")
	}
	return key, before, after, bkeys, nil
}

func (c *Changes) deleteFiles() error {
	if err := c.keys.deleteFile(); err != nil {
		return err
	}
	if err := c.before.deleteFile(); err != nil {
		return err
	}
	if err := c.after.deleteFile(); err != nil {
		return err
	}
	return nil
}

func buildIndex(datPath, idxPath, tmpDir string, count int) (*compress.Decompressor, *recsplit.Index, error) {
	d, err := compress.NewDecompressor(datPath)
	if err != nil {
		return nil, nil, err
	}
	var rs *recsplit.RecSplit
	if rs, err = recsplit.NewRecSplit(recsplit.RecSplitArgs{
		KeyCount:   count,
		Enums:      false,
		BucketSize: 2000,
		Salt:       0,
		LeafSize:   8,
		TmpDir:     tmpDir,
		StartSeed: []uint64{0x106393c187cae21a, 0x6453cec3f7376937, 0x643e521ddbd2be98, 0x3740c6412f6572cb, 0x717d47562f1ce470, 0x4cd6eb4c63befb7c, 0x9bfd8c5e18c8da73,
			0x082f20e10092a9a3, 0x2ada2ce68d21defc, 0xe33cb4f3e7c6466b, 0x3980be458c509c59, 0xc466fd9584828e8c, 0x45f0aabe1a61ede6, 0xf6e7b8b33ad9b98d,
			0x4ef95e25f4b4983d, 0x81175195173b92d3, 0x4e50927d8dd15978, 0x1ea2099d1fafae7f, 0x425c8a06fbaaa815, 0xcd4216006c74052a},
		IndexFile: idxPath,
	}); err != nil {
		return nil, nil, err
	}
	word := make([]byte, 0, 256)
	var pos uint64
	for {
		g := d.MakeGetter()
		for g.HasNext() {
			word, _ = g.Next(word[:0])
			if err = rs.AddKey(word, pos); err != nil {
				return nil, nil, err
			}
			// Skip value
			word, pos = g.Next(word[:0])
		}
		if err = rs.Build(); err != nil {
			return nil, nil, err
		}
		if rs.Collision() {
			log.Info("Building recsplit. Collision happened. It's ok. Restarting...")
			rs.ResetNextSalt()
		} else {
			break
		}
	}
	var idx *recsplit.Index
	if idx, err = recsplit.OpenIndex(idxPath); err != nil {
		return nil, nil, err
	}
	return d, idx, nil
}

func (c *Changes) aggregate(blockFrom, blockTo uint64, prefixLen int, tx kv.RwTx, table string) (*compress.Decompressor, *recsplit.Index, error) {
	if err := c.openFiles(blockTo, false /* write */); err != nil {
		return nil, nil, fmt.Errorf("open files: %w", err)
	}
	bt := btree.New(32)
	if err := c.aggregateToBtree(bt, prefixLen); err != nil {
		return nil, nil, fmt.Errorf("aggregateToBtree: %w", err)
	}
	if err := c.closeFiles(); err != nil {
		return nil, nil, fmt.Errorf("close files: %w", err)
	}
	// Clean up the DB table
	var e error
	bt.Ascend(func(i btree.Item) bool {
		item := i.(*AggregateItem)
		if item.count == 0 {
			return true
		}
		prevV, err := tx.GetOne(table, item.k)
		if err != nil {
			e = err
			return false
		}
		if prevV == nil {
			e = fmt.Errorf("record not found in db for %s key %x", table, item.k)
			return false
		}
		prevNum := binary.BigEndian.Uint32(prevV[:4])
		if prevNum < item.count {
			e = fmt.Errorf("record count too low for %s key %s count %d, subtracting %d", table, item.k, prevNum, item.count)
			return false
		}
		if prevNum == item.count {
			if e = tx.Delete(table, item.k, nil); e != nil {
				return false
			}
		} else {
			v := common.Copy(prevV)
			binary.BigEndian.PutUint32(v[:4], prevNum-item.count)
			if e = tx.Put(table, item.k, v); e != nil {
				return false
			}
		}
		return true
	})
	if e != nil {
		return nil, nil, fmt.Errorf("clean up table %s after aggregation: %w", table, e)
	}
	datPath := path.Join(c.dir, fmt.Sprintf("%s.%d-%d.dat", c.namebase, blockFrom, blockTo))
	idxPath := path.Join(c.dir, fmt.Sprintf("%s.%d-%d.idx", c.namebase, blockFrom, blockTo))
	var count int
	var err error
	if count, err = btreeToFile(bt, datPath, c.dir); err != nil {
		return nil, nil, fmt.Errorf("btreeToFile: %w", err)
	}
	return buildIndex(datPath, idxPath, c.dir, count)
}

type AggregateItem struct {
	k, v  []byte
	count uint32
}

func (i *AggregateItem) Less(than btree.Item) bool {
	return bytes.Compare(i.k, than.(*AggregateItem).k) < 0
}

func (c *Changes) aggregateToBtree(bt *btree.BTree, prefixLen int) error {
	var b bool
	var e error
	var key, before, after []byte
	var ai AggregateItem
	var prefix []byte
	for b, e = c.prevBlock(); b && e == nil; b, e = c.prevBlock() {
		for key, before, after, b, e = c.nextTriple(key[:0], before[:0], after[:0]); b && e == nil; key, before, after, b, e = c.nextTriple(key[:0], before[:0], after[:0]) {
			if prefixLen > 0 && !bytes.Equal(prefix, key[:prefixLen]) {
				prefix = common.Copy(key[:prefixLen])
				item := &AggregateItem{k: prefix, count: 0}
				bt.ReplaceOrInsert(item)
			}
			ai.k = key
			ai.v = after
			i := bt.Get(&ai)
			if i == nil {
				item := &AggregateItem{k: common.Copy(key), count: 1}
				if len(after) > 0 {
					item.v = make([]byte, 1+len(after))
					if len(before) == 0 {
						item.v[0] = 1
					}
					copy(item.v[1:], after)
				}
				bt.ReplaceOrInsert(item)
			} else {
				i.(*AggregateItem).count++
			}
		}
		if e != nil {
			return fmt.Errorf("nextTriple: %w", e)
		}
	}
	if e != nil {
		return fmt.Errorf("prevBlock: %w", e)
	}
	return nil
}

const AggregatorPrefix = "aggregator"

func btreeToFile(bt *btree.BTree, datPath string, tmpdir string) (int, error) {
	comp, err := compress.NewCompressor(AggregatorPrefix, datPath, tmpdir, 1024 /* minPatterScore */)
	if err != nil {
		return 0, err
	}
	count := 0
	bt.Ascend(func(i btree.Item) bool {
		item := i.(*AggregateItem)
		if err = comp.AddWord(item.k); err != nil {
			return false
		}
		count++ // Only counting keys, not values
		if err = comp.AddWord(item.v); err != nil {
			return false
		}
		return true
	})
	if err != nil {
		return 0, err
	}
	if err = comp.Compress(); err != nil {
		return 0, err
	}
	return count, nil
}

type ChangesItem struct {
	endBlock   uint64
	startBlock uint64
	fileCount  int
}

func (i *ChangesItem) Less(than btree.Item) bool {
	if i.endBlock == than.(*ChangesItem).endBlock {
		// Larger intevals will come last
		return i.startBlock > than.(*ChangesItem).startBlock
	}
	return i.endBlock < than.(*ChangesItem).endBlock
}

type byEndBlockItem struct {
	startBlock    uint64
	endBlock      uint64
	fileCount     int
	accountsD     *compress.Decompressor
	accountsIdx   *recsplit.Index
	storageD      *compress.Decompressor
	storageIdx    *recsplit.Index
	codeD         *compress.Decompressor
	codeIdx       *recsplit.Index
	commitmentD   *compress.Decompressor
	commitmentIdx *recsplit.Index
}

func (i *byEndBlockItem) Less(than btree.Item) bool {
	if i.endBlock == than.(*byEndBlockItem).endBlock {
		return i.startBlock > than.(*byEndBlockItem).startBlock
	}
	return i.endBlock < than.(*byEndBlockItem).endBlock
}

func NewAggregator(diffDir string, unwindLimit uint64, aggregationStep uint64) (*Aggregator, error) {
	a := &Aggregator{
		diffDir:         diffDir,
		unwindLimit:     unwindLimit,
		aggregationStep: aggregationStep,
		tracedKeys:      make(map[string]struct{}),
	}
	byEndBlock := btree.New(32)
	var closeBtree bool = true // It will be set to false in case of success at the end of the function
	defer func() {
		// Clean up all decompressor and indices upon error
		if closeBtree {
			closeFiles(byEndBlock)
		}
	}()
	// Scan the diff directory and create the mapping of end blocks to files
	files, err := os.ReadDir(diffDir)
	if err != nil {
		return nil, err
	}
	re := regexp.MustCompile(`(accounts|storage|code|commitment).([0-9]+)-([0-9]+).(dat|idx)`)
	for _, f := range files {
		name := f.Name()
		subs := re.FindStringSubmatch(name)
		if len(subs) != 5 {
			log.Warn("File ignored by aggregator, more than 4 submatches", "name", name)
			continue
		}
		var startBlock, endBlock uint64
		if startBlock, err = strconv.ParseUint(subs[2], 10, 64); err != nil {
			log.Warn("File ignored by aggregator, parsing startBlock", "error", err, "name", name)
			continue
		}
		if endBlock, err = strconv.ParseUint(subs[3], 10, 64); err != nil {
			log.Warn("File ignored by aggregator, parsing endBlock", "error", err, "name", name)
			continue
		}
		if startBlock > endBlock {
			log.Warn("File ignored by aggregator, startBlock > endBlock", "name", name)
			continue
		}
		var item *byEndBlockItem = &byEndBlockItem{fileCount: 1, startBlock: startBlock, endBlock: endBlock}
		var foundI *byEndBlockItem
		byEndBlock.AscendGreaterOrEqual(&byEndBlockItem{startBlock: endBlock, endBlock: endBlock}, func(i btree.Item) bool {
			it := i.(*byEndBlockItem)
			if it.endBlock == endBlock {
				foundI = it
			}
			return false
		})
		if foundI == nil {
			byEndBlock.ReplaceOrInsert(item)
		} else if foundI.startBlock > startBlock {
			byEndBlock.ReplaceOrInsert(item)
		} else if foundI.startBlock == startBlock {
			foundI.fileCount++
		}
	}
	// Check for overlaps and holes while moving items out of temporary btree
	a.byEndBlock = btree.New(32)
	var minStart uint64 = math.MaxUint64
	byEndBlock.Descend(func(i btree.Item) bool {
		item := i.(*byEndBlockItem)
		if item.startBlock < minStart {
			if item.endBlock >= minStart {
				err = fmt.Errorf("overlap of state files [%d-%d] with %d", item.startBlock, item.endBlock, minStart)
				return false
			}
			if minStart != math.MaxUint64 && item.endBlock+1 != minStart {
				err = fmt.Errorf("hole in state files [%d-%d]", item.endBlock, minStart)
				return false
			}
			if item.fileCount != 8 {
				err = fmt.Errorf("missing state files for interval [%d-%d]", item.startBlock, item.endBlock)
				return false
			}
			minStart = item.startBlock
			a.byEndBlock.ReplaceOrInsert(item)
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	a.byEndBlock.Ascend(func(i btree.Item) bool {
		item := i.(*byEndBlockItem)
		if item.accountsD, err = compress.NewDecompressor(path.Join(diffDir, fmt.Sprintf("accounts.%d-%d.dat", item.startBlock, item.endBlock))); err != nil {
			return false
		}
		if item.accountsIdx, err = recsplit.OpenIndex(path.Join(diffDir, fmt.Sprintf("accounts.%d-%d.idx", item.startBlock, item.endBlock))); err != nil {
			return false
		}
		if item.codeD, err = compress.NewDecompressor(path.Join(diffDir, fmt.Sprintf("code.%d-%d.dat", item.startBlock, item.endBlock))); err != nil {
			return false
		}
		if item.codeIdx, err = recsplit.OpenIndex(path.Join(diffDir, fmt.Sprintf("code.%d-%d.idx", item.startBlock, item.endBlock))); err != nil {
			return false
		}
		if item.storageD, err = compress.NewDecompressor(path.Join(diffDir, fmt.Sprintf("storage.%d-%d.dat", item.startBlock, item.endBlock))); err != nil {
			return false
		}
		if item.storageIdx, err = recsplit.OpenIndex(path.Join(diffDir, fmt.Sprintf("storage.%d-%d.idx", item.startBlock, item.endBlock))); err != nil {
			return false
		}
		if item.commitmentD, err = compress.NewDecompressor(path.Join(diffDir, fmt.Sprintf("commitment.%d-%d.dat", item.startBlock, item.endBlock))); err != nil {
			return false
		}
		if item.commitmentIdx, err = recsplit.OpenIndex(path.Join(diffDir, fmt.Sprintf("commitment.%d-%d.idx", item.startBlock, item.endBlock))); err != nil {
			return false
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	a.accountChanges.Init("accounts", aggregationStep, diffDir)
	a.codeChanges.Init("code", aggregationStep, diffDir)
	a.storageChanges.Init("storage", aggregationStep, diffDir)
	a.commChanges.Init("commitment", aggregationStep, diffDir)
	a.changesBtree = btree.New(32)
	re = regexp.MustCompile(`(accounts|storage|code|commitment).(keys|before|after).([0-9]+)-([0-9]+).chg`)
	for _, f := range files {
		name := f.Name()
		subs := re.FindStringSubmatch(name)
		if len(subs) != 5 {
			log.Warn("File ignored by changes scan, more than 4 submatches", "name", name)
			continue
		}
		var startBlock, endBlock uint64
		if startBlock, err = strconv.ParseUint(subs[3], 10, 64); err != nil {
			log.Warn("File ignored by changes scan, parsing startBlock", "error", err, "name", name)
			continue
		}
		if endBlock, err = strconv.ParseUint(subs[4], 10, 64); err != nil {
			log.Warn("File ignored by changes scan, parsing endBlock", "error", err, "name", name)
			continue
		}
		if startBlock > endBlock {
			log.Warn("File ignored by changes scan, startBlock > endBlock", "name", name)
			continue
		}
		if endBlock != startBlock+aggregationStep-1 {
			log.Warn("File ignored by changes scan, endBlock != startBlock+aggregationStep-1", "name", name)
			continue
		}
		var item *ChangesItem = &ChangesItem{fileCount: 1, startBlock: startBlock, endBlock: endBlock}
		i := a.changesBtree.Get(item)
		if i == nil {
			a.changesBtree.ReplaceOrInsert(item)
		} else {
			item = i.(*ChangesItem)
			if item.startBlock == startBlock {
				item.fileCount++
			} else {
				return nil, fmt.Errorf("change files overlap [%d-%d] with [%d-%d]", item.startBlock, item.endBlock, startBlock, endBlock)
			}
		}
	}
	// Check for holes in change files
	minStart = math.MaxUint64
	a.changesBtree.Descend(func(i btree.Item) bool {
		item := i.(*ChangesItem)
		if item.startBlock < minStart {
			if item.endBlock >= minStart {
				err = fmt.Errorf("overlap of change files [%d-%d] with %d", item.startBlock, item.endBlock, minStart)
				return false
			}
			if minStart != math.MaxUint64 && item.endBlock+1 != minStart {
				err = fmt.Errorf("whole in change files [%d-%d]", item.endBlock, minStart)
				return false
			}
			if item.fileCount != 12 {
				err = fmt.Errorf("missing change files for interval [%d-%d]", item.startBlock, item.endBlock)
				return false
			}
			minStart = item.startBlock
		} else {
			err = fmt.Errorf("overlap of change files [%d-%d] with %d", item.startBlock, item.endBlock, minStart)
			return false
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	if lastStateI := a.byEndBlock.Max(); lastStateI != nil {
		item := lastStateI.(*byEndBlockItem)
		if minStart != math.MaxUint64 && item.endBlock+1 != minStart {
			return nil, fmt.Errorf("hole or overlap between state files and change files [%d-%d]", item.endBlock, minStart)
		}
	}
	return a, nil
}

func closeFiles(byEndBlock *btree.BTree) {
	byEndBlock.Ascend(func(i btree.Item) bool {
		item := i.(*byEndBlockItem)
		if item.accountsD != nil {
			item.accountsD.Close()
		}
		if item.accountsIdx != nil {
			item.accountsIdx.Close()
		}
		if item.storageD != nil {
			item.storageD.Close()
		}
		if item.storageIdx != nil {
			item.storageIdx.Close()
		}
		if item.codeD != nil {
			item.codeD.Close()
		}
		if item.codeIdx != nil {
			item.codeIdx.Close()
		}
		if item.commitmentD != nil {
			item.commitmentD.Close()
		}
		if item.commitmentIdx != nil {
			item.commitmentIdx.Close()
		}
		return true
	})
}

func (a *Aggregator) Close() {
	a.accountChanges.closeFiles()
	a.codeChanges.closeFiles()
	a.storageChanges.closeFiles()
	a.commChanges.closeFiles()
	closeFiles(a.byEndBlock)
}

func (a *Aggregator) readAccount(blockNum uint64, addr []byte, trace bool) []byte {
	var val []byte
	a.byEndBlock.DescendLessOrEqual(&byEndBlockItem{endBlock: blockNum}, func(i btree.Item) bool {
		item := i.(*byEndBlockItem)
		if trace {
			fmt.Printf("readAccount %x: search in file [%d-%d]\n", addr, item.startBlock, item.endBlock)
		}
		if item.accountsIdx.Empty() {
			return true
		}
		offset := item.accountsIdx.Lookup(addr)
		g := item.accountsD.MakeGetter() // TODO Cache in the reader
		g.Reset(offset)
		if g.HasNext() {
			key, _ := g.Next(nil) // Add special function that just checks the key
			if bytes.Equal(key, addr) {
				val, _ = g.Next(nil)
				if trace {
					fmt.Printf("readAccount %x: found [%x] in file [%d-%d]\n", addr, val, item.startBlock, item.endBlock)
				}
				return false
			}
		}
		return true
	})
	if len(val) > 0 {
		return val[1:]
	}
	return nil
}

func (a *Aggregator) readCode(blockNum uint64, addr []byte, trace bool) []byte {
	var val []byte
	a.byEndBlock.DescendLessOrEqual(&byEndBlockItem{endBlock: blockNum}, func(i btree.Item) bool {
		item := i.(*byEndBlockItem)
		if trace {
			fmt.Printf("readCode %x: search in file [%d-%d]\n", addr, item.startBlock, item.endBlock)
		}
		if item.codeIdx.Empty() {
			return true
		}
		offset := item.codeIdx.Lookup(addr)
		g := item.codeD.MakeGetter() // TODO Cache in the reader
		g.Reset(offset)
		if g.HasNext() {
			key, _ := g.Next(nil) // Add special function that just checks the key
			if bytes.Equal(key, addr) {
				val, _ = g.Next(nil)
				if trace {
					fmt.Printf("readCode %x: found [%x] in file [%d-%d]\n", addr, val, item.startBlock, item.endBlock)
				}
				return false
			}
		}
		return true
	})
	if len(val) > 0 {
		return val[1:]
	}
	return nil
}

func (a *Aggregator) readStorage(blockNum uint64, filekey []byte, trace bool) []byte {
	var val []byte
	a.byEndBlock.DescendLessOrEqual(&byEndBlockItem{endBlock: blockNum}, func(i btree.Item) bool {
		item := i.(*byEndBlockItem)
		if trace {
			fmt.Printf("readStorage %x: search in file [%d-%d]\n", filekey, item.startBlock, item.endBlock)
		}
		if item.storageIdx.Empty() {
			return true
		}
		offset := item.storageIdx.Lookup(filekey)
		g := item.storageD.MakeGetter() // TODO Cache in the reader
		g.Reset(offset)
		if g.HasNext() {
			key, _ := g.Next(nil) // Add special function that just checks the key
			if bytes.Equal(key, filekey) {
				val, _ = g.Next(nil)
				if trace {
					fmt.Printf("readStorage %x: found [%x] in file [%d-%d]\n", filekey, val, item.startBlock, item.endBlock)
				}
				return false
			}
		}
		return true
	})
	if len(val) > 0 {
		return val[1:]
	}
	return nil
}

func (a *Aggregator) MakeStateReader(tx kv.Getter, blockNum uint64) *Reader {
	r := &Reader{
		a:        a,
		tx:       tx,
		blockNum: blockNum,
	}
	return r
}

type Reader struct {
	a        *Aggregator
	tx       kv.Getter
	blockNum uint64
}

func (r *Reader) ReadAccountData(addr []byte, trace bool) ([]byte, error) {
	// Look in the summary table first
	v, err := r.tx.GetOne(kv.StateAccounts, addr)
	if err != nil {
		return nil, err
	}
	if v != nil {
		// First 4 bytes is the number of 1-block state diffs containing the key
		if trace {
			fmt.Printf("ReadAccountData %x, found in DB: %x, number of diffs: %d\n", addr, v[4:], binary.BigEndian.Uint32(v[:4]))
		}
		return v[4:], nil
	}
	// Look in the files
	val := r.a.readAccount(r.blockNum, addr, trace)
	return val, nil
}

func (r *Reader) ReadAccountStorage(addr []byte, loc []byte, trace bool) (*uint256.Int, error) {
	// Look in the summary table first
	dbkey := make([]byte, len(addr)+len(loc))
	copy(dbkey[0:], addr)
	copy(dbkey[len(addr):], loc)
	v, err := r.tx.GetOne(kv.StateStorage, dbkey)
	if err != nil {
		return nil, err
	}
	if v != nil {
		if trace {
			fmt.Printf("ReadAccountStorage %x %x, found in DB: %x, number of diffs: %d\n", addr, loc, v[4:], binary.BigEndian.Uint32(v[:4]))
		}
		if len(v) == 4 {
			return nil, nil
		}
		// First 4 bytes is the number of 1-block state diffs containing the key
		return new(uint256.Int).SetBytes(v[4:]), nil
	}
	// Look in the files
	val := r.a.readStorage(r.blockNum, dbkey, trace)
	if val != nil {
		return new(uint256.Int).SetBytes(val), nil
	}
	return nil, nil
}

func (r *Reader) ReadAccountCode(addr []byte, trace bool) ([]byte, error) {
	// Look in the summary table first
	v, err := r.tx.GetOne(kv.StateCode, addr)
	if err != nil {
		return nil, err
	}
	if v != nil {
		// First 4 bytes is the number of 1-block state diffs containing the key
		if trace {
			fmt.Printf("ReadAccountCode %x, found in DB: %x, number of diffs: %d\n", addr, v[4:], binary.BigEndian.Uint32(v[:4]))
		}
		return v[4:], nil
	}
	// Look in the files
	val := r.a.readCode(r.blockNum, addr, trace)
	return val, nil
}

type Writer struct {
	a        *Aggregator
	tx       kv.RwTx
	blockNum uint64
}

func (a *Aggregator) MakeStateWriter(tx kv.RwTx, blockNum uint64) (*Writer, error) {
	w := &Writer{
		a: a,
	}
	if err := w.Reset(tx, blockNum); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Writer) Reset(tx kv.RwTx, blockNum uint64) error {
	w.tx = tx
	w.blockNum = blockNum
	if blockNum > w.a.changeFileNum {
		if err := w.a.accountChanges.closeFiles(); err != nil {
			return err
		}
		if err := w.a.codeChanges.closeFiles(); err != nil {
			return err
		}
		if err := w.a.storageChanges.closeFiles(); err != nil {
			return err
		}
		if err := w.a.commChanges.closeFiles(); err != nil {
			return err
		}
		w.a.changesBtree.ReplaceOrInsert(&ChangesItem{startBlock: w.a.changeFileNum + 1 - w.a.aggregationStep, endBlock: w.a.changeFileNum, fileCount: 12})
	}
	if w.a.changeFileNum == 0 || blockNum > w.a.changeFileNum {
		if err := w.a.accountChanges.openFiles(blockNum, true /* write */); err != nil {
			return err
		}
		if err := w.a.codeChanges.openFiles(blockNum, true /* write */); err != nil {
			return err
		}
		if err := w.a.storageChanges.openFiles(blockNum, true /* write */); err != nil {
			return err
		}
		if err := w.a.commChanges.openFiles(blockNum, true /* write */); err != nil {
			return err
		}
		w.a.changeFileNum = blockNum - (blockNum % w.a.aggregationStep) + w.a.aggregationStep - 1
	}
	return nil
}

func (w *Writer) Finish() error {
	if err := w.a.accountChanges.finish(w.blockNum); err != nil {
		return fmt.Errorf("finish accountChanges: %w", err)
	}
	if err := w.a.codeChanges.finish(w.blockNum); err != nil {
		return fmt.Errorf("finish codeChanges: %w", err)
	}
	if err := w.a.storageChanges.finish(w.blockNum); err != nil {
		return fmt.Errorf("finish storageChanges: %w", err)
	}
	if err := w.a.commChanges.finish(w.blockNum); err != nil {
		return fmt.Errorf("finish commChanges: %w", err)
	}
	if w.blockNum < w.a.unwindLimit+w.a.aggregationStep-1 {
		return nil
	}
	diff := w.blockNum - w.a.unwindLimit
	if (diff+1)%w.a.aggregationStep != 0 {
		return nil
	}
	if err := w.aggregateUpto(diff+1-w.a.aggregationStep, diff); err != nil {
		return fmt.Errorf("aggregateUpto(%d, %d): %w", diff+1-w.a.aggregationStep, diff, err)
	}
	return nil
}

func (w *Writer) UpdateAccountData(addr []byte, account []byte, trace bool) error {
	prevV, err := w.tx.GetOne(kv.StateAccounts, addr)
	if err != nil {
		return err
	}
	var prevNum uint32
	var original []byte
	if prevV == nil {
		original = w.a.readAccount(w.blockNum, addr, trace)
	} else {
		prevNum = binary.BigEndian.Uint32(prevV[:4])
	}
	v := make([]byte, 4+len(account))
	binary.BigEndian.PutUint32(v[:4], prevNum+1)
	copy(v[4:], account)
	if err = w.tx.Put(kv.StateAccounts, addr, v); err != nil {
		return err
	}
	if prevV == nil && original == nil {
		w.a.accountChanges.insert(addr, account)
	} else {
		if original == nil {
			original = prevV[4:]
		}
		w.a.accountChanges.update(addr, original, account)
	}
	if trace {
		w.a.trace = true
		w.a.tracedKeys[string(addr)] = struct{}{}
	}
	return nil
}

func (w *Writer) UpdateAccountCode(addr []byte, code []byte, trace bool) error {
	prevV, err := w.tx.GetOne(kv.StateCode, addr)
	if err != nil {
		return err
	}
	var prevNum uint32
	var original []byte
	if prevV == nil {
		original = w.a.readCode(w.blockNum, addr, trace)
	} else {
		prevNum = binary.BigEndian.Uint32(prevV[:4])
	}
	v := make([]byte, 4+len(code))
	binary.BigEndian.PutUint32(v[:4], prevNum+1)
	copy(v[4:], code)
	if err = w.tx.Put(kv.StateCode, addr, v); err != nil {
		return err
	}
	if prevV == nil && original == nil {
		w.a.codeChanges.insert(addr, code)
	} else {
		if original == nil {
			original = prevV[4:]
		}
		w.a.codeChanges.update(addr, original, code)
	}
	if trace {
		w.a.trace = true
		w.a.tracedKeys[string(addr)] = struct{}{}
	}
	return nil
}

// CursorItem is the item in the priority queue used to do merge interation
// over storage of a given account
type CursorItem struct {
	file     bool // Whether this item represents state file or DB record
	endBlock uint64
	key, val []byte
	dg       *compress.Getter
	c        kv.Cursor
}

type CursorHeap []*CursorItem

func (ch CursorHeap) Len() int {
	return len(ch)
}

func (ch CursorHeap) Less(i, j int) bool {
	cmp := bytes.Compare(ch[i].key, ch[j].key)
	if cmp == 0 {
		// when keys match, the items with later blocks are preferred
		return ch[i].endBlock > ch[j].endBlock
	}
	return cmp < 0
}

func (ch *CursorHeap) Swap(i, j int) {
	(*ch)[i], (*ch)[j] = (*ch)[j], (*ch)[i]
}

func (ch *CursorHeap) Push(x interface{}) {
	*ch = append(*ch, x.(*CursorItem))
}

func (ch *CursorHeap) Pop() interface{} {
	old := *ch
	n := len(old)
	x := old[n-1]
	*ch = old[0 : n-1]
	return x
}

func (w *Writer) deleteAccount(addr []byte, trace bool) error {
	prevV, err := w.tx.GetOne(kv.StateAccounts, addr)
	if err != nil {
		return err
	}
	var prevNum uint32
	var original []byte
	if prevV == nil {
		original = w.a.readAccount(w.blockNum, addr, trace)
	} else {
		prevNum = binary.BigEndian.Uint32(prevV[:4])
	}
	v := make([]byte, 4)
	binary.BigEndian.PutUint32(v[:4], prevNum+1)
	if err = w.tx.Put(kv.StateAccounts, addr, v); err != nil {
		return err
	}
	if prevV == nil && original == nil {
		return fmt.Errorf("previous value expected for DeleteAccount")
	} else if original == nil {
		original = prevV[4:]
	}
	w.a.accountChanges.delete(addr, original)
	return nil
}

func (w *Writer) deleteCode(addr []byte, trace bool) error {
	prevV, err := w.tx.GetOne(kv.StateCode, addr)
	if err != nil {
		return err
	}
	var prevNum uint32
	var original []byte
	if prevV == nil {
		original = w.a.readCode(w.blockNum, addr, trace)
	} else {
		prevNum = binary.BigEndian.Uint32(prevV[:4])
	}
	v := make([]byte, 4)
	binary.BigEndian.PutUint32(v[:4], prevNum+1)
	if err = w.tx.Put(kv.StateCode, addr, v); err != nil {
		return err
	}
	if prevV == nil && original == nil {
		// Nothing to do
		return nil
	} else if original == nil {
		original = prevV[4:]
	}
	w.a.codeChanges.delete(addr, original)
	return nil
}

func (w *Writer) DeleteAccount(addr []byte, trace bool) error {
	if err := w.deleteAccount(addr, trace); err != nil {
		return err
	}
	if err := w.deleteCode(addr, trace); err != nil {
		return err
	}
	// Find all storage items for this address
	var cp CursorHeap
	heap.Init(&cp)
	c, err := w.tx.Cursor(kv.StateStorage)
	if err != nil {
		return err
	}
	var k, v []byte
	if k, v, err = c.Seek(addr); err != nil {
		return err
	}
	if k != nil && bytes.HasPrefix(k, addr) {
		heap.Push(&cp, &CursorItem{file: false, key: common.Copy(k), val: common.Copy(v), c: c, endBlock: w.blockNum})
	}
	w.a.byEndBlock.Ascend(func(i btree.Item) bool {
		item := i.(*byEndBlockItem)
		if item.storageIdx.Empty() {
			return true
		}
		offset := item.storageIdx.Lookup(addr)
		g := item.storageD.MakeGetter() // TODO Cache in the reader
		g.Reset(offset)
		if g.HasNext() {
			key, _ := g.Next(nil) // Add special function that just checks the key
			if !bytes.Equal(key, addr) {
				return true
			}
			g.Next(nil)
		}
		if g.HasNext() {
			key, _ := g.Next(nil)
			if bytes.HasPrefix(key, addr) {
				val, _ := g.Next(nil)
				heap.Push(&cp, &CursorItem{file: true, key: key, val: val, dg: g, endBlock: item.endBlock})
			}
		}
		return true
	})
	for cp.Len() > 0 {
		lastKey := common.Copy(cp[0].key)
		lastVal := common.Copy(cp[0].val)
		// Advance all the items that have this key (including the top)
		for cp.Len() > 0 && bytes.Equal(cp[0].key, lastKey) {
			ci1 := cp[0]
			if ci1.file {
				if ci1.dg.HasNext() {
					ci1.key, _ = ci1.dg.Next(ci1.key[:0])
					if bytes.HasPrefix(ci1.key, addr) {
						ci1.val, _ = ci1.dg.Next(ci1.val[:0])
						heap.Fix(&cp, 0)
					} else {
						heap.Pop(&cp)
					}
				} else {
					heap.Pop(&cp)
				}
			} else {
				k, v, err = ci1.c.Next()
				if err != nil {
					return err
				}
				if k != nil && bytes.HasPrefix(k, addr) {
					ci1.key = common.Copy(k)
					ci1.val = common.Copy(v)
					heap.Fix(&cp, 0)
				} else {
					heap.Pop(&cp)
				}
			}
		}
		var prevV []byte
		prevV, err = w.tx.GetOne(kv.StateStorage, lastKey)
		if err != nil {
			return err
		}
		var prevNum uint32
		if prevV != nil {
			prevNum = binary.BigEndian.Uint32(prevV[:4])
		}
		v = make([]byte, 4)
		binary.BigEndian.PutUint32(v[:4], prevNum+1)
		if err = w.tx.Put(kv.StateStorage, lastKey, v); err != nil {
			return err
		}
		w.a.storageChanges.delete(lastKey, lastVal)
	}
	if trace {
		w.a.trace = true
		w.a.tracedKeys[string(addr)] = struct{}{}
	}
	return nil
}

func (w *Writer) WriteAccountStorage(addr []byte, loc []byte, value *uint256.Int, trace bool) error {
	dbkey := make([]byte, len(addr)+len(loc))
	copy(dbkey[0:], addr)
	copy(dbkey[len(addr):], loc)
	prevV, err := w.tx.GetOne(kv.StateStorage, dbkey)
	if err != nil {
		return err
	}
	var prevNum uint32
	var original []byte
	if prevV == nil {
		original = w.a.readStorage(w.blockNum, dbkey, trace)
	} else {
		prevNum = binary.BigEndian.Uint32(prevV[:4])
	}
	vLen := value.ByteLen()
	v := make([]byte, 4+vLen)
	binary.BigEndian.PutUint32(v[:4], prevNum+1)
	value.WriteToSlice(v[4:])
	if err = w.tx.Put(kv.StateStorage, dbkey, v); err != nil {
		return err
	}
	if prevV == nil {
		w.a.storageChanges.insert(dbkey, v[4:])
	} else {
		if original == nil {
			original = prevV[4:]
		}
		w.a.storageChanges.update(dbkey, original, v[4:])
	}
	if trace {
		w.a.trace = true
		w.a.tracedKeys[string(dbkey)] = struct{}{}
	}
	return nil
}

func (w *Writer) aggregateUpto(blockFrom, blockTo uint64) error {
	i := w.a.changesBtree.Get(&ChangesItem{startBlock: blockFrom, endBlock: blockTo})
	if i == nil {
		return fmt.Errorf("did not find change files for [%d-%d], w.a.changesBtree.Len() = %d", blockFrom, blockTo, w.a.changesBtree.Len())
	}
	item := i.(*ChangesItem)
	if item.startBlock != blockFrom {
		return fmt.Errorf("expected change files[%d-%d], got [%d-%d]", blockFrom, blockTo, item.startBlock, item.endBlock)
	}
	var accountChanges, codeChanges, storageChanges, commChanges Changes
	accountChanges.Init("accounts", w.a.aggregationStep, w.a.diffDir)
	codeChanges.Init("code", w.a.aggregationStep, w.a.diffDir)
	storageChanges.Init("storage", w.a.aggregationStep, w.a.diffDir)
	commChanges.Init("commitment", w.a.aggregationStep, w.a.diffDir)
	var err error
	var item1 *byEndBlockItem = &byEndBlockItem{fileCount: 8, startBlock: blockFrom, endBlock: blockTo}
	if item1.accountsD, item1.accountsIdx, err = accountChanges.aggregate(blockFrom, blockTo, 0, w.tx, kv.StateAccounts); err != nil {
		return fmt.Errorf("aggregate accountsChanges: %w", err)
	}
	if item1.codeD, item1.codeIdx, err = codeChanges.aggregate(blockFrom, blockTo, 0, w.tx, kv.StateCode); err != nil {
		return fmt.Errorf("aggregate codeChanges: %w", err)
	}
	if item1.storageD, item1.storageIdx, err = storageChanges.aggregate(blockFrom, blockTo, 20, w.tx, kv.StateStorage); err != nil {
		return fmt.Errorf("aggregate storageChanges: %w", err)
	}
	if item1.commitmentD, item1.commitmentIdx, err = commChanges.aggregate(blockFrom, blockTo, 20, w.tx, kv.StateCommitment); err != nil {
		return fmt.Errorf("aggregate storageChanges: %w", err)
	}
	if err = accountChanges.closeFiles(); err != nil {
		return err
	}
	if err = codeChanges.closeFiles(); err != nil {
		return err
	}
	if err = storageChanges.closeFiles(); err != nil {
		return err
	}
	if err = commChanges.closeFiles(); err != nil {
		return err
	}
	if err = accountChanges.deleteFiles(); err != nil {
		return err
	}
	if err = codeChanges.deleteFiles(); err != nil {
		return err
	}
	if err = storageChanges.deleteFiles(); err != nil {
		return err
	}
	if err = commChanges.deleteFiles(); err != nil {
		return err
	}
	w.a.byEndBlock.ReplaceOrInsert(item1)
	// Now aggregate state files
	var toAggregate []*byEndBlockItem
	toAggregate = append(toAggregate, item1)
	lastStart := blockFrom
	nextSize := blockTo - blockFrom + 1
	nextEnd := blockFrom - 1
	nextStart := nextEnd - nextSize + 1
	var nextI *byEndBlockItem
	w.a.byEndBlock.AscendGreaterOrEqual(&byEndBlockItem{startBlock: nextEnd, endBlock: nextEnd}, func(i btree.Item) bool {
		item := i.(*byEndBlockItem)
		if item.endBlock == nextEnd {
			nextI = item
		}
		return false
	})
	for nextI != nil {
		if nextI.startBlock != nextStart {
			break
		}
		lastStart = nextStart
		toAggregate = append(toAggregate, nextI)
		nextSize *= 2
		nextEnd = nextStart - 1
		nextStart = nextEnd - nextSize + 1
		nextI = nil
		w.a.byEndBlock.AscendGreaterOrEqual(&byEndBlockItem{startBlock: nextEnd, endBlock: nextEnd}, func(i btree.Item) bool {
			item := i.(*byEndBlockItem)
			if item.endBlock == nextEnd {
				nextI = item
			}
			return false
		})
	}
	if len(toAggregate) == 1 {
		// Nothing to aggregate yet
		return nil
	}
	var item2 *byEndBlockItem = &byEndBlockItem{fileCount: 6, startBlock: lastStart, endBlock: blockTo}
	var cp CursorHeap
	heap.Init(&cp)
	for _, ag := range toAggregate {
		g := ag.accountsD.MakeGetter()
		if g.HasNext() {
			key, _ := g.Next(nil)
			val, _ := g.Next(nil)
			heap.Push(&cp, &CursorItem{file: true, dg: g, key: key, val: val, endBlock: ag.endBlock})
		}
	}
	if item2.accountsD, item2.accountsIdx, err = w.a.mergeIntoStateFile(&cp, 0, "accounts", lastStart, blockTo, w.a.diffDir); err != nil {
		return fmt.Errorf("mergeIntoStateFile accounts [%d-%d]: %w", lastStart, blockTo, err)
	}
	cp = cp[:0]
	heap.Init(&cp)
	for _, ag := range toAggregate {
		g := ag.codeD.MakeGetter()
		if g.HasNext() {
			key, _ := g.Next(nil)
			val, _ := g.Next(nil)
			heap.Push(&cp, &CursorItem{file: true, dg: g, key: key, val: val, endBlock: ag.endBlock})
		}
	}
	if item2.codeD, item2.codeIdx, err = w.a.mergeIntoStateFile(&cp, 0, "code", lastStart, blockTo, w.a.diffDir); err != nil {
		return fmt.Errorf("mergeIntoStateFile code [%d-%d]: %w", lastStart, blockTo, err)
	}
	cp = cp[:0]
	heap.Init(&cp)
	for _, ag := range toAggregate {
		g := ag.storageD.MakeGetter()
		if g.HasNext() {
			key, _ := g.Next(nil)
			val, _ := g.Next(nil)
			heap.Push(&cp, &CursorItem{file: true, dg: g, key: key, val: val, endBlock: ag.endBlock})
		}
	}
	if item2.storageD, item2.storageIdx, err = w.a.mergeIntoStateFile(&cp, 20, "storage", lastStart, blockTo, w.a.diffDir); err != nil {
		return fmt.Errorf("mergeIntoStateFile storage [%d-%d]: %w", lastStart, blockTo, err)
	}
	cp = cp[:0]
	heap.Init(&cp)
	for _, ag := range toAggregate {
		g := ag.commitmentD.MakeGetter()
		if g.HasNext() {
			key, _ := g.Next(nil)
			val, _ := g.Next(nil)
			heap.Push(&cp, &CursorItem{file: true, dg: g, key: key, val: val, endBlock: ag.endBlock})
		}
	}
	if item2.commitmentD, item2.commitmentIdx, err = w.a.mergeIntoStateFile(&cp, 20, "commitment", lastStart, blockTo, w.a.diffDir); err != nil {
		return fmt.Errorf("mergeIntoStateFile commitment [%d-%d]: %w", lastStart, blockTo, err)
	}
	// Remove all items in toAggregate and insert item2 instead
	w.a.byEndBlock.ReplaceOrInsert(item2)
	for _, ag := range toAggregate {
		w.a.byEndBlock.Delete(ag)
	}
	// Close all the memory maps etc
	for _, ag := range toAggregate {
		if err = ag.accountsIdx.Close(); err != nil {
			return err
		}
		if err = ag.accountsD.Close(); err != nil {
			return err
		}
		if err = ag.codeIdx.Close(); err != nil {
			return err
		}
		if err = ag.codeD.Close(); err != nil {
			return err
		}
		if err = ag.storageIdx.Close(); err != nil {
			return err
		}
		if err = ag.storageD.Close(); err != nil {
			return err
		}
		if err = ag.commitmentIdx.Close(); err != nil {
			return err
		}
		if err = ag.commitmentD.Close(); err != nil {
			return err
		}
	}
	// Delete files
	// TODO: in a non-test version, this is delayed to allow other participants to roll over to the next file
	for _, ag := range toAggregate {
		if err = os.Remove(path.Join(w.a.diffDir, fmt.Sprintf("accounts.%d-%d.dat", ag.startBlock, ag.endBlock))); err != nil {
			return err
		}
		if err = os.Remove(path.Join(w.a.diffDir, fmt.Sprintf("accounts.%d-%d.idx", ag.startBlock, ag.endBlock))); err != nil {
			return err
		}
		if err = os.Remove(path.Join(w.a.diffDir, fmt.Sprintf("code.%d-%d.dat", ag.startBlock, ag.endBlock))); err != nil {
			return err
		}
		if err = os.Remove(path.Join(w.a.diffDir, fmt.Sprintf("code.%d-%d.idx", ag.startBlock, ag.endBlock))); err != nil {
			return err
		}
		if err = os.Remove(path.Join(w.a.diffDir, fmt.Sprintf("storage.%d-%d.dat", ag.startBlock, ag.endBlock))); err != nil {
			return err
		}
		if err = os.Remove(path.Join(w.a.diffDir, fmt.Sprintf("storage.%d-%d.idx", ag.startBlock, ag.endBlock))); err != nil {
			return err
		}
		if err = os.Remove(path.Join(w.a.diffDir, fmt.Sprintf("commitment.%d-%d.dat", ag.startBlock, ag.endBlock))); err != nil {
			return err
		}
		if err = os.Remove(path.Join(w.a.diffDir, fmt.Sprintf("commitment.%d-%d.idx", ag.startBlock, ag.endBlock))); err != nil {
			return err
		}
	}
	w.a.changesBtree.Delete(i)
	return nil
}

func (a *Aggregator) mergeIntoStateFile(cp *CursorHeap, prefixLen int, basename string, startBlock, endBlock uint64, dir string) (*compress.Decompressor, *recsplit.Index, error) {
	datPath := path.Join(dir, fmt.Sprintf("%s.%d-%d.dat", basename, startBlock, endBlock))
	idxPath := path.Join(dir, fmt.Sprintf("%s.%d-%d.idx", basename, startBlock, endBlock))
	var comp *compress.Compressor
	var err error
	if comp, err = compress.NewCompressor(AggregatorPrefix, datPath, dir, 1024 /* minPatterScore */); err != nil {
		return nil, nil, fmt.Errorf("compressor %s: %w", datPath, err)
	}
	count := 0
	var keyBuf, valBuf []byte
	for cp.Len() > 0 {
		lastKey := common.Copy((*cp)[0].key)
		lastVal := common.Copy((*cp)[0].val)
		if a.trace {
			if _, ok := a.tracedKeys[string(lastKey)]; ok {
				fmt.Printf("looking at key %x val [%x] endBlock %d to merge into [%d-%d]\n", lastKey, lastVal, (*cp)[0].endBlock, startBlock, endBlock)
			}
		}
		var first, firstDelete, firstInsert bool
		// Advance all the items that have this key (including the top)
		for cp.Len() > 0 && bytes.Equal((*cp)[0].key, lastKey) {
			ci1 := (*cp)[0]
			if a.trace {
				if _, ok := a.tracedKeys[string(ci1.key)]; ok {
					fmt.Printf("skipping same key %x val [%x] endBlock %d to merge into [%d-%d]\n", ci1.key, ci1.val, ci1.endBlock, startBlock, endBlock)
				}
			}
			first = true
			firstDelete = len(ci1.val) == 0
			firstInsert = !firstDelete && ci1.val[0] != 0
			if ci1.dg.HasNext() {
				ci1.key, _ = ci1.dg.Next(ci1.key[:0])
				ci1.val, _ = ci1.dg.Next(ci1.val[:0])
				heap.Fix(cp, 0)
			} else {
				heap.Pop(cp)
			}
		}
		lastDelete := len(lastVal) == 0
		lastInsert := !lastDelete && lastVal[0] != 0
		var skip bool
		if first {
			if firstInsert {
				if lastDelete {
					// Insert => Delete
					skip = true
				}
			} else if firstDelete {
				if lastInsert {
					// Delete => Insert equivalent to Update
					lastVal[0] = 0
				}
			} else {
				if lastInsert {
					// Update => Insert equivalent to Update
					lastVal[0] = 0
				}
			}
		}
		if startBlock != 0 || !skip {
			if keyBuf != nil && (prefixLen == 0 || len(keyBuf) != prefixLen || bytes.HasPrefix(lastKey, keyBuf)) {
				if err = comp.AddWord(keyBuf); err != nil {
					return nil, nil, err
				}
				if a.trace {
					if _, ok := a.tracedKeys[string(keyBuf)]; ok {
						fmt.Printf("merge key %x val [%x] into [%d-%d]\n", keyBuf, valBuf, startBlock, endBlock)
					}
				}
				count++ // Only counting keys, not values
				if err = comp.AddWord(valBuf); err != nil {
					return nil, nil, err
				}
			}
			keyBuf = append(keyBuf[:0], lastKey...)
			valBuf = append(valBuf[:0], lastVal...)
		} else if a.trace {
			if _, ok := a.tracedKeys[string(keyBuf)]; ok {
				fmt.Printf("skipped key %x for [%d-%d]\n", keyBuf, startBlock, endBlock)
			}
		}
	}
	if keyBuf != nil {
		if err = comp.AddWord(keyBuf); err != nil {
			return nil, nil, err
		}
		if a.trace {
			if _, ok := a.tracedKeys[string(keyBuf)]; ok {
				fmt.Printf("merge key %x val [%x] into [%d-%d]\n", keyBuf, valBuf, startBlock, endBlock)
			}
		}
		count++ // Only counting keys, not values
		if err = comp.AddWord(valBuf); err != nil {
			return nil, nil, err
		}
	}
	if err = comp.Compress(); err != nil {
		return nil, nil, err
	}
	var d *compress.Decompressor
	var idx *recsplit.Index
	if d, idx, err = buildIndex(datPath, idxPath, dir, count); err != nil {
		return nil, nil, fmt.Errorf("build index: %w", err)
	}
	return d, idx, nil
}
