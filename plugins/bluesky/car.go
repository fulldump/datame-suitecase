package bluesky

// A minimal reader of what an AT Protocol repository export is made of:
// CAR v1 files (a header and a list of content-addressed blocks), CIDs and
// DAG-CBOR (a strict subset of CBOR). It is about 300 lines, so it is written
// here rather than pulling a full IPLD stack; it only reads, and it checks the
// SHA-256 of every block against its CID.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
)

const (
	maxBlock = 16 << 20 // records are at most 1 MB; this only stops garbage
	maxDepth = 64
)

// cid is a binary CID (version 1 here; version 0 is read too).
type cid string

var base32Lower = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// String gives the usual text form: «b» + base32, as in «bafyrei…».
func (c cid) String() string {
	return "b" + base32Lower.EncodeToString(c.v1())
}

// v1 returns the CID in version 1: a v0 (a bare multihash, never used by
// atproto but valid in a CAR) is the same as v1 with the dag-pb codec.
func (c cid) v1() []byte {
	if len(c) == 34 && c[0] == 0x12 && c[1] == 0x20 {
		return append([]byte{1, 0x70}, c...)
	}
	return []byte(c)
}

// parseCID reads a text CID (only the base32 «b…» form atproto uses).
func parseCID(s string) (cid, error) {
	if !strings.HasPrefix(s, "b") {
		return "", fmt.Errorf("CID no admitido: %q", s)
	}
	data, err := base32Lower.DecodeString(s[1:])
	if err != nil {
		return "", fmt.Errorf("CID no válido: %q", s)
	}
	n, err := cidLength(data)
	if err != nil || n != len(data) {
		return "", fmt.Errorf("CID no válido: %q", s)
	}
	return cid(data), nil
}

// cidLength returns how many bytes of b are the CID at its start.
func cidLength(b []byte) (int, error) {
	if len(b) >= 34 && b[0] == 0x12 && b[1] == 0x20 {
		return 34, nil // v0: a bare sha2-256 multihash
	}
	pos := 0
	next := func() (uint64, error) {
		v, n := binary.Uvarint(b[pos:])
		if n <= 0 {
			return 0, errors.New("CID cortado")
		}
		pos += n
		return v, nil
	}
	version, err := next()
	if err != nil {
		return 0, err
	}
	if version != 1 {
		return 0, fmt.Errorf("versión de CID %d", version)
	}
	if _, err := next(); err != nil { // codec
		return 0, err
	}
	if _, err := next(); err != nil { // hash function
		return 0, err
	}
	size, err := next()
	if err != nil {
		return 0, err
	}
	if size > uint64(len(b)-pos) {
		return 0, errors.New("CID cortado")
	}
	return pos + int(size), nil
}

// verify checks that data is what the CID says, when it is a SHA-256 CID.
func (c cid) verify(data []byte) error {
	b := c.v1()
	pos := 0
	for i := 0; i < 2; i++ { // version, codec
		_, n := binary.Uvarint(b[pos:])
		pos += n
	}
	code, n := binary.Uvarint(b[pos:])
	pos += n
	size, n := binary.Uvarint(b[pos:])
	pos += n
	if code != 0x12 || size != 32 {
		return nil // another hash function: not checked
	}
	sum := sha256.Sum256(data)
	if !bytes.Equal(sum[:], b[pos:]) {
		return fmt.Errorf("el bloque %s no coincide con su huella", c)
	}
	return nil
}

// readCAR reads a CAR v1 into blocks and returns its roots.
func readCAR(r io.Reader, blocks map[cid][]byte) ([]cid, error) {
	br := bufio.NewReaderSize(r, 1<<16)
	headerLen, err := binary.ReadUvarint(br)
	if err != nil {
		return nil, fmt.Errorf("no es un CAR: %w", err)
	}
	if headerLen == 0 || headerLen > maxBlock {
		return nil, errors.New("no es un CAR: cabecera no válida")
	}
	header := make([]byte, headerLen)
	if _, err := io.ReadFull(br, header); err != nil {
		return nil, fmt.Errorf("no es un CAR: %w", err)
	}
	h, err := decodeCBOR(header)
	if err != nil {
		return nil, fmt.Errorf("cabecera del CAR: %w", err)
	}
	hm, _ := h.(map[string]any)
	if v, _ := hm["version"].(int64); v != 1 {
		return nil, fmt.Errorf("versión de CAR no admitida: %v", hm["version"])
	}
	roots := []cid{}
	list, _ := hm["roots"].([]any)
	for _, item := range list {
		if c, ok := item.(cid); ok {
			roots = append(roots, c)
		}
	}
	for {
		size, err := binary.ReadUvarint(br)
		if err == io.EOF {
			return roots, nil
		}
		if err != nil {
			return nil, fmt.Errorf("CAR cortado: %w", err)
		}
		if size == 0 || size > maxBlock {
			return nil, errors.New("CAR con un bloque no válido")
		}
		section := make([]byte, size)
		if _, err := io.ReadFull(br, section); err != nil {
			return nil, fmt.Errorf("CAR cortado: %w", err)
		}
		n, err := cidLength(section)
		if err != nil {
			return nil, err
		}
		c, data := cid(section[:n]), section[n:]
		if err := c.verify(data); err != nil {
			return nil, err
		}
		blocks[c] = data
	}
}

