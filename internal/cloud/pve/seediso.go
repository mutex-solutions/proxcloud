package pve

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"slices"
	"time"
	"unicode/utf16"
)

// BuildSeedISO writes a minimal ISO 9660 image, volume label "cidata", with a Joliet tree: the file
// names cloud-init looks for ("user-data") are not legal ISO 9660 names, and Linux reads Joliet by
// default. No dependency writes Joliet in pure Go without pulling in a whole disk-image library, and
// a flat directory of small files is about the simplest image there is.
func BuildSeedISO(files map[string]string) ([]byte, error) {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	slices.Sort(names)
	const sector = 2048

	// Layout: 16 empty, PVD, SVD, terminator, then for each tree a little-endian and a big-endian
	// path table and the root directory, then the file contents.
	const pvdLBA, svdLBA, termLBA = 16, 17, 18
	pathL1, pathM1, root1 := 19, 20, 21
	pathL2, pathM2, root2 := 22, 23, 24
	next := 25
	type entry struct {
		name string
		lba  int
		size int
	}
	entries := make([]entry, len(names))
	for i, n := range names {
		entries[i] = entry{n, next, len(files[n])}
		next += (len(files[n]) + sector - 1) / sector
		if len(files[n]) == 0 {
			next++
		}
	}
	total := next

	now := time.Now().UTC()
	rec := func(id []byte, lba, size int, dir bool) []byte {
		n := 33 + len(id)
		if n%2 == 1 {
			n++
		}
		b := make([]byte, n)
		b[0] = byte(n)
		bothEndian32(b[2:], uint32(lba))
		bothEndian32(b[10:], uint32(size))
		copy(b[18:], recordDate(now))
		if dir {
			b[25] = 2
		}
		bothEndian16(b[28:], 1) // volume sequence
		b[32] = byte(len(id))
		copy(b[33:], id)
		return b
	}
	dirSector := func(self int, id func(i int, e entry) []byte) ([]byte, error) {
		var d bytes.Buffer
		d.Write(rec([]byte{0}, self, sector, true))
		d.Write(rec([]byte{1}, self, sector, true))
		for i, e := range entries {
			d.Write(rec(id(i, e), e.lba, e.size, false))
		}
		if d.Len() > sector {
			return nil, fmt.Errorf("pve seed iso: too many files")
		}
		return pad(d.Bytes(), sector), nil
	}
	dir1, err := dirSector(root1, func(i int, _ entry) []byte { return fmt.Appendf(nil, "F%d.;1", i) })
	if err != nil {
		return nil, err
	}
	dir2, err := dirSector(root2, func(_ int, e entry) []byte { return ucs2(e.name) })
	if err != nil {
		return nil, err
	}

	img := make([]byte, 0, total*sector)
	img = append(img, make([]byte, 16*sector)...)
	img = append(img, volumeDescriptor(1, []byte("cidata"), nil, total, pathL1, pathM1, rec([]byte{0}, root1, sector, true), now)...)
	img = append(img, volumeDescriptor(2, ucs2("cidata"), []byte("%/E"), total, pathL2, pathM2, rec([]byte{0}, root2, sector, true), now)...)
	term := make([]byte, sector)
	term[0], term[6] = 255, 1
	copy(term[1:], "CD001")
	img = append(img, term...)
	img = append(img, pad(pathTable(root1, binary.LittleEndian), sector)...)
	img = append(img, pad(pathTable(root1, binary.BigEndian), sector)...)
	img = append(img, dir1...)
	img = append(img, pad(pathTable(root2, binary.LittleEndian), sector)...)
	img = append(img, pad(pathTable(root2, binary.BigEndian), sector)...)
	img = append(img, dir2...)
	for _, e := range entries {
		size := (e.size + sector - 1) / sector * sector
		if e.size == 0 {
			size = sector
		}
		img = append(img, pad([]byte(files[e.name]), size)...)
	}
	if len(img) != total*sector || pvdLBA*sector != 32768 || svdLBA != pvdLBA+1 || termLBA != svdLBA+1 {
		return nil, fmt.Errorf("pve seed iso: layout mismatch")
	}
	return img, nil
}

// volumeDescriptor is the PVD (type 1) or the Joliet SVD (type 2, escape set).
func volumeDescriptor(typ byte, volID, escape []byte, total, pathL, pathM int, rootRec []byte, now time.Time) []byte {
	b := make([]byte, 2048)
	b[0] = typ
	copy(b[1:], "CD001")
	b[6] = 1
	// Text fields are space padded, in UCS-2 on the Joliet descriptor (blkid reads its label there).
	fill := func(from, to int) {
		for i := from; i < to; i++ {
			b[i] = ' '
			if typ == 2 && (i-from)%2 == 0 {
				b[i] = 0
			}
		}
	}
	fill(8, 72)
	copy(b[40:72], volID)
	bothEndian32(b[80:], uint32(total))
	copy(b[88:], escape)
	bothEndian16(b[120:], 1)
	bothEndian16(b[124:], 1)
	bothEndian16(b[128:], 2048)
	bothEndian32(b[132:], 10) // path table: one record, 8 + 1 + pad
	binary.LittleEndian.PutUint32(b[140:], uint32(pathL))
	binary.BigEndian.PutUint32(b[148:], uint32(pathM))
	copy(b[156:], rootRec[:34])
	fill(190, 813) // set, publisher, preparer, application, file ids
	stamp := []byte(now.Format("20060102150405") + "00\x00")
	copy(b[813:], stamp)
	copy(b[830:], stamp)
	copy(b[847:], []byte("0000000000000000\x00"))
	copy(b[864:], []byte("0000000000000000\x00"))
	b[881] = 1
	return b
}

// pathTable is the one-directory path table of a tree.
func pathTable(rootLBA int, order binary.ByteOrder) []byte {
	b := make([]byte, 10)
	b[0] = 1 // id length
	order.PutUint32(b[2:], uint32(rootLBA))
	order.PutUint16(b[6:], 1)
	return b
}

func recordDate(t time.Time) []byte {
	return []byte{byte(t.Year() - 1900), byte(t.Month()), byte(t.Day()), byte(t.Hour()), byte(t.Minute()), byte(t.Second()), 0}
}

func bothEndian32(b []byte, v uint32) {
	binary.LittleEndian.PutUint32(b, v)
	binary.BigEndian.PutUint32(b[4:], v)
}

func bothEndian16(b []byte, v uint16) {
	binary.LittleEndian.PutUint16(b, v)
	binary.BigEndian.PutUint16(b[2:], v)
}

func pad(b []byte, n int) []byte {
	out := make([]byte, n)
	copy(out, b)
	return out
}

func ucs2(s string) []byte {
	var out []byte
	for _, u := range utf16.Encode([]rune(s)) {
		out = binary.BigEndian.AppendUint16(out, u)
	}
	return out
}
