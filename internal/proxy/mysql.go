package proxy

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	capCompress     = 0x00000020
	capProtocol41   = 0x00000200
	capSSL          = 0x00000800
	capDeprecateEOF = 0x01000000
	capQueryAttrs   = 0x08000000
	statusMoreRes   = 0x0008
)

type myStmt struct {
	sql     string
	nparams int
	types   []uint16
}

// myConn tracks one client connection.
type myConn struct {
	p       *Proxy
	id      string
	caps    uint32
	mu      sync.Mutex
	stmts   map[uint32]*myStmt
	cur     *myCmd // command awaiting its response
	command bool   // in the command phase (auth done)
}

type myCmd struct {
	op      byte
	sql     string
	start   time.Time
	prepare string // COM_STMT_PREPARE text
	// response parsing state
	state    int // 0 first packet, 1 column defs, 2 eof after defs, 3 rows, 4 prepare defs
	cols     int
	seen     int
	skipDefs int
}

func readPacket(r *bufio.Reader) ([]byte, byte, error) {
	var h [4]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, 0, err
	}
	n := int(h[0]) | int(h[1])<<8 | int(h[2])<<16
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, 0, err
	}
	return b, h[3], nil
}

func writePacket(w io.Writer, b []byte, seq byte) error {
	h := []byte{byte(len(b)), byte(len(b) >> 8), byte(len(b) >> 16), seq}
	if _, err := w.Write(append(h, b...)); err != nil {
		return err
	}
	return nil
}

func (p *Proxy) serveMy(c net.Conn, id string) error {
	defer c.Close()
	s, err := net.DialTimeout("tcp", p.target, 10*time.Second)
	if err != nil {
		return err
	}
	defer s.Close()
	sr, cr := bufio.NewReader(s), bufio.NewReader(c)
	// server greeting: hide TLS, compression and query attributes so the
	// client speaks plain, parseable packets
	greet, seq, err := readPacket(sr)
	if err != nil {
		return err
	}
	if len(greet) > 0 && greet[0] == 10 {
		i := 1
		for i < len(greet) && greet[i] != 0 {
			i++
		}
		i += 1 + 4 + 8 + 1 // NUL, thread id, auth data 1, filler
		if i+2 <= len(greet) {
			lo := binary.LittleEndian.Uint16(greet[i:])
			lo &^= uint16(capSSL | capCompress)
			binary.LittleEndian.PutUint16(greet[i:], lo)
			if j := i + 2 + 1 + 2; j+2 <= len(greet) {
				hi := binary.LittleEndian.Uint16(greet[j:])
				hi &^= uint16(capQueryAttrs >> 16)
				binary.LittleEndian.PutUint16(greet[j:], hi)
			}
		}
	}
	if err := writePacket(c, greet, seq); err != nil {
		return err
	}
	mc := &myConn{p: p, id: id, stmts: map[uint32]*myStmt{}}
	done := make(chan struct{}, 2)
	go func() { mc.clientLoop(cr, s); done <- struct{}{} }()
	go func() { mc.serverLoop(sr, c); done <- struct{}{} }()
	<-done
	return nil
}

func (mc *myConn) clientLoop(r *bufio.Reader, s net.Conn) {
	first := true
	for {
		b, seq, err := readPacket(r)
		if err != nil {
			return
		}
		if first && len(b) >= 4 { // handshake response: remember capabilities
			mc.mu.Lock()
			mc.caps = binary.LittleEndian.Uint32(b)
			mc.mu.Unlock()
			first = false
		} else if seq == 0 {
			mc.onCommand(b)
		}
		if err := writePacket(s, b, seq); err != nil {
			return
		}
	}
}

func (mc *myConn) serverLoop(r *bufio.Reader, c net.Conn) {
	for {
		b, seq, err := readPacket(r)
		if err != nil {
			return
		}
		if err := writePacket(c, b, seq); err != nil {
			return
		}
		mc.onResponse(b)
	}
}

