package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// partHeader mirrors metadata.json on disk.
type partHeader struct {
	ItemsCount  uint64 `json:"itemsCount"`
	BlocksCount uint64 `json:"blocksCount"`
	FirstItem   string `json:"firstItem"`
	LastItem    string `json:"lastItem"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: indexdb-reader <part-directory>\n")
		os.Exit(1)
	}
	partDir := os.Args[1]

	// --- Step 1: Read metadata.json ---
	data, err := os.ReadFile(partDir + "/metadata.json")
	if err != nil {
		fatalf("read metadata.json: %v", err)
	}
	var ph partHeader
	if err := json.Unmarshal(data, &ph); err != nil {
		fatalf("parse metadata.json: %v", err)
	}

	fmt.Printf("=== Part Header ===\n")
	fmt.Printf("  items:  %d\n", ph.ItemsCount)
	fmt.Printf("  blocks: %d\n", ph.BlocksCount)

	firstItem, _ := hex.DecodeString(ph.FirstItem)
	lastItem, _ := hex.DecodeString(ph.LastItem)
	fmt.Printf("  firstItem (%d bytes): %x\n", len(firstItem), firstItem)
	fmt.Printf("  lastItem  (%d bytes): %x\n", len(lastItem), lastItem)

	// --- Step 2: Read and decompress metaindex.bin ---
	//
	// metaindex.bin is ZSTD-compressed. After decompression it contains
	// a sequence of metaindex rows packed back-to-back. Each row:
	//
	//   marshalBytes(firstItem)   — VarUint64(len) + raw bytes
	//   Uint32(blockHeadersCount)          — big-endian, 4 bytes
	//   Uint64(indexBlockOffset)           — big-endian, 8 bytes
	//   Uint32(indexBlockSize)             — big-endian, 4 bytes
	//
	// The metaindex tells us where to find index blocks inside index.bin.
	metaindexCompressed, err := os.ReadFile(partDir + "/metaindex.bin")
	if err != nil {
		fatalf("read metaindex.bin: %v", err)
	}
	metaindexData, err := decompressZSTD(metaindexCompressed)
	if err != nil {
		fatalf("decompress metaindex.bin: %v", err)
	}

	fmt.Printf("\n=== Metaindex ===\n")
	fmt.Printf("  compressed: %d bytes, decompressed: %d bytes\n",
		len(metaindexCompressed), len(metaindexData))

	src := metaindexData
	rowIdx := 0
	for len(src) > 0 {
		// Unmarshal firstItem: VarUint64-prefixed byte slice
		fi, n := unmarshalBytes(src)
		if n <= 0 {
			fatalf("metaindex row %d: cannot unmarshal firstItem", rowIdx)
		}
		src = src[n:]

		if len(src) < 4+8+4 {
			fatalf("metaindex row %d: not enough bytes for fixed fields", rowIdx)
		}
		blockHeadersCount := binary.BigEndian.Uint32(src)
		src = src[4:]
		indexBlockOffset := binary.BigEndian.Uint64(src)
		src = src[8:]
		indexBlockSize := binary.BigEndian.Uint32(src)
		src = src[4:]

		fmt.Printf("  row %d:\n", rowIdx)
		fmt.Printf("    firstItem (%d bytes): %x\n", len(fi), fi)
		fmt.Printf("    blockHeadersCount:    %d\n", blockHeadersCount)
		fmt.Printf("    indexBlockOffset:      %d\n", indexBlockOffset)
		fmt.Printf("    indexBlockSize:        %d\n", indexBlockSize)
		rowIdx++

		// --- Step 3: Read and decompress index block from index.bin ---
		//
		// index.bin contains ZSTD-compressed index blocks. Each metaindex row
		// tells us where an index block lives (offset + size). After decompression,
		// an index block contains blockHeadersCount block headers packed
		// back-to-back. Each block header:
		//
		//   marshalBytes(commonPrefix)  — shared prefix for all items
		//   marshalBytes(firstItem)     — first item in the block
		//   Uint8(marshalType)                   — 0=plain, 1=ZSTD
		//   Uint32(itemsCount)                   — items EXCLUDING first item
		//   Uint64(itemsBlockOffset)             — byte offset in items.bin
		//   Uint64(lensBlockOffset)              — byte offset in lens.bin
		//   Uint32(itemsBlockSize)               — byte size in items.bin
		//   Uint32(lensBlockSize)                — byte size in lens.bin
		//
		// The block header tells us where to find the actual item data.
		indexData, err := os.ReadFile(partDir + "/index.bin")
		if err != nil {
			fatalf("read index.bin: %v", err)
		}
		indexBlock := indexData[indexBlockOffset : indexBlockOffset+uint64(indexBlockSize)]
		indexBlockDecompressed, err := decompressZSTD(indexBlock)
		if err != nil {
			fatalf("decompress index block: %v", err)
		}

		bhSrc := indexBlockDecompressed
		for bhIdx := 0; bhIdx < int(blockHeadersCount); bhIdx++ {
			// commonPrefix
			commonPrefix, n := unmarshalBytes(bhSrc)
			if n <= 0 {
				fatalf("block header %d: cannot unmarshal commonPrefix", bhIdx)
			}
			bhSrc = bhSrc[n:]

			// firstItem
			bhFirstItem, n := unmarshalBytes(bhSrc)
			if n <= 0 {
				fatalf("block header %d: cannot unmarshal firstItem", bhIdx)
			}
			bhSrc = bhSrc[n:]

			// marshalType (1 byte): 0=plain, 1=ZSTD
			if len(bhSrc) < 1 {
				fatalf("block header %d: cannot unmarshal marshalType", bhIdx)
			}
			marshalType := bhSrc[0]
			bhSrc = bhSrc[1:]

			// Fixed fields: itemsCount(4) + itemsBlockOffset(8) + lensBlockOffset(8) + itemsBlockSize(4) + lensBlockSize(4)
			if len(bhSrc) < 4+8+8+4+4 {
				fatalf("block header %d: not enough bytes for fixed fields", bhIdx)
			}
			itemsCount := binary.BigEndian.Uint32(bhSrc)
			bhSrc = bhSrc[4:]
			itemsBlockOffset := binary.BigEndian.Uint64(bhSrc)
			bhSrc = bhSrc[8:]
			lensBlockOffset := binary.BigEndian.Uint64(bhSrc)
			bhSrc = bhSrc[8:]
			itemsBlockSize := binary.BigEndian.Uint32(bhSrc)
			bhSrc = bhSrc[4:]
			lensBlockSize := binary.BigEndian.Uint32(bhSrc)
			bhSrc = bhSrc[4:]

			marshalTypeName := "plain"
			if marshalType == 1 {
				marshalTypeName = "zstd"
			}

			fmt.Printf("    block header %d:\n", bhIdx)
			fmt.Printf("      commonPrefix (%d bytes): %x\n", len(commonPrefix), commonPrefix)
			fmt.Printf("      firstItem    (%d bytes): %x\n", len(bhFirstItem), bhFirstItem)
			fmt.Printf("      marshalType:             %d (%s)\n", marshalType, marshalTypeName)
			fmt.Printf("      itemsCount:              %d (total items = %d including first)\n", itemsCount, itemsCount+1)
			fmt.Printf("      itemsBlockOffset:        %d\n", itemsBlockOffset)
			fmt.Printf("      lensBlockOffset:         %d\n", lensBlockOffset)
			fmt.Printf("      itemsBlockSize:          %d\n", itemsBlockSize)
			fmt.Printf("      lensBlockSize:           %d\n", lensBlockSize)

			// --- Step 4: Decode items from items.bin + lens.bin ---
			//
			// For marshalType=1 (ZSTD), both files are ZSTD-compressed.
			//
			// lens.bin (after decompression) contains two VarUint64 arrays
			// packed back-to-back, each with (itemsCount - 1) elements:
			//
			//   prefixLensXOR[0..itemsCount-2]  — XOR-delta encoded prefix lengths
			//   itemLensXOR[0..itemsCount-2]    — XOR-delta encoded item lengths
			//
			// To recover actual values, undo the XOR-delta:
			//   prefixLens[0] = 0
			//   prefixLens[i] = prefixLensXOR[i-1] ^ prefixLens[i-1]
			//
			//   itemLens[0] = len(firstItem) - len(commonPrefix)
			//   itemLens[i] = itemLensXOR[i-1] ^ itemLens[i-1]
			//
			// items.bin (after decompression) contains the raw suffix bytes
			// for items 1..N-1 concatenated. Each item is reconstructed as:
			//   commonPrefix + prevItem[0:prefixLen] + suffix[0:suffixLen]
			// where suffixLen = itemLen - prefixLen.
			//
			// Item 0 is the firstItem from the block header.
			itemsFile, err := os.ReadFile(partDir + "/items.bin")
			if err != nil {
				fatalf("read items.bin: %v", err)
			}
			lensFile, err := os.ReadFile(partDir + "/lens.bin")
			if err != nil {
				fatalf("read lens.bin: %v", err)
			}

			itemsRaw := itemsFile[itemsBlockOffset : itemsBlockOffset+uint64(itemsBlockSize)]
			lensRaw := lensFile[lensBlockOffset : lensBlockOffset+uint64(lensBlockSize)]

			// Decompress if marshalType=1 (ZSTD), otherwise use raw bytes as-is.
			itemsData := itemsRaw
			lensData := lensRaw
			if marshalType == 1 {
				itemsData, err = decompressZSTD(itemsRaw)
				if err != nil {
					fatalf("decompress items block %d: %v", bhIdx, err)
				}
				lensData, err = decompressZSTD(lensRaw)
				if err != nil {
					fatalf("decompress lens block %d: %v", bhIdx, err)
				}
			}

			items, err := decodeItems(commonPrefix, bhFirstItem, itemsCount, marshalType, itemsData, lensData)
			if err != nil {
				fatalf("decode items for block %d: %v", bhIdx, err)
			}

			fmt.Printf("      --- decoded %d items ---\n", len(items))
			for i, item := range items {
				fmt.Printf("      item[%02d] (%3d bytes): %x\n", i, len(item), item)
				fmt.Printf("               %s\n", interpretItem(item))
			}
		}
		if len(bhSrc) > 0 {
			fatalf("unexpected %d trailing bytes in index block", len(bhSrc))
		}
	}
}

// interpretItem decodes a raw indexdb item into a human-readable string.
//
// Each item starts with a 1-byte namespace prefix that determines its format:
//
//	0x00 — MetricName → TSID
//	0x01 — Tag → MetricID
//	0x02 — MetricID → TSID
//	0x03 — MetricID → MetricName
//	0x04 — Deleted MetricID
//	0x05 — Date → MetricID
//	0x06 — (Date,Tag) → MetricID
//	0x07 — (Date,MetricName) → TSID
//
// Tags are encoded as: tagKey + 0x01 (separator) + tagValue + 0x01 (separator)
// An empty tagKey means __name__ (the metric name itself).
// A tagKey starting with 0xFE is a composite key: 0xFE + VarUint64(nameLen) + metricName + tagKey.
// MetricIDs and dates are 8-byte big-endian uint64.
// TSID is 24 bytes: MetricGroupID(8) + JobID(4) + InstanceID(4) + MetricID(8).
func interpretItem(item []byte) string {
	if len(item) == 0 {
		return "<empty>"
	}
	nsPrefix := item[0]
	rest := item[1:]

	switch nsPrefix {
	case 0x00: // MetricName → TSID
		return "MetricName→TSID: " + interpretMetricNameToTSID(rest)
	case 0x01: // Tag → MetricID
		return "Tag→MetricID: " + interpretTagToMetricID(rest)
	case 0x02: // MetricID → TSID
		return "MetricID→TSID: " + interpretMetricIDToTSID(rest)
	case 0x03: // MetricID → MetricName
		return "MetricID→MetricName: " + interpretMetricIDToMetricName(rest)
	case 0x04: // Deleted MetricID
		return "DeletedMetricID: " + interpretDeletedMetricID(rest)
	case 0x05: // Date → MetricID
		return "Date→MetricID: " + interpretDateToMetricID(rest)
	case 0x06: // (Date,Tag) → MetricID
		return "DateTag→MetricID: " + interpretDateTagToMetricID(rest)
	case 0x07: // (Date,MetricName) → TSID
		return "DateMetricName→TSID: " + interpretDateMetricNameToTSID(rest)
	default:
		return fmt.Sprintf("<unknown nsPrefix=0x%02x>", nsPrefix)
	}
}

func interpretMetricNameToTSID(src []byte) string {
	// Format: MetricName (tags) + 0x02 (kvSeparator) + TSID (24 bytes)
	sep := bytes.IndexByte(src, 0x02)
	if sep < 0 {
		return fmt.Sprintf("<no kvSeparator found: %x>", src)
	}
	metricName := unmarshalAllTags(src[:sep])
	tsid := formatTSID(src[sep+1:])
	return fmt.Sprintf("%s → %s", metricName, tsid)
}

func interpretMetricIDToTSID(src []byte) string {
	if len(src) < 8 {
		return fmt.Sprintf("<need 8 bytes for metricID, got %d>", len(src))
	}
	metricID := binary.BigEndian.Uint64(src[:8])
	tsid := formatTSID(src[8:])
	return fmt.Sprintf("metricID=%d → %s", metricID, tsid)
}

func interpretMetricIDToMetricName(src []byte) string {
	if len(src) < 8 {
		return fmt.Sprintf("<need 8 bytes for metricID, got %d>", len(src))
	}
	metricID := binary.BigEndian.Uint64(src[:8])
	metricName := unmarshalAllTags(src[8:])
	return fmt.Sprintf("metricID=%d → %s", metricID, metricName)
}

func interpretDeletedMetricID(src []byte) string {
	if len(src) < 8 {
		return fmt.Sprintf("<need 8 bytes, got %d>", len(src))
	}
	metricID := binary.BigEndian.Uint64(src[:8])
	return fmt.Sprintf("metricID=%d", metricID)
}

func interpretDateToMetricID(src []byte) string {
	if len(src) < 16 {
		return fmt.Sprintf("<need 16 bytes, got %d>", len(src))
	}
	date := binary.BigEndian.Uint64(src[:8])
	metricID := binary.BigEndian.Uint64(src[8:16])
	return fmt.Sprintf("date=%s → metricID=%d", formatDate(date), metricID)
}

func interpretDateTagToMetricID(src []byte) string {
	if len(src) < 8 {
		return fmt.Sprintf("<need 8 bytes for date, got %d>", len(src))
	}
	date := binary.BigEndian.Uint64(src[:8])
	rest := src[8:]
	tagStr := interpretTagToMetricID(rest)
	return fmt.Sprintf("date=%s, %s", formatDate(date), tagStr)
}

func interpretDateMetricNameToTSID(src []byte) string {
	if len(src) < 8 {
		return fmt.Sprintf("<need 8 bytes for date, got %d>", len(src))
	}
	date := binary.BigEndian.Uint64(src[:8])
	rest := src[8:]
	mnToTSID := interpretMetricNameToTSID(rest)
	return fmt.Sprintf("date=%s, %s", formatDate(date), mnToTSID)
}

// unmarshalTag reads one tag (key + value) from src.
// Tag format: key bytes + 0x01 + value bytes + 0x01
// Returns the key, value, remaining bytes, and whether a composite key was found.
func unmarshalTag(src []byte) (key, value string, tail []byte, ok bool) {
	// Find key (terminated by tagSeparatorChar=0x01)
	sep := bytes.IndexByte(src, 0x01)
	if sep < 0 {
		return "", "", src, false
	}
	rawKey := src[:sep]
	src = src[sep+1:]

	// Find value (terminated by tagSeparatorChar=0x01)
	sep = bytes.IndexByte(src, 0x01)
	if sep < 0 {
		return "", "", src, false
	}
	rawValue := src[:sep]
	src = src[sep+1:]

	k := unescapeTagValue(rawKey)
	v := unescapeTagValue(rawValue)

	// Check for composite key prefix (0xFE)
	if len(k) > 0 && k[0] == 0xFE {
		name, tagKey := unmarshalCompositeKey(k[1:])
		return fmt.Sprintf("composite(%s,%s)", name, tagKey), string(v), src, true
	}

	if len(k) == 0 {
		return "__name__", string(v), src, true
	}
	return string(k), string(v), src, true
}

func unmarshalCompositeKey(src []byte) (name, key string) {
	nameLen, n := unmarshalVarUint64(src)
	if n <= 0 || uint64(n)+nameLen > uint64(len(src)) {
		return "<bad-composite>", ""
	}
	name = string(src[n : uint64(n)+nameLen])
	key = string(src[uint64(n)+nameLen:])
	return name, key
}

func unescapeTagValue(b []byte) []byte {
	// escapeChar=0x00, '0'→0x00, '1'→0x01, '2'→0x02
	if bytes.IndexByte(b, 0x00) < 0 {
		return b
	}
	var dst []byte
	for i := 0; i < len(b); i++ {
		if b[i] == 0x00 && i+1 < len(b) {
			switch b[i+1] {
			case '0':
				dst = append(dst, 0x00)
			case '1':
				dst = append(dst, 0x01)
			case '2':
				dst = append(dst, 0x02)
			default:
				dst = append(dst, b[i])
				continue
			}
			i++ // skip the escape code
		} else {
			dst = append(dst, b[i])
		}
	}
	return dst
}

// unmarshalAllTags reads a MetricName from src.
//
// MetricName format:
//
//	marshalTagValue(MetricGroup) — the __name__, terminated by 0x01
//	then for each tag:
//	  marshalTagValue(key) + marshalTagValue(value) — each terminated by 0x01
func unmarshalAllTags(src []byte) string {
	// First field is the metric group (__name__)
	sep := bytes.IndexByte(src, 0x01)
	if sep < 0 {
		return fmt.Sprintf("<cannot find metric group: %x>", src)
	}
	metricGroup := string(unescapeTagValue(src[:sep]))
	src = src[sep+1:]

	var tags []string
	for len(src) > 0 {
		key, value, tail, ok := unmarshalTag(src)
		if !ok {
			tags = append(tags, fmt.Sprintf("<remaining %d bytes: %x>", len(src), src))
			break
		}
		tags = append(tags, fmt.Sprintf("%s=%q", key, value))
		src = tail
	}

	if len(tags) == 0 {
		return metricGroup
	}
	return metricGroup + "{" + strings.Join(tags, ", ") + "}"
}

func formatTSID(src []byte) string {
	if len(src) < 24 {
		return fmt.Sprintf("<bad TSID: %d bytes>", len(src))
	}
	metricGroupID := binary.BigEndian.Uint64(src[0:8])
	jobID := binary.BigEndian.Uint32(src[8:12])
	instanceID := binary.BigEndian.Uint32(src[12:16])
	metricID := binary.BigEndian.Uint64(src[16:24])
	return fmt.Sprintf("TSID{metricGroupID=%d, jobID=%d, instanceID=%d, metricID=%d}", metricGroupID, jobID, instanceID, metricID)
}

func formatDate(dateVal uint64) string {
	// Date is stored as days since Unix epoch.
	if dateVal == 0 {
		return "global"
	}
	t := int64(dateVal) * 24 * 60 * 60
	return fmt.Sprintf("%s (day=%d)", time.Unix(t, 0).UTC().Format("2006-01-02"), dateVal)
}

func interpretTagToMetricID(src []byte) string {
	// Format: tagKey\x01 tagValue\x01 metricID(8 bytes)...
	//
	// There is exactly ONE tag (key + value), then the rest is raw metricIDs.
	// After compaction, multiple metricIDs can be packed in a single item.
	//
	// We must read exactly one tag, not greedily scan for 0x01 separators,
	// because raw metricID bytes can contain 0x01.
	key, value, tail, ok := unmarshalTag(src)
	if !ok {
		return fmt.Sprintf("<cannot parse tag: %x>", src)
	}
	tag := fmt.Sprintf("%s=%q", key, value)
	src = tail

	numIDs := len(src) / 8
	if numIDs == 1 {
		metricID := binary.BigEndian.Uint64(src)
		return fmt.Sprintf("%s → metricID=%d", tag, metricID)
	}
	if numIDs > 1 {
		ids := make([]string, numIDs)
		for i := 0; i < numIDs; i++ {
			ids[i] = fmt.Sprintf("%d", binary.BigEndian.Uint64(src[i*8:]))
		}
		return fmt.Sprintf("%s → %d metricIDs=[%s]", tag, numIDs, strings.Join(ids, ", "))
	}
	return fmt.Sprintf("%s → <incomplete, remaining: %x>", tag, src)
}

// decodeItems decodes all items from already-decompressed items and lens data.
//
// For marshalType=0 (plain):
//   - lensData contains fixed 8-byte big-endian uint64 lengths for items 1..N-1
//   - itemsData contains raw item suffixes (after commonPrefix) concatenated
//
// For marshalType=1 (ZSTD):
//   - lensData contains two VarUint64 arrays (each itemsCount-1 elements):
//     1. prefixLens: XOR-delta encoded — bytes of previous item to reuse
//     2. itemLens: XOR-delta encoded — total item length (excluding commonPrefix)
//   - itemsData contains concatenated suffix bytes for items 1..N-1
//
// In both cases, item 0 is the firstItem from the block header.
func decodeItems(commonPrefix, firstItem []byte, itemsCount uint32, marshalType byte, itemsData, lensData []byte) ([][]byte, error) {
	count := int(itemsCount)
	items := make([][]byte, count)
	items[0] = append([]byte{}, firstItem...)

	cpLen := len(commonPrefix)

	// Decode prefix lengths and item lengths depending on marshal type.
	prefixLens := make([]uint64, count)
	itemLens := make([]uint64, count)
	itemLens[0] = uint64(len(firstItem) - cpLen)

	if marshalType == 0 {
		// Plain: no prefix sharing, lens are fixed 8-byte big-endian.
		for i := 1; i < count; i++ {
			if len(lensData) < 8 {
				return nil, fmt.Errorf("item %d: not enough lens data", i)
			}
			itemLens[i] = binary.BigEndian.Uint64(lensData)
			lensData = lensData[8:]
			prefixLens[i] = 0
		}
	} else {
		// ZSTD: two VarUint64 arrays, XOR-delta encoded.
		prefixXOR := make([]uint64, count-1)
		tail, err := unmarshalVarUint64s(prefixXOR, lensData)
		if err != nil {
			return nil, fmt.Errorf("unmarshal prefixLens: %w", err)
		}
		itemXOR := make([]uint64, count-1)
		tail, err = unmarshalVarUint64s(itemXOR, tail)
		if err != nil {
			return nil, fmt.Errorf("unmarshal itemLens: %w", err)
		}
		if len(tail) > 0 {
			return nil, fmt.Errorf("unexpected %d trailing bytes in lens data", len(tail))
		}

		// Undo XOR-delta.
		for i, x := range prefixXOR {
			prefixLens[i+1] = x ^ prefixLens[i]
		}
		for i, x := range itemXOR {
			itemLens[i+1] = x ^ itemLens[i]
		}
	}

	// Reconstruct items 1..N-1.
	prevItem := firstItem[cpLen:]
	b := itemsData
	for i := 1; i < count; i++ {
		prefixLen := prefixLens[i]
		itemLen := itemLens[i]
		suffixLen := itemLen - prefixLen

		if uint64(len(b)) < suffixLen {
			return nil, fmt.Errorf("item %d: need %d suffix bytes, have %d", i, suffixLen, len(b))
		}
		//log.Println(string(prevItem[:prefixLen]))
		item := make([]byte, 0, cpLen+int(itemLen))
		item = append(item, commonPrefix...)
		item = append(item, prevItem[:prefixLen]...)
		item = append(item, b[:suffixLen]...)
		items[i] = item

		b = b[suffixLen:]
		prevItem = item[cpLen:]
	}
	if len(b) > 0 {
		return nil, fmt.Errorf("unexpected %d trailing bytes in items data", len(b))
	}
	return items, nil
}

// unmarshalVarUint64 returns unmarshaled uint64 from src and its size in bytes.
//
// Copied from github.com/VictoriaMetrics/VictoriaMetrics/lib/encoding.
func unmarshalVarUint64(src []byte) (uint64, int) {
	if len(src) == 0 {
		return 0, 0
	}
	if src[0] < 0x80 {
		return uint64(src[0]), 1
	}
	if len(src) == 1 {
		return 0, 0
	}
	if src[1] < 0x80 {
		return uint64(src[0]&0x7f) | uint64(src[1])<<7, 2
	}
	return binary.Uvarint(src)
}

// unmarshalVarUint64s unmarshals len(dst) uint64 values from src and returns the remaining tail.
//
// Copied from github.com/VictoriaMetrics/VictoriaMetrics/lib/encoding.
func unmarshalVarUint64s(dst []uint64, src []byte) ([]byte, error) {
	if len(src) < len(dst) {
		return src, fmt.Errorf("too small len(src)=%d; it must be bigger or equal to len(dst)=%d", len(src), len(dst))
	}
	for i := range dst {
		c := src[i]
		if c >= 0x80 {
			return unmarshalVarUint64sSlow(dst, src)
		}
		dst[i] = uint64(c)
	}
	return src[len(dst):], nil
}

func unmarshalVarUint64sSlow(dst []uint64, src []byte) ([]byte, error) {
	idx := uint(0)
	for i := range dst {
		if idx >= uint(len(src)) {
			return nil, fmt.Errorf("cannot unmarshal varuint from empty data")
		}
		c := src[idx]
		idx++
		if c < 0x80 {
			dst[i] = uint64(c)
			continue
		}

		if idx >= uint(len(src)) {
			return nil, fmt.Errorf("unexpected end of encoded varuint at byte 1; src=%x", src[idx-1:])
		}
		d := src[idx]
		idx++
		if d < 0x80 {
			dst[i] = uint64(c&0x7f) | (uint64(d) << 7)
			continue
		}

		if idx >= uint(len(src)) {
			return nil, fmt.Errorf("unexpected end of encoded varuint at byte 2; src=%x", src[idx-2:])
		}
		e := src[idx]
		idx++
		if e < 0x80 {
			dst[i] = uint64(c&0x7f) | (uint64(d&0x7f) << 7) | (uint64(e) << (2 * 7))
			continue
		}

		u := uint64(c&0x7f) | (uint64(d&0x7f) << 7) | (uint64(e&0x7f) << (2 * 7))

		j := idx
		for {
			if idx >= uint(len(src)) {
				return nil, fmt.Errorf("unexpected end of encoded varint; src=%x", src[j-3:])
			}
			c := src[idx]
			idx++
			if c < 0x80 {
				break
			}
		}

		switch idx - j {
		case 1:
			u |= (uint64(src[j]) << (3 * 7))
		case 2:
			b := src[j : j+2 : j+2]
			u |= (uint64(b[0]&0x7f) << (3 * 7)) | (uint64(b[1]) << (4 * 7))
		case 3:
			b := src[j : j+3 : j+3]
			u |= (uint64(b[0]&0x7f) << (3 * 7)) | (uint64(b[1]&0x7f) << (4 * 7)) | (uint64(b[2]) << (5 * 7))
		case 4:
			b := src[j : j+4 : j+4]
			u |= (uint64(b[0]&0x7f) << (3 * 7)) | (uint64(b[1]&0x7f) << (4 * 7)) | (uint64(b[2]&0x7f) << (5 * 7)) | (uint64(b[3]) << (6 * 7))
		case 5:
			b := src[j : j+5 : j+5]
			u |= (uint64(b[0]&0x7f) << (3 * 7)) | (uint64(b[1]&0x7f) << (4 * 7)) | (uint64(b[2]&0x7f) << (5 * 7)) | (uint64(b[3]&0x7f) << (6 * 7)) |
				(uint64(b[4]) << (7 * 7))
		case 6:
			b := src[j : j+6 : j+6]
			u |= (uint64(b[0]&0x7f) << (3 * 7)) | (uint64(b[1]&0x7f) << (4 * 7)) | (uint64(b[2]&0x7f) << (5 * 7)) | (uint64(b[3]&0x7f) << (6 * 7)) |
				(uint64(b[4]&0x7f) << (7 * 7)) | (uint64(b[5]) << (8 * 7))
		case 7:
			b := src[j : j+7 : j+7]
			if b[6] > 1 {
				return src[idx:], fmt.Errorf("too big encoded varuint; src=%x", src[j-3:])
			}
			u |= (uint64(b[0]&0x7f) << (3 * 7)) | (uint64(b[1]&0x7f) << (4 * 7)) | (uint64(b[2]&0x7f) << (5 * 7)) | (uint64(b[3]&0x7f) << (6 * 7)) |
				(uint64(b[4]&0x7f) << (7 * 7)) | (uint64(b[5]&0x7f) << (8 * 7)) | (1 << (9 * 7))
		default:
			return src[idx:], fmt.Errorf("too long encoded varuint; the maximum allowed length is 10 bytes; got %d bytes; src=%x", idx-j+3, src[j-3:])
		}

		dst[i] = u
	}
	return src[idx:], nil
}

// unmarshalBytes returns unmarshaled bytes from src and the size of the unmarshaled bytes.
//
// Copied from github.com/VictoriaMetrics/VictoriaMetrics/lib/encoding.
func unmarshalBytes(src []byte) ([]byte, int) {
	n, nSize := unmarshalVarUint64(src)
	if nSize <= 0 {
		return nil, 0
	}
	if uint64(nSize)+n > uint64(len(src)) {
		return nil, 0
	}
	start := nSize
	nSize += int(n)
	return src[start:nSize], nSize
}

func decompressZSTD(compressed []byte) ([]byte, error) {
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	defer decoder.Close()
	return decoder.DecodeAll(compressed, nil)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
