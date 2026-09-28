# IndexDB Reader

A CLI tool for studying the on-disk format of VictoriaMetrics indexdb.

**Disclaimer:** This project is for learning purposes only. It is kept as simple as possible with no optimizations, does not depend on VictoriaMetrics libraries, and is designed to work only on small parts.

## Usage

```
go run indexdb-reader/main.go <part-directory>
```

Example:
```
go run indexdb-reader/main.go ./legacy-vm-data/indexdb/18D8D04BD1AD1DC0/18D8D04BD2E00A3A/
```

## IndexDB Directory Structure

```
indexdb/
├── 18D8D04BD1AD1DC0/          # table (prev/curr/next rotation)
│   ├── parts.json             # lists active parts
│   ├── 18D8D04BD2E00A3A/      # part (like an SSTable in LSM tree)
│   │   ├── metadata.json
│   │   ├── metaindex.bin
│   │   ├── index.bin
│   │   ├── items.bin
│   │   └── lens.bin
│   └── 18D8D04BD2E00AE5/      # another part
│       └── ...
└── 18D8D04BD1AD1DC1/          # another table
    └── ...
```

### Tables

VictoriaMetrics keeps 3 indexdb tables: `prev`, `curr`, `next`. They are used for
index rotation/expiration, NOT time-range partitioning. Table names are
`time.Now().UnixNano()` formatted as `%016X`.

### Parts

Parts are like SSTables in an LSM tree. They CAN and DO overlap in key ranges.
Part names also use the UnixNano hex pattern. Active parts are listed in
`parts.json` at the table level.

## On-Disk Format (4 layers)

Reading a part requires traversing 4 layers:

```
metaindex.bin ──► index.bin ──► items.bin + lens.bin ──► decoded items
```

### Layer 1: metadata.json

```json
{
  "itemsCount": 119573,
  "blocksCount": 597,
  "firstItem": "0001...",
  "lastItem": "0704..."
}
```

`firstItem` and `lastItem` are hex-encoded raw item bytes.

### Layer 2: metaindex.bin

ZSTD-compressed. After decompression, contains a sequence of metaindex rows:

```
VarUint64(len) + firstItem bytes    # length-prefixed first item
Uint32(blockHeadersCount)           # big-endian
Uint64(indexBlockOffset)            # big-endian, offset into index.bin
Uint32(indexBlockSize)              # big-endian, compressed size in index.bin
```

Each metaindex row points to a chunk of `index.bin`.

### Layer 3: index.bin

Each chunk (pointed to by a metaindex row) is ZSTD-compressed. After
decompression, contains `blockHeadersCount` block headers back-to-back:

```
VarUint64(len) + commonPrefix bytes
VarUint64(len) + firstItem bytes
Uint8(marshalType)                  # 0 = plain, 1 = ZSTD
Uint32(itemsCount)                  # items EXCLUDING first item
Uint64(itemsBlockOffset)            # offset into items.bin
Uint64(lensBlockOffset)             # offset into lens.bin
Uint32(itemsBlockSize)              # byte range in items.bin
Uint32(lensBlockSize)               # byte range in lens.bin
```

### Layer 4: items.bin + lens.bin

Each block header points to a slice of `items.bin` and `lens.bin`. The encoding
depends on `marshalType`.

**marshalType = 0 (Plain):**
- `lens.bin` slice: fixed 8-byte big-endian uint64 lengths for items 1..N
- `items.bin` slice: raw item suffixes (after commonPrefix) concatenated

**marshalType = 1 (ZSTD):**
- Both slices are ZSTD-compressed first.
- `lens.bin` (decompressed) contains two VarUint64 arrays back-to-back:
  1. `prefixLensXOR[0..itemsCount-1]` — XOR-delta encoded prefix lengths
  2. `itemLensXOR[0..itemsCount-1]` — XOR-delta encoded item lengths (excluding commonPrefix)
- `items.bin` (decompressed) contains concatenated suffix bytes for items 1..N

**Item reconstruction:**
- Item 0 = `firstItem` from the block header
- Each subsequent item: `commonPrefix + prevItem[:prefixLen] + suffix`

**Two levels of deduplication:**
- `commonPrefix` — longest common byte prefix between first and last items in
  block. Removed from ALL items.
- `prefixLen` — bytes shared with the PREVIOUS item (consecutive items are
  similar due to sorting). This is the per-item prefix compression.

**XOR-delta decoding:**
```
accumulator = 0
for each xorValue in array:
    accumulator = accumulator ^ xorValue
    result = accumulator
```

## Item Namespaces

Each decoded item starts with a namespace byte that determines its format:

| Byte | Type | Description |
|------|------|-------------|
| `0x00` | MetricName → TSID | Full tag set mapped to a TSID |
| `0x01` | Tag → MetricIDs | A single tag key+value mapped to one or more metricIDs |
| `0x02` | MetricID → TSID | 8-byte metricID mapped to 24-byte TSID |
| `0x03` | MetricID → MetricName | 8-byte metricID mapped to marshaled metric name |
| `0x04` | Deleted MetricID | Tombstone marker, just 8-byte metricID |
| `0x05` | Date → MetricID | 8-byte date + 8-byte metricID |
| `0x06` | DateTag → MetricIDs | Per-day version of Tag → MetricIDs |
| `0x07` | DateMetricName → TSID | Per-day version of MetricName → TSID |

### TSID (24 bytes)

```
MetricGroupID   uint64   (8 bytes, big-endian)
JobID           uint32   (4 bytes, big-endian)
InstanceID      uint32   (4 bytes, big-endian)
MetricID        uint64   (8 bytes, big-endian)
```

### Tag Encoding

Tags use an escape scheme for separator bytes:
- `0x00` → `0x00` + `'0'`
- `0x01` → `0x00` + `'1'`
- `0x02` → `0x00` + `'2'`
- `0x01` is used as a separator between key and value, and after the value

A single tag: `escapedKey + 0x01 + escapedValue + 0x01`

A metric name: `escapedMetricGroup + 0x01 + escapedKey1 + 0x01 + escapedValue1 + 0x01 + escapedKey2 + 0x01 + ...`

### Composite Tags

Composite tags combine `__name__` with a tag key for efficient single-lookup
queries like `metric{job="x"}`.

Format: `0xFE + VarUint64(nameLen) + metricName + tagKey`

For each tag, TWO index entries are created: a plain Tag → MetricID and a
composite Tag → MetricID.

### Merged Rows

During compaction, items with the same tag key+value are merged. The result is a
single item with multiple 8-byte metricIDs packed at the end. Items are split
across multiple entries when they exceed ~64KB.

### Per-Metric Index Footprint

For a metric with N tags, the index stores:
- N+1 Tag → MetricID entries (one per tag + `__name__`)
- N Composite Tag → MetricID entries
- 1 MetricID → TSID
- 1 MetricID → MetricName
- 1 Date → MetricID
- N+1 DateTag → MetricID entries
- N DateTag → MetricID composite entries
- 1 DateMetricName → TSID

Global (prefixes `0x00`-`0x04`) and per-day (prefixes `0x05`-`0x07`) indexes
store the same metricID/TSID values.