func (mc *myConn) onCommand(b []byte) {
	if len(b) == 0 {
		return
	}
	mc.mu.Lock()
	defer mc.mu.Unlock()
	mc.command = true
	cmd := &myCmd{op: b[0], start: time.Now()}
	switch b[0] {
	case 0x03: // COM_QUERY
		cmd.sql = string(b[1:])
	case 0x16: // COM_STMT_PREPARE
		cmd.prepare = string(b[1:])
	case 0x17: // COM_STMT_EXECUTE
		if len(b) < 10 {
			return
		}
		st := mc.stmts[binary.LittleEndian.Uint32(b[1:])]
		if st == nil {
			return
		}
		cmd.sql = st.inline(b[10:])
	case 0x19, 0x18: // COM_STMT_CLOSE / SEND_LONG_DATA: no response
		if b[0] == 0x19 && len(b) >= 5 {
			delete(mc.stmts, binary.LittleEndian.Uint32(b[1:]))
		}
		return
	default:
		cmd.op = 0 // other commands: only consume the response
	}
	mc.cur = cmd
}

func (mc *myConn) deprecateEOF() bool { return mc.caps&capDeprecateEOF != 0 }

// onResponse follows the server's reply to the current command to find
// where it ends (OK/ERR, or the end of the last result set).
func (mc *myConn) onResponse(b []byte) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	cmd := mc.cur
	if cmd == nil || len(b) == 0 || !mc.command {
		return
	}
	isEOF := b[0] == 0xfe && len(b) < 0xffffff && (len(b) < 9 || mc.deprecateEOF())
	switch cmd.state {
	case 0:
		switch b[0] {
		case 0x00:
			if cmd.prepare != "" && len(b) >= 12 {
				id := binary.LittleEndian.Uint32(b[1:])
				ncol := int(binary.LittleEndian.Uint16(b[5:]))
				npar := int(binary.LittleEndian.Uint16(b[7:]))
				mc.stmts[id] = &myStmt{sql: cmd.prepare, nparams: npar}
				defs := ncol + npar
				if !mc.deprecateEOF() {
					if ncol > 0 {
						defs++
					}
					if npar > 0 {
						defs++
					}
				}
				if defs == 0 {
					mc.cur = nil
					return
				}
				cmd.skipDefs, cmd.state = defs, 4
				return
			}
			if okStatus(b)&statusMoreRes != 0 {
				return
			}
			mc.end(false)
		case 0xff:
			mc.end(true)
		case 0xfb: // LOCAL INFILE request
			mc.end(false)
		default: // result set: column count
			n, _ := lenenc(b)
			cmd.cols, cmd.seen, cmd.state = int(n), 0, 1
		}
	case 1: // column definitions
		cmd.seen++
		if cmd.seen >= cmd.cols {
			if mc.deprecateEOF() {
				cmd.state = 3
			} else {
				cmd.state = 2
			}
		}
	case 2: // EOF after column definitions
		cmd.state = 3
	case 3: // rows until EOF / OK
		if b[0] == 0xff {
			mc.end(true)
			return
		}
		if isEOF {
			var status uint16
			if len(b) >= 5 && !mc.deprecateEOF() {
				status = binary.LittleEndian.Uint16(b[3:])
			} else {
				status = okStatus(b)
			}
			if status&statusMoreRes != 0 {
				cmd.state = 0
				return
			}
			mc.end(false)
		}
	case 4: // prepare: parameter and column definitions
		cmd.skipDefs--
		if cmd.skipDefs <= 0 {
			mc.cur = nil
		}
	}
}

func (mc *myConn) end(failed bool) {
	cmd := mc.cur
	mc.cur = nil
	if cmd == nil || cmd.sql == "" || failed || cmd.op == 0 {
		return
	}
	mc.p.add(record{sql: strings.TrimSpace(cmd.sql), ms: float64(time.Since(cmd.start).Microseconds()) / 1000, conn: mc.id})
}

