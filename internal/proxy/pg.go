package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nazishasghar/routeperf/internal/db"
)

const (
	pgSSLRequest    = 80877103
	pgGSSEncRequest = 80877104
	pgCancelRequest = 80877102
)

type pgStmt struct {
	sql  string
	oids []uint32
}

type pgPortal struct {
	sql    string
	params []string
}

type pgPending struct {
	simple bool
	sql    string
	params []string
	start  time.Time
}

// pgConn tracks protocol state for one client connection.
type pgConn struct {
	p        *Proxy
	id       string
	mu       sync.Mutex
	stmts    map[string]*pgStmt
	portals  map[string]*pgPortal
	describe []string // statement names awaiting ParameterDescription
	pending  []pgPending
}

func readStartup(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint32(hdr[:]))
	if n < 8 || n > 10000 {
		return nil, fmt.Errorf("bad startup length %d", n)
	}
	buf := make([]byte, n)
	copy(buf, hdr[:])
	_, err := io.ReadFull(r, buf[4:])
	return buf, err
}

func (p *Proxy) dialPG() (net.Conn, error) {
	s, err := net.DialTimeout("tcp", p.target, 10*time.Second)
	if err != nil {
		return nil, err
	}
	if p.upTLS == nil {
		return s, nil
	}
	req := make([]byte, 8)
	binary.BigEndian.PutUint32(req[0:], 8)
	binary.BigEndian.PutUint32(req[4:], pgSSLRequest)
	if _, err := s.Write(req); err != nil {
		s.Close()
		return nil, err
	}
	var resp [1]byte
	if _, err := io.ReadFull(s, resp[:]); err != nil || resp[0] != 'S' {
		s.Close()
		return nil, errors.New("server refused TLS")
	}
	tc := tls.Client(s, p.upTLS)
	if err := tc.Handshake(); err != nil {
		s.Close()
		return nil, err
	}
	return tc, nil
}

func (p *Proxy) servePG(c net.Conn, id string) error {
	defer c.Close()
	cr := bufio.NewReader(c)
	var startup []byte
	for {
		m, err := readStartup(cr)
		if err != nil {
			return err
		}
		code := binary.BigEndian.Uint32(m[4:8])
		switch code {
		case pgSSLRequest, pgGSSEncRequest:
			if _, err := c.Write([]byte{'N'}); err != nil { // no TLS between app and proxy
				return err
			}
			continue
		case pgCancelRequest:
			s, err := p.dialPG()
			if err != nil {
				return err
			}
			_, _ = s.Write(m)
			s.Close()
			return nil
		}
		startup = m
		break
	}
	s, err := p.dialPG()
	if err != nil {
		return err
	}
	defer s.Close()
	if _, err := s.Write(startup); err != nil {
		return err
	}
	pc := &pgConn{p: p, id: id, stmts: map[string]*pgStmt{}, portals: map[string]*pgPortal{}}
	done := make(chan struct{}, 2)
	go func() { pc.clientLoop(cr, s); done <- struct{}{} }()
	go func() { pc.serverLoop(bufio.NewReader(s), c); done <- struct{}{} }()
	<-done
	return nil
}

func readMsg(r *bufio.Reader) (byte, []byte, error) {
	t, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	var l [4]byte
	if _, err := io.ReadFull(r, l[:]); err != nil {
		return 0, nil, err
	}
	n := int(binary.BigEndian.Uint32(l[:]))
	if n < 4 {
		return 0, nil, errors.New("bad message length")
	}
	body := make([]byte, n-4)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	return t, body, nil
}

func writeMsg(w io.Writer, t byte, body []byte) error {
	buf := make([]byte, 5+len(body))
	buf[0] = t
	binary.BigEndian.PutUint32(buf[1:], uint32(len(body)+4))
	copy(buf[5:], body)
	_, err := w.Write(buf)
	return err
}

func cstring(b []byte) (string, []byte) {
	for i, c := range b {
		if c == 0 {
			return string(b[:i]), b[i+1:]
		}
	}
	return string(b), nil
}

func (pc *pgConn) clientLoop(r *bufio.Reader, s net.Conn) {
	for {
		t, body, err := readMsg(r)
		if err != nil {
			return
		}
		pc.onClient(t, body)
		if err := writeMsg(s, t, body); err != nil {
			return
		}
		if t == 'X' {
			return
		}
	}
}

func (pc *pgConn) serverLoop(r *bufio.Reader, c net.Conn) {
	for {
		t, body, err := readMsg(r)
		if err != nil {
			return
		}
		if err := writeMsg(c, t, body); err != nil {
			return
		}
		pc.onServer(t, body)
	}
}

