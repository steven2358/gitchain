package git

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
)

type Delta struct {
	Hash  []byte
	Delta []byte
}

type Packfile struct {
	Version  uint32
	Objects  []Object
	Checksum []byte
	Deltas   []Delta
	offsets  map[int]int
	hashes   map[string]int
}

func (r *Packfile) ObjectByHash(hash []byte) Object {
	index, exists := r.hashes[string(hash)]
	if !exists {
		return nil
	}
	return r.Objects[index]
}

func (r *Packfile) PutObject(o Object) {
	r.Objects = append(r.Objects, o)
	r.hashes[string(o.Hash())] = len(r.Objects) - 1
}

func readMSBEncodedSize(reader io.Reader, initialOffset uint) uint64 {
	var b byte
	var sz uint64
	shift := initialOffset
	sz = 0
	for {
		binary.Read(reader, binary.BigEndian, &b)
		sz += (uint64(b) &^ 0x80) << shift
		shift += 7
		if (b & 0x80) == 0 {
			break
		}
	}
	return sz
}

func inflate(reader io.Reader, sz int) ([]byte, error) {
	zr, err := zlib.NewReader(reader)
	if err != nil {
		return nil, errors.New(fmt.Sprintf("error opening packfile's object zlib: %v", err))
	}
	buf := make([]byte, sz)

	n, err := zr.Read(buf)
	if err != nil {
		return nil, err
	}

	if n != sz {
		return nil, errors.New(fmt.Sprintf("inflated size mismatch, expected %d, got %d", sz, n))
	}

	zr.Close()
	return buf, nil
}

func readEntry(packfile *Packfile, reader flate.Reader) error {
	var b, typ uint8
	var sz uint64
	binary.Read(reader, binary.BigEndian, &b)
	typ = (b &^ 0x8f) >> 4
	sz = uint64(b &^ 0xf0)
	switch typ {
	case OBJ_REF_DELTA:
		if (b & 0x80) != 0 {
			sz += readMSBEncodedSize(reader, 4)
		}
		ref := make([]byte, 20)
		reader.Read(ref)

		buf, err := inflate(reader, int(sz))
		if err != nil {
			return err
		}

		referenced := packfile.ObjectByHash(ref)
		if referenced == nil {
			packfile.Deltas = append(packfile.Deltas, Delta{Hash: ref, Delta: buf})
		} else {
			patched := PatchDelta(referenced.Bytes(), buf)
			if patched == nil {
				return errors.New(fmt.Sprintf("error while patching %s", hex.EncodeToString(ref)))
			}
			newObject := referenced.New()
			newObject.SetBytes(patched)
			packfile.PutObject(newObject)
		}
	case OBJ_OFS_DELTA:
		if (b & 0x80) != 0 {
			sz += readMSBEncodedSize(reader, 4)
		}
		// TODO: read the negative offset
		_, err := inflate(reader, int(sz))
		if err != nil {
			return err
		}
		// packfile.Objects = append(packfile.Objects, buf)
	case OBJ_COMMIT, OBJ_TREE, OBJ_BLOB, OBJ_TAG:
		if (b & 0x80) != 0 {
			sz += readMSBEncodedSize(reader, 4)
		}
		buf, err := inflate(reader, int(sz))
		if err != nil {
			return err
		}
		var obj Object
		switch typ {
		case OBJ_COMMIT:
			obj = &Commit{Content: buf}
		case OBJ_TREE:
			obj = &Tree{Content: buf}
		case OBJ_BLOB:
			obj = &Blob{Content: buf}
		case OBJ_TAG:
			obj = &Tag{Content: buf}
		}
		packfile.PutObject(obj)
	default:
		return errors.New(fmt.Sprintf("Invalid git object tag %03b", typ))
	}
	return nil
}

func ReadPackfile(r io.Reader) (*Packfile, error) {
	// bufreader := bufio.NewReader(r)

	magic := make([]byte, 4)
	r.Read(magic)
	if bytes.Compare(magic, []byte("PACK")) != 0 {
		return nil, errors.New("not a packfile")
	}
	packfile := &Packfile{offsets: make(map[int]int), hashes: make(map[string]int)}

	var objects uint32
	binary.Read(r, binary.BigEndian, &packfile.Version)
	binary.Read(r, binary.BigEndian, &objects)

	content, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, err
	}
	offset := 12

	for i := 0; i < int(objects); i++ {
		peReader := &packEntryReader{reader: bytes.NewBuffer(content)}
		err := readEntry(packfile, peReader)
		if err != nil {
			return packfile, err
		}
		packfile.offsets[offset] = len(packfile.Objects) - 1

		offset += peReader.Counter + 4
		content = content[peReader.Counter+4:]

	}
	packfile.Checksum = make([]byte, 20)
	bytes.NewBuffer(content).Read(packfile.Checksum)

	var unresolvedDeltas []Delta
	for i := range packfile.Deltas {
		ref := packfile.ObjectByHash(packfile.Deltas[i].Hash)
		if ref == nil {
			unresolvedDeltas = append(unresolvedDeltas, packfile.Deltas[i])
		} else {
			patched := PatchDelta(ref.Bytes(), packfile.Deltas[i].Delta)
			newObject := ref.New()
			newObject.SetBytes(patched)
			packfile.Objects = append(packfile.Objects, newObject)
		}
	}
	packfile.Deltas = unresolvedDeltas
	return packfile, nil
}

// This byte-counting hack is here to work around the fact that both zlib
// and flate use bufio and are very eager to read more data than they need.
// The counter in this reader allows us to know the length of the header +
// packed data read and therefore readjust the offset
type packEntryReader struct {
	Counter int
	reader  io.Reader
}

func (r *packEntryReader) Read(p []byte) (int, error) {
	r.Counter += (len(p))
	return r.reader.Read(p)
}

func (r *packEntryReader) ReadByte() (byte, error) {
	b := make([]byte, 1)
	_, err := r.Read(b)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}