// okStatus reads the status flags of an OK (0x00 or 0xfe) packet.
func okStatus(b []byte) uint16 {
	if len(b) < 2 {
		return 0
	}
	rest := b[1:]
	for i := 0; i < 2; i++ { // affected rows, last insert id
		_, n := lenenc(rest)
		if n == 0 || n > len(rest) {
			return 0
		}
		rest = rest[n:]
	}
	if len(rest) < 2 {
		return 0
	}
	return binary.LittleEndian.Uint16(rest)
}

// lenenc decodes a length-encoded integer, returning it and its size.
func lenenc(b []byte) (uint64, int) {
	if len(b) == 0 {
		return 0, 0
	}
	switch b[0] {
	case 0xfc:
		if len(b) >= 3 {
			return uint64(binary.LittleEndian.Uint16(b[1:])), 3
		}
	case 0xfd:
		if len(b) >= 4 {
			return uint64(b[1]) | uint64(b[2])<<8 | uint64(b[3])<<16, 4
		}
	case 0xfe:
		if len(b) >= 9 {
			return binary.LittleEndian.Uint64(b[1:]), 9
		}
	default:
		return uint64(b[0]), 1
	}
	return 0, 0
}

// inline renders COM_STMT_EXECUTE parameters into the statement text, the
// way the general log shows executed prepared statements.
func (st *myStmt) inline(b []byte) string {
	n := st.nparams
	if n == 0 {
		return st.sql
	}
	nb := (n + 7) / 8
	if len(b) < nb+1 {
		return st.sql
	}
	nulls := b[:nb]
	b = b[nb:]
	bound := b[0]
	b = b[1:]
	if bound == 1 {
		if len(b) < 2*n {
			return st.sql
		}
		st.types = make([]uint16, n)
		for i := 0; i < n; i++ {
			st.types[i] = binary.LittleEndian.Uint16(b[2*i:])
		}
		b = b[2*n:]
	}
	if len(st.types) != n {
		return st.sql
	}
	vals := make([]string, n)
	for i := 0; i < n; i++ {
		if nulls[i/8]&(1<<(i%8)) != 0 {
			vals[i] = "NULL"
			continue
		}
		v, used := myValue(st.types[i], b)
		if used < 0 {
			return st.sql
		}
		vals[i], b = v, b[used:]
	}
	return substitute(st.sql, vals)
}

func quoteMy(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `'`, `\'`, "\x00", `\0`, "\n", `\n`, "\r", `\r`, "\x1a", `\Z`)
	return "'" + r.Replace(s) + "'"
}

