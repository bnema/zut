package sessionindex

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// On-disk segment format (little-endian):
//
//	header (headerSize bytes):
//	  0   magic "ZUTSIDX\x00"
//	  8   format version u16, tokenizer version u16, reserved u32
//	  16  nDocs, nMsgs, nTerms, nRows u32
//	  32  section offsets u64 x numSections, then the end offset (file size)
//	  104 CRC32 (IEEE) of header bytes [0, 104) followed by [headerSize, end)
//	  The docs section starts right after the header (offset headerSize).
//	sections, in order:
//	  docs      per doc: str Path, str ID, str CWD, str Title,
//	            Started i64, Updated i64 (unix ns, minInt64 = zero time),
//	            Size varint, Cursor.Offset varint, Cursor.MsgSeq uvarint,
//	            str Cursor.MetaID, Cursor.HeadHash u64, TotalMsgs uvarint,
//	            flags u8 (bit 0 = Excluded)
//	            (str = uvarint length + bytes)
//	  first     u32 x (nDocs+1)
//	  rowFirst  u32 x (nDocs+1)
//	  termOffs  u32 x (nTerms+1), offsets into terms
//	  terms     sorted terms, concatenated
//	  postOffs  u64 x (nTerms+1), offsets into postings
//	  postings  per term: ascending global message IDs as uvarint deltas
//	  rows      rowSize bytes each, sorted by MsgID: MsgID u32, Offset u64,
//	            Len u32, Role u8, Time i64, Fingerprint u64
const (
	segmentMagic     = "ZUTSIDX\x00"
	formatVersion    = 3
	tokenizerVersion = 1
	headerSize       = 128
	rowSize          = 33
	crcOffset        = 104
)

const (
	secDocs = iota
	secFirst
	secRowFirst
	secTermOffs
	secTerms
	secPostOffs
	secPostings
	secRows
	numSections
)

// ErrCorrupt reports an unreadable, damaged, or incompatible index file.
// Callers should rebuild the index.
var ErrCorrupt = errors.New("sessionindex: corrupt or incompatible index")

// segmentCRC checksums the header fields before the CRC and the body.
func segmentCRC(header, body []byte) uint32 {
	return crc32.Update(crc32.ChecksumIEEE(header), crc32.IEEETable, body)
}

func corrupt(what string) error { return fmt.Errorf("%w: %s", ErrCorrupt, what) }

const (
	roleUser      = 1
	roleAssistant = 2
)

func encodeRole(r provider.Role) byte {
	if r == provider.RoleAssistant {
		return roleAssistant
	}
	return roleUser
}

func decodeRole(b byte) (provider.Role, bool) {
	switch b {
	case roleUser:
		return provider.RoleUser, true
	case roleAssistant:
		return provider.RoleAssistant, true
	}
	return "", false
}

func encodeTime(t time.Time) int64 {
	if t.IsZero() {
		return math.MinInt64
	}
	return t.UnixNano()
}