// ---- DAG-CBOR ----

type cborDecoder struct {
	data  []byte
	pos   int
	depth int
}

// decodeCBOR decodes one DAG-CBOR value into: map[string]any, []any, string,
// []byte, int64, float64, bool, nil and cid (tag 42).
func decodeCBOR(data []byte) (any, error) {
	d := &cborDecoder{data: data}
	v, err := d.value()
	if err != nil {
		return nil, err
	}
	if d.pos != len(data) {
		return nil, errors.New("CBOR con datos de más")
	}
	return v, nil
}

var errShort = errors.New("CBOR cortado")

func (d *cborDecoder) head() (major, info byte, arg uint64, err error) {
	if d.pos >= len(d.data) {
		return 0, 0, 0, errShort
	}
	b := d.data[d.pos]
	d.pos++
	major, info = b>>5, b&0x1f
	var n int
	switch {
	case info < 24:
		return major, info, uint64(info), nil
	case info == 24:
		n = 1
	case info == 25:
		n = 2
	case info == 26:
		n = 4
	case info == 27:
		n = 8
	default:
		return 0, 0, 0, fmt.Errorf("CBOR no admitido (longitud indefinida)")
	}
	if d.pos+n > len(d.data) {
		return 0, 0, 0, errShort
	}
	for _, c := range d.data[d.pos : d.pos+n] {
		arg = arg<<8 | uint64(c)
	}
	d.pos += n
	return major, info, arg, nil
}

func (d *cborDecoder) bytes(n uint64) ([]byte, error) {
	if n > uint64(len(d.data)-d.pos) {
		return nil, errShort
	}
	out := d.data[d.pos : d.pos+int(n)]
	d.pos += int(n)
	return out, nil
}