// myValue decodes one binary-protocol value; used is the bytes consumed.
func myValue(t uint16, b []byte) (string, int) {
	unsigned := t&0x8000 != 0
	switch byte(t) {
	case 0x01: // TINY
		if len(b) >= 1 {
			if unsigned {
				return strconv.Itoa(int(b[0])), 1
			}
			return strconv.Itoa(int(int8(b[0]))), 1
		}
	case 0x02, 0x0d: // SHORT, YEAR
		if len(b) >= 2 {
			v := binary.LittleEndian.Uint16(b)
			if unsigned {
				return strconv.Itoa(int(v)), 2
			}
			return strconv.Itoa(int(int16(v))), 2
		}
	case 0x03, 0x09: // LONG, INT24
		if len(b) >= 4 {
			v := binary.LittleEndian.Uint32(b)
			if unsigned {
				return strconv.FormatUint(uint64(v), 10), 4
			}
			return strconv.Itoa(int(int32(v))), 4
		}
	case 0x08: // LONGLONG
		if len(b) >= 8 {
			v := binary.LittleEndian.Uint64(b)
			if unsigned {
				return strconv.FormatUint(v, 10), 8
			}
			return strconv.FormatInt(int64(v), 10), 8
		}
	case 0x04:
		if len(b) >= 4 {
			return strconv.FormatFloat(float64(math.Float32frombits(binary.LittleEndian.Uint32(b))), 'g', -1, 32), 4
		}
	case 0x05:
		if len(b) >= 8 {
			return strconv.FormatFloat(math.Float64frombits(binary.LittleEndian.Uint64(b)), 'g', -1, 64), 8
		}
	case 0x06: // NULL
		return "NULL", 0
	case 0x0a, 0x07, 0x0c: // DATE, TIMESTAMP, DATETIME
		if len(b) < 1 || len(b) < 1+int(b[0]) {
			return "", -1
		}
		l := int(b[0])
		d := b[1 : 1+l]
		if l == 0 {
			return "'0000-00-00 00:00:00'", 1
		}
		s := fmt.Sprintf("%04d-%02d-%02d", binary.LittleEndian.Uint16(d), d[2], d[3])
		if l >= 7 {
			s += fmt.Sprintf(" %02d:%02d:%02d", d[4], d[5], d[6])
		}
		if l >= 11 {
			s += fmt.Sprintf(".%06d", binary.LittleEndian.Uint32(d[7:]))
		}
		return "'" + s + "'", 1 + l
	case 0x0b: // TIME
		if len(b) < 1 || len(b) < 1+int(b[0]) {
			return "", -1
		}
		l := int(b[0])
		d := b[1 : 1+l]
		if l == 0 {
			return "'00:00:00'", 1
		}
		sign := ""
		if d[0] == 1 {
			sign = "-"
		}
		h := int(binary.LittleEndian.Uint32(d[1:]))*24 + int(d[5])
		s := fmt.Sprintf("%s%02d:%02d:%02d", sign, h, d[6], d[7])
		if l >= 12 {
			s += fmt.Sprintf(".%06d", binary.LittleEndian.Uint32(d[8:]))
		}
		return "'" + s + "'", 1 + l
	default: // strings, decimals, blobs, JSON, enums: length-encoded bytes
		n, sz := lenenc(b)
		if sz == 0 || sz+int(n) > len(b) {
			return "", -1
		}
		v := b[sz : sz+int(n)]
		if byte(t) == 0xf6 || byte(t) == 0x00 { // DECIMAL / NEWDECIMAL
			return string(v), sz + int(n)
		}
		if byte(t) >= 0xf9 && byte(t) <= 0xfc && !isText(v) {
			return "X'" + hex.EncodeToString(v) + "'", sz + int(n)
		}
		return quoteMy(string(v)), sz + int(n)
	}
	return "", -1
}

func isText(b []byte) bool {
	for _, c := range b {
		if c < 0x09 || c == 0x7f {
			return false
		}
	}
	return true
}

// substitute replaces ? placeholders outside quotes, backticks and comments.
func substitute(sql string, vals []string) string {
	var b strings.Builder
	n := 0
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			j := i + 1
			for j < len(sql) && sql[j] != c {
				if sql[j] == '\\' && c != '`' {
					j++
				}
				j++
			}
			if j >= len(sql) {
				j = len(sql) - 1
			}
			b.WriteString(sql[i : j+1])
			i = j
		case c == '-' && i+1 < len(sql) && sql[i+1] == '-' || c == '#':
			j := strings.IndexByte(sql[i:], '\n')
			if j < 0 {
				j = len(sql) - i - 1
			}
			b.WriteString(sql[i : i+j+1])
			i += j
		case c == '/' && i+1 < len(sql) && sql[i+1] == '*':
			j := strings.Index(sql[i+2:], "*/")
			if j < 0 {
				b.WriteString(sql[i:])
				return b.String()
			}
			b.WriteString(sql[i : i+j+4])
			i += j + 3
		case c == '?' && n < len(vals):
			b.WriteString(vals[n])
			n++
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func (p *Proxy) probeMy(ctx context.Context, token string) error {
	u, err := url.Parse(p.ProxyURL())
	if err != nil {
		return err
	}
	pass, _ := u.User.Password()
	auth := u.User.Username()
	if pass != "" {
		auth += ":" + pass
	}
	d, err := sql.Open("mysql", fmt.Sprintf("%s@tcp(%s)/%s", auth, u.Host, strings.TrimPrefix(u.Path, "/")))
	if err != nil {
		return err
	}
	defer d.Close()
	var s string
	err = d.QueryRowContext(ctx, "SELECT ?", token).Scan(&s)
	if err == nil && s != token {
		err = errors.New("probe returned the wrong value")
	}
	return err
}