func (pc *pgConn) onClient(t byte, b []byte) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	switch t {
	case 'Q':
		sql, _ := cstring(b)
		pc.pending = append(pc.pending, pgPending{simple: true, sql: sql, start: time.Now()})
	case 'P':
		name, rest := cstring(b)
		sql, rest := cstring(rest)
		st := &pgStmt{sql: sql}
		if len(rest) >= 2 {
			n := int(binary.BigEndian.Uint16(rest))
			rest = rest[2:]
			for i := 0; i < n && len(rest) >= 4; i++ {
				st.oids = append(st.oids, binary.BigEndian.Uint32(rest))
				rest = rest[4:]
			}
		}
		pc.stmts[name] = st
	case 'D':
		if len(b) > 0 && b[0] == 'S' {
			name, _ := cstring(b[1:])
			pc.describe = append(pc.describe, name)
		}
	case 'B':
		portal, rest := cstring(b)
		name, rest := cstring(rest)
		st := pc.stmts[name]
		if st == nil {
			return
		}
		pc.portals[portal] = &pgPortal{sql: st.sql, params: decodeBind(rest, st.oids)}
	case 'E':
		portal, _ := cstring(b)
		if pt := pc.portals[portal]; pt != nil {
			pc.pending = append(pc.pending, pgPending{sql: pt.sql, params: pt.params, start: time.Now()})
		}
	case 'C':
		if len(b) > 0 && b[0] == 'S' {
			name, _ := cstring(b[1:])
			delete(pc.stmts, name)
		}
	}
}

func (pc *pgConn) onServer(t byte, b []byte) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	switch t {
	case 't': // ParameterDescription for the oldest Describe(S)
		if len(pc.describe) == 0 {
			return
		}
		name := pc.describe[0]
		pc.describe = pc.describe[1:]
		if st := pc.stmts[name]; st != nil && len(b) >= 2 {
			n := int(binary.BigEndian.Uint16(b))
			oids := make([]uint32, 0, n)
			for i := 0; i < n && 2+4*i+4 <= len(b); i++ {
				oids = append(oids, binary.BigEndian.Uint32(b[2+4*i:]))
			}
			st.oids = oids
		}
	case 'C', 'E', 's', 'I': // one Execute finished
		if len(pc.pending) > 0 && !pc.pending[0].simple {
			pc.finish(pc.pending[0], t == 'E')
			pc.pending = pc.pending[1:]
		}
	case 'Z':
		if len(pc.pending) > 0 && pc.pending[0].simple {
			pc.finish(pc.pending[0], false)
			pc.pending = pc.pending[1:]
		}
		// after an error the server skips the batch's remaining Executes
		for len(pc.pending) > 0 && !pc.pending[0].simple {
			pc.pending = pc.pending[1:]
		}
		pc.describe = nil
	}
}

func (pc *pgConn) finish(x pgPending, failed bool) {
	if failed {
		return
	}
	pc.p.add(record{sql: strings.TrimSpace(x.sql), params: x.params, ms: float64(time.Since(x.start).Microseconds()) / 1000, conn: pc.id})
}

// decodeBind reads Bind parameters (after portal and statement names),
// rendering binary values by their type OIDs.
func decodeBind(b []byte, oids []uint32) []string {
	if len(b) < 2 {
		return nil
	}
	nf := int(binary.BigEndian.Uint16(b))
	b = b[2:]
	fmts := make([]int16, nf)
	for i := 0; i < nf && len(b) >= 2; i++ {
		fmts[i] = int16(binary.BigEndian.Uint16(b))
		b = b[2:]
	}
	if len(b) < 2 {
		return nil
	}
	np := int(binary.BigEndian.Uint16(b))
	b = b[2:]
	out := make([]string, 0, np)
	for i := 0; i < np && len(b) >= 4; i++ {
		l := int32(binary.BigEndian.Uint32(b))
		b = b[4:]
		if l < 0 {
			out = append(out, db.NullParam())
			continue
		}
		if int(l) > len(b) {
			break
		}
		v := b[:l]
		b = b[l:]
		f := int16(0)
		switch {
		case nf == 1:
			f = fmts[0]
		case i < nf:
			f = fmts[i]
		}
		var oid uint32
		if i < len(oids) {
			oid = oids[i]
		}
		if f == 0 {
			out = append(out, string(v))
		} else {
			out = append(out, pgBinary(oid, v))
		}
	}
	return out
}

var pgEpoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// pgBinary renders a binary-format value as Postgres text.
func pgBinary(oid uint32, v []byte) string {
	switch oid {
	case 16:
		if len(v) == 1 && v[0] != 0 {
			return "t"
		}
		return "f"
	case 21:
		if len(v) == 2 {
			return strconv.Itoa(int(int16(binary.BigEndian.Uint16(v))))
		}
	case 23, 26:
		if len(v) == 4 {
			return strconv.Itoa(int(int32(binary.BigEndian.Uint32(v))))
		}
	case 20:
		if len(v) == 8 {
			return strconv.FormatInt(int64(binary.BigEndian.Uint64(v)), 10)
		}
	case 700:
		if len(v) == 4 {
			return strconv.FormatFloat(float64(math.Float32frombits(binary.BigEndian.Uint32(v))), 'g', -1, 32)
		}
	case 701:
		if len(v) == 8 {
			return strconv.FormatFloat(math.Float64frombits(binary.BigEndian.Uint64(v)), 'g', -1, 64)
		}
	case 2950:
		if len(v) == 16 {
			return fmt.Sprintf("%x-%x-%x-%x-%x", v[0:4], v[4:6], v[6:8], v[8:10], v[10:16])
		}
	case 1082:
		if len(v) == 4 {
			return pgEpoch.AddDate(0, 0, int(int32(binary.BigEndian.Uint32(v)))).Format("2006-01-02")
		}
	case 1114, 1184:
		if len(v) == 8 {
			t := pgEpoch.Add(time.Duration(int64(binary.BigEndian.Uint64(v))) * time.Microsecond)
			s := t.Format("2006-01-02 15:04:05.999999")
			if oid == 1184 {
				s += "+00"
			}
			return s
		}
	case 3802:
		if len(v) > 0 && v[0] == 1 {
			return string(v[1:])
		}
	case 17:
		return `\x` + hex.EncodeToString(v)
	case 1700:
		return pgNumeric(v)
	case 1005, 1007, 1016, 1009, 1015, 2951, 1000: // int2[], int4[], int8[], text[], varchar[], uuid[], bool[]
		return pgArray(v)
	case 25, 1043, 1042, 19, 114, 0:
		return string(v)
	}
	return string(v)
}

func pgNumeric(v []byte) string {
	if len(v) < 8 {
		return "0"
	}
	nd := int(binary.BigEndian.Uint16(v))
	weight := int(int16(binary.BigEndian.Uint16(v[2:])))
	sign := binary.BigEndian.Uint16(v[4:])
	dscale := int(binary.BigEndian.Uint16(v[6:]))
	if sign == 0xC000 {
		return "NaN"
	}
	val := new(big.Int)
	for i := 0; i < nd && 8+2*i+2 <= len(v); i++ {
		val.Mul(val, big.NewInt(10000))
		val.Add(val, big.NewInt(int64(binary.BigEndian.Uint16(v[8+2*i:]))))
	}
	// value = val × 10000^(weight-nd+1)
	exp := 4 * (weight - nd + 1)
	s := val.String()
	if exp >= 0 {
		s += strings.Repeat("0", exp)
	} else {
		frac := -exp
		for len(s) <= frac {
			s = "0" + s
		}
		s = s[:len(s)-frac] + "." + s[len(s)-frac:]
	}
	if i := strings.IndexByte(s, '.'); i >= 0 {
		want := i + 1 + dscale
		if dscale == 0 {
			want = i
		}
		if want < len(s) {
			s = s[:want]
		}
	}
	if sign == 0x4000 && s != "0" {
		s = "-" + s
	}
	return s
}

// pgArray renders a one-dimensional binary array as an array literal.
func pgArray(v []byte) string {
	if len(v) < 12 {
		return "{}"
	}
	ndim := int(binary.BigEndian.Uint32(v))
	elem := binary.BigEndian.Uint32(v[8:])
	if ndim == 0 {
		return "{}"
	}
	if ndim != 1 || len(v) < 20 {
		return "{}"
	}
	n := int(binary.BigEndian.Uint32(v[12:]))
	b := v[20:]
	var parts []string
	for i := 0; i < n && len(b) >= 4; i++ {
		l := int32(binary.BigEndian.Uint32(b))
		b = b[4:]
		if l < 0 {
			parts = append(parts, "NULL")
			continue
		}
		if int(l) > len(b) {
			break
		}
		s := pgBinary(elem, b[:l])
		b = b[l:]
		if elem == 25 || elem == 1043 || elem == 2950 {
			s = `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
		}
		parts = append(parts, s)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func (p *Proxy) probeQuery(ctx context.Context, token string) error {
	if p.dialect == "mysql" {
		return p.probeMy(ctx, token)
	}
	cc, err := pgx.ParseConfig(p.ProxyURL())
	if err != nil {
		return err
	}
	cc.RuntimeParams["application_name"] = "rp-probe"
	c, err := pgx.ConnectConfig(ctx, cc)
	if err != nil {
		return err
	}
	defer c.Close(ctx)
	var s string
	return c.QueryRow(ctx, "SELECT $1::text", token).Scan(&s)
}