func (d *cborDecoder) value() (any, error) {
	d.depth++
	defer func() { d.depth-- }()
	if d.depth > maxDepth {
		return nil, errors.New("CBOR demasiado anidado")
	}
	major, info, arg, err := d.head()
	if err != nil {
		return nil, err
	}
	switch major {
	case 0:
		if arg > math.MaxInt64 {
			return nil, errors.New("entero demasiado grande")
		}
		return int64(arg), nil
	case 1:
		if arg > math.MaxInt64 {
			return nil, errors.New("entero demasiado grande")
		}
		return -1 - int64(arg), nil
	case 2:
		b, err := d.bytes(arg)
		if err != nil {
			return nil, err
		}
		return append([]byte(nil), b...), nil
	case 3:
		b, err := d.bytes(arg)
		if err != nil {
			return nil, err
		}
		return string(b), nil
	case 4:
		if arg > uint64(len(d.data)-d.pos) { // each item takes at least a byte
			return nil, errShort
		}
		out := make([]any, 0, arg)
		for i := uint64(0); i < arg; i++ {
			v, err := d.value()
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case 5:
		if arg > uint64(len(d.data)-d.pos)/2 {
			return nil, errShort
		}
		out := make(map[string]any, arg)
		for i := uint64(0); i < arg; i++ {
			k, err := d.value()
			if err != nil {
				return nil, err
			}
			key, ok := k.(string)
			if !ok {
				return nil, errors.New("CBOR con una clave que no es texto")
			}
			v, err := d.value()
			if err != nil {
				return nil, err
			}
			out[key] = v
		}
		return out, nil
	case 6:
		if arg != 42 {
			return nil, fmt.Errorf("etiqueta CBOR %d no admitida", arg)
		}
		v, err := d.value()
		if err != nil {
			return nil, err
		}
		b, ok := v.([]byte)
		if !ok || len(b) < 2 || b[0] != 0 {
			return nil, errors.New("enlace CID no válido")
		}
		if n, err := cidLength(b[1:]); err != nil || n != len(b)-1 {
			return nil, errors.New("enlace CID no válido")
		}
		return cid(b[1:]), nil
	default: // 7
		switch info {
		case 20:
			return false, nil
		case 21:
			return true, nil
		case 22, 23:
			return nil, nil
		case 25:
			return halfFloat(uint16(arg)), nil
		case 26:
			return float64(math.Float32frombits(uint32(arg))), nil
		case 27:
			return math.Float64frombits(arg), nil
		}
		return nil, fmt.Errorf("valor CBOR simple %d no admitido", info)
	}
}

func halfFloat(h uint16) float64 {
	sign := 1.0
	if h&0x8000 != 0 {
		sign = -1
	}
	exp := int(h>>10) & 0x1f
	frac := float64(h & 0x3ff)
	switch exp {
	case 0:
		return sign * math.Ldexp(frac, -24)
	case 31:
		if frac == 0 {
			return sign * math.Inf(1)
		}
		return math.NaN()
	}
	return sign * math.Ldexp(1+frac/1024, exp-15)
}

// toJSON gives the atproto JSON form of a decoded value: links become
// {"$link": "bafy…"} and bytes {"$bytes": "base64"}, as the XRPC API shows
// records, so the saved JSON reads like what the rest of the world uses.
func toJSON(v any) any {
	switch t := v.(type) {
	case cid:
		return map[string]any{"$link": t.String()}
	case []byte:
		return map[string]any{"$bytes": base64.RawStdEncoding.EncodeToString(t)}
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, item := range t {
			out[k] = toJSON(item)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = toJSON(item)
		}
		return out
	}
	return v
}

// ---- Repository ----

// errMissingBlock means the blocks at hand do not make a whole repository:
// the incremental CAR could not be joined with the previous ones.
var errMissingBlock = errors.New("falta un bloque del repositorio")

type record struct {
	Collection string
	RKey       string
	CID        cid
	Value      any // decoded DAG-CBOR
}

type commit struct {
	DID  string
	Rev  string
	Data cid // root of the Merkle Search Tree of records
}

func readCommit(blocks map[cid][]byte, root cid) (commit, error) {
	data, ok := blocks[root]
	if !ok {
		return commit{}, errMissingBlock
	}
	v, err := decodeCBOR(data)
	if err != nil {
		return commit{}, fmt.Errorf("commit: %w", err)
	}
	m, _ := v.(map[string]any)
	c := commit{}
	c.DID, _ = m["did"].(string)
	c.Rev, _ = m["rev"].(string)
	c.Data, _ = m["data"].(cid)
	if c.DID == "" || c.Data == "" {
		return commit{}, errors.New("el CAR no empieza por un commit de un repositorio")
	}
	return c, nil
}

// walkRecords visits every record of the tree in key order. MST nodes are
// {l: left subtree, e: [{p: prefix length, k: key suffix, v: record, t: right subtree}]},
// with keys «collection/rkey» compressed against the previous key of the node.
func walkRecords(blocks map[cid][]byte, root cid, fn func(record) error) error {
	var walk func(node cid, depth int) error
	walk = func(node cid, depth int) error {
		if depth > 64 {
			return errors.New("árbol del repositorio demasiado profundo")
		}
		data, ok := blocks[node]
		if !ok {
			return errMissingBlock
		}
		v, err := decodeCBOR(data)
		if err != nil {
			return fmt.Errorf("nodo del árbol: %w", err)
		}
		m, _ := v.(map[string]any)
		if left, ok := m["l"].(cid); ok {
			if err := walk(left, depth+1); err != nil {
				return err
			}
		}
		entries, _ := m["e"].([]any)
		prev := ""
		for _, item := range entries {
			e, _ := item.(map[string]any)
			p, _ := e["p"].(int64)
			k, _ := e["k"].([]byte)
			value, ok := e["v"].(cid)
			if !ok || p < 0 || int(p) > len(prev) {
				return errors.New("entrada del árbol no válida")
			}
			key := prev[:p] + string(k)
			prev = key
			collection, rkey, ok := strings.Cut(key, "/")
			if !ok {
				return fmt.Errorf("clave no válida: %q", key)
			}
			raw, ok := blocks[value]
			if !ok {
				return errMissingBlock
			}
			rec, err := decodeCBOR(raw)
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			if err := fn(record{Collection: collection, RKey: rkey, CID: value, Value: rec}); err != nil {
				return err
			}
			if right, ok := e["t"].(cid); ok {
				if err := walk(right, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(root, 0)
}