func decodeTime(n int64) time.Time {
	if n == math.MinInt64 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// encodeSegment serializes m.
func encodeSegment(m *memIndex) []byte {
	le := binary.LittleEndian
	buf := make([]byte, headerSize)
	var offs [numSections + 1]uint64
	mark := func(s int) { offs[s] = uint64(len(buf)) }
	str := func(s string) {
		buf = binary.AppendUvarint(buf, uint64(len(s)))
		buf = append(buf, s...)
	}

	mark(secDocs)
	for _, d := range m.docs {
		str(d.Path)
		str(d.ID)
		str(d.CWD)
		str(d.Title)
		buf = le.AppendUint64(buf, uint64(encodeTime(d.Started)))
		buf = le.AppendUint64(buf, uint64(encodeTime(d.Updated)))
		buf = binary.AppendVarint(buf, d.Size)
		buf = binary.AppendVarint(buf, d.Cursor.Offset)
		buf = binary.AppendUvarint(buf, uint64(d.Cursor.MsgSeq))
		str(d.Cursor.MetaID)
		buf = le.AppendUint64(buf, d.Cursor.HeadHash)
		buf = binary.AppendUvarint(buf, uint64(d.TotalMsgs))
		var flags byte
		if d.Excluded {
			flags |= 1
		}
		buf = append(buf, flags)
	}
	mark(secFirst)
	for _, v := range m.first {
		buf = le.AppendUint32(buf, v)
	}
	mark(secRowFirst)
	for _, v := range m.rowFirst {
		buf = le.AppendUint32(buf, v)
	}
	mark(secTermOffs)
	off := uint32(0)
	for _, t := range m.terms {
		buf = le.AppendUint32(buf, off)
		off += uint32(len(t))
	}
	buf = le.AppendUint32(buf, off)
	mark(secTerms)
	for _, t := range m.terms {
		buf = append(buf, t...)
	}
	// Postings are encoded first into a scratch buffer to know their offsets.
	var post []byte
	postOffs := make([]uint64, 0, len(m.terms)+1)
	for _, ids := range m.postings {
		postOffs = append(postOffs, uint64(len(post)))
		prev := uint32(0)
		for _, id := range ids {
			post = binary.AppendUvarint(post, uint64(id-prev))
			prev = id
		}
	}
	postOffs = append(postOffs, uint64(len(post)))
	mark(secPostOffs)
	for _, v := range postOffs {
		buf = le.AppendUint64(buf, v)
	}
	mark(secPostings)
	buf = append(buf, post...)
	mark(secRows)
	for _, r := range m.rows {
		buf = le.AppendUint32(buf, r.MsgID)
		buf = le.AppendUint64(buf, uint64(r.Offset))
		buf = le.AppendUint32(buf, uint32(r.Len))
		buf = append(buf, encodeRole(r.Role))
		buf = le.AppendUint64(buf, uint64(encodeTime(r.Time)))
		buf = le.AppendUint64(buf, r.Fingerprint)
	}
	offs[numSections] = uint64(len(buf))

	copy(buf, segmentMagic)
	le.PutUint16(buf[8:], formatVersion)
	le.PutUint16(buf[10:], tokenizerVersion)
	le.PutUint32(buf[16:], uint32(len(m.docs)))
	le.PutUint32(buf[20:], uint32(m.nMsgs()))
	le.PutUint32(buf[24:], uint32(len(m.terms)))
	le.PutUint32(buf[28:], uint32(len(m.rows)))
	for i, o := range offs {
		le.PutUint64(buf[32+8*i:], o)
	}
	le.PutUint32(buf[crcOffset:], segmentCRC(buf[:crcOffset], buf[headerSize:]))
	return buf
}

// writeSegment atomically replaces path with the serialized m. The parent
// directory is created 0700 and the file is written 0600.
func writeSegment(path string, m *memIndex) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("write session index: mkdir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".sessionindex-*.tmp")
	if err != nil {
		return fmt.Errorf("write session index: create: %w", err)
	}
	name := tmp.Name()
	fail := func(op string, err error) error {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("write session index: %s: %w", op, err)
	}
	// os.CreateTemp creates the file with mode 0600.
	if _, err := tmp.Write(encodeSegment(m)); err != nil {
		return fail("write", err)
	}
	if err := tmp.Sync(); err != nil {
		return fail("sync", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("write session index: close: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return fmt.Errorf("write session index: rename: %w", err)
	}
	return nil
}

// segment is an opened index file. Docs, IDs, and the term dictionary live
// in heap; postings and rows are read on demand with ReadAt.
type segment struct {
	f        *os.File
	docs     []docEntry
	first    []uint32
	rowFirst []uint32
	termOffs []uint32
	terms    []byte
	postOffs []uint64
	postBase int64
	rowsBase int64
	nRows    int
}

// openSegment opens and verifies path. Any damage or version mismatch
// returns an error wrapping ErrCorrupt. The whole body CRC is checked at
// open by streaming the file once; postings and rows are not retained.
func openSegment(path string) (*segment, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open session index: %w", err)
	}
	s, err := loadSegment(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return s, nil
}

func loadSegment(f *os.File) (*segment, error) {
	le := binary.LittleEndian
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("open session index: stat: %w", err)
	}
	size := info.Size()
	var h [headerSize]byte
	if size < headerSize {
		return nil, corrupt("short file")
	}
	if _, err := f.ReadAt(h[:], 0); err != nil {
		return nil, fmt.Errorf("open session index: read header: %w", err)
	}
	if string(h[:8]) != segmentMagic {
		return nil, corrupt("bad magic")
	}
	if le.Uint16(h[8:]) != formatVersion || le.Uint16(h[10:]) != tokenizerVersion {
		return nil, corrupt("version mismatch")
	}
	nDocs, nMsgs := int(le.Uint32(h[16:])), int(le.Uint32(h[20:]))
	nTerms, nRows := int(le.Uint32(h[24:])), int(le.Uint32(h[28:]))
	var offs [numSections + 1]int64
	prev := int64(headerSize)
	for i := range offs {
		o := int64(le.Uint64(h[32+8*i:]))
		if o < prev || o > size {
			return nil, corrupt("bad section offsets")
		}
		offs[i], prev = o, o
	}
	if offs[secDocs] != headerSize || offs[numSections] != size {
		return nil, corrupt("section bounds")
	}
	secLen := func(s int) int64 { return offs[s+1] - offs[s] }
	if secLen(secFirst) != 4*int64(nDocs+1) || secLen(secRowFirst) != 4*int64(nDocs+1) ||
		secLen(secTermOffs) != 4*int64(nTerms+1) || secLen(secPostOffs) != 8*int64(nTerms+1) ||
		secLen(secRows) != rowSize*int64(nRows) {
		return nil, corrupt("section sizes")
	}

	crc := crc32.NewIEEE()
	crc.Write(h[:crcOffset])
	if _, err := io.Copy(crc, bufio.NewReaderSize(io.NewSectionReader(f, headerSize, size-headerSize), 256*1024)); err != nil {
		return nil, fmt.Errorf("open session index: read body: %w", err)
	}
	if crc.Sum32() != le.Uint32(h[crcOffset:]) {
		return nil, corrupt("checksum mismatch")
	}

	// Heap sections are contiguous: docs .. postOffs.
	heap := make([]byte, offs[secPostings]-offs[secDocs])
	if _, err := f.ReadAt(heap, offs[secDocs]); err != nil {
		return nil, fmt.Errorf("open session index: read: %w", err)
	}
	sec := func(s int) []byte { return heap[offs[s]-headerSize : offs[s+1]-headerSize] }
	u32s := func(b []byte) []uint32 {
		out := make([]uint32, len(b)/4)
		for i := range out {
			out[i] = le.Uint32(b[4*i:])
		}
		return out
	}
	s := &segment{
		f:        f,
		first:    u32s(sec(secFirst)),
		rowFirst: u32s(sec(secRowFirst)),
		termOffs: u32s(sec(secTermOffs)),
		terms:    bytes.Clone(sec(secTerms)),
		postBase: offs[secPostings],
		rowsBase: offs[secRows],
		nRows:    nRows,
	}
	po := sec(secPostOffs)
	s.postOffs = make([]uint64, nTerms+1)
	for i := range s.postOffs {
		s.postOffs[i] = le.Uint64(po[8*i:])
	}
	if s.docs, err = decodeDocs(sec(secDocs), nDocs); err != nil {
		return nil, err
	}
	if err := s.validate(nMsgs, secLen(secPostings)); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *segment) validate(nMsgs int, postLen int64) error {
	if s.first[0] != 0 || int(s.first[len(s.first)-1]) != nMsgs || s.rowFirst[0] != 0 || int(s.rowFirst[len(s.rowFirst)-1]) != s.nRows {
		return corrupt("id tables")
	}
	for d := range s.docs {
		if s.first[d+1] < s.first[d] || s.rowFirst[d+1] < s.rowFirst[d] || int(s.first[d+1]-s.first[d]) != s.docs[d].TotalMsgs {
			return corrupt("id tables")
		}
	}
	if s.termOffs[0] != 0 || int(s.termOffs[len(s.termOffs)-1]) != len(s.terms) || s.postOffs[0] != 0 || int64(s.postOffs[len(s.postOffs)-1]) != postLen {
		return corrupt("term tables")
	}
	for i := 1; i < len(s.termOffs); i++ {
		if s.termOffs[i] < s.termOffs[i-1] || s.postOffs[i] < s.postOffs[i-1] {
			return corrupt("term tables")
		}
	}
	// Terms must be strictly ascending: lookups binary search them and
	// merges rely on the order.
	for i := 1; i+1 < len(s.termOffs); i++ {
		if bytes.Compare(s.term(i-1), s.term(i)) >= 0 {
			return corrupt("term order")
		}
	}
	return nil
}

func decodeDocs(b []byte, n int) ([]docEntry, error) {
	le := binary.LittleEndian
	bad := false
	uv := func() uint64 {
		v, k := binary.Uvarint(b)
		if k <= 0 {
			bad = true
			return 0
		}
		b = b[k:]
		return v
	}
	sv := func() int64 {
		v, k := binary.Varint(b)
		if k <= 0 {
			bad = true
			return 0
		}
		b = b[k:]
		return v
	}
	str := func() string {
		l := uv()
		if bad || l > uint64(len(b)) {
			bad = true
			return ""
		}
		s := string(b[:l])
		b = b[l:]
		return s
	}
	u64 := func() uint64 {
		if len(b) < 8 {
			bad = true
			return 0
		}
		v := le.Uint64(b)
		b = b[8:]
		return v
	}
	docs := make([]docEntry, n)
	for i := range docs {
		d := &docs[i]
		d.Path, d.ID, d.CWD, d.Title = str(), str(), str(), str()
		d.Started = decodeTime(int64(u64()))
		d.Updated = decodeTime(int64(u64()))
		d.Size = sv()
		d.Cursor = core.SessionScanCursor{Offset: sv(), MsgSeq: int(uv())}
		d.Cursor.MetaID = str()
		d.Cursor.HeadHash = u64()
		d.TotalMsgs = int(uv())
		if len(b) == 0 || b[0] > 1 {
			bad = true
		} else {
			d.Excluded = b[0] == 1
			b = b[1:]
		}
		if bad {
			return nil, corrupt("docs")
		}
	}
	if len(b) != 0 {
		return nil, corrupt("docs trailing bytes")
	}
	return docs, nil
}

// Close releases the file handle.
func (s *segment) Close() error { return s.f.Close() }

func (s *segment) term(i int) []byte { return s.terms[s.termOffs[i]:s.termOffs[i+1]] }

// tokenBits reads the postings of all terms starting with prefix in one
// ReadAt: matching terms are contiguous, so their postings are too.
func (s *segment) tokenBits(prefix string) (bitset, bool, error) {
	nTerms := len(s.termOffs) - 1
	p := []byte(prefix)
	lo := sort.Search(nTerms, func(i int) bool { return bytes.Compare(s.term(i), p) >= 0 })
	hi, partial := lo, false
	for hi < nTerms && bytes.HasPrefix(s.term(hi), p) {
		if hi-lo == maxPrefixTerms {
			partial = true
			break
		}
		hi++
	}
	nMsgs := s.first[len(s.first)-1]
	bs := newBitset(int(nMsgs))
	if lo == hi {
		return bs, partial, nil
	}
	buf := make([]byte, s.postOffs[hi]-s.postOffs[lo])
	if _, err := s.f.ReadAt(buf, s.postBase+int64(s.postOffs[lo])); err != nil {
		return nil, false, fmt.Errorf("read session index postings: %w", err)
	}
	base := s.postOffs[lo]
	for i := lo; i < hi; i++ {
		d := buf[s.postOffs[i]-base : s.postOffs[i+1]-base]
		id := uint64(0)
		for len(d) > 0 {
			v, k := binary.Uvarint(d)
			if k <= 0 {
				return nil, false, corrupt("postings")
			}
			d = d[k:]
			if v >= uint64(nMsgs) { // also rules out wraparound
				return nil, false, corrupt("postings")
			}
			id += v
			if id >= uint64(nMsgs) {
				return nil, false, corrupt("postings")
			}
			bs.set(uint32(id))
		}
	}
	return bs, partial, nil
}

func (s *segment) docRows(d int) ([]rowEntry, error) {
	le := binary.LittleEndian
	lo, hi := int64(s.rowFirst[d]), int64(s.rowFirst[d+1])
	buf := make([]byte, (hi-lo)*rowSize)
	if _, err := s.f.ReadAt(buf, s.rowsBase+lo*rowSize); err != nil {
		return nil, fmt.Errorf("read session index rows: %w", err)
	}
	rows := make([]rowEntry, hi-lo)
	for i := range rows {
		b := buf[i*rowSize:]
		role, ok := decodeRole(b[16])
		if !ok {
			return nil, corrupt("row role")
		}
		rows[i] = rowEntry{
			MsgID:       le.Uint32(b),
			Offset:      int64(le.Uint64(b[4:])),
			Len:         int64(le.Uint32(b[12:])),
			Role:        role,
			Time:        decodeTime(int64(le.Uint64(b[17:]))),
			Fingerprint: le.Uint64(b[25:]),
		}
		// Rows must lie in the doc's ID range, ascending, with a sane span.
		r := rows[i]
		if r.MsgID < s.first[d] || r.MsgID >= s.first[d+1] || (i > 0 && r.MsgID <= rows[i-1].MsgID) || r.Offset < 0 || r.Len <= 0 {
			return nil, corrupt("rows")
		}
	}
	return rows, nil
}
