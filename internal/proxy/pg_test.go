package proxy

import (
	"encoding/binary"
	"errors"
	"net"
	"testing"
	"time"
)

func newTestConn() (*pgConn, *Proxy) {
	p := &Proxy{capturing: true, closed: make(chan struct{})}
	return &pgConn{p: p, id: "t", stmts: map[string]*pgStmt{}, portals: map[string]*pgPortal{}}, p
}

func cstr(s string) []byte { return append([]byte(s), 0) }

// extended runs Parse/Bind/Execute for sql on the client side.
func extended(pc *pgConn, sql string) {
	pc.onClient('P', append(cstr(""), append(cstr(sql), 0, 0)...))
	pc.onClient('B', append(append(cstr(""), cstr("")...), 0, 0, 0, 0, 0, 0))
	pc.onClient('E', append(cstr(""), 0, 0, 0, 0))
}

func recorded(p *Proxy) []string {
	var out []string
	for _, r := range p.recs {
		out = append(out, r.sql)
	}
	return out
}

func TestSimpleQueryErrorIsNotRecorded(t *testing.T) {
	pc, p := newTestConn()
	pc.onServer('Z', nil) // startup handshake

	pc.onClient('Q', cstr("SELECT * FROM missing_table"))
	pc.onServer('E', nil)
	pc.onServer('Z', nil)
	pc.onClient('Q', cstr("SELECT 1"))
	pc.onServer('C', nil)
	pc.onServer('Z', nil)

	if got := recorded(p); len(got) != 1 || got[0] != "SELECT 1" {
		t.Fatalf("recorded %q, want only the successful query", got)
	}
}

func TestErrorDoesNotDropPipelinedNextBatch(t *testing.T) {
	pc, p := newTestConn()
	pc.onServer('Z', nil)

	extended(pc, "INSERT bad")
	pc.onClient('S', nil)
	extended(pc, "SELECT next") // pipelined before the first reply arrives
	pc.onClient('S', nil)

	pc.onServer('E', nil) // batch 1 fails
	pc.onServer('Z', nil)
	pc.onServer('C', nil) // batch 2 succeeds
	pc.onServer('Z', nil)

	if got := recorded(p); len(got) != 1 || got[0] != "SELECT next" {
		t.Fatalf("recorded %q, want [SELECT next]", got)
	}
}

func TestPgArrayDimensions(t *testing.T) {
	arr := func(elem uint32, dims []uint32, vals ...int32) []byte {
		b := make([]byte, 12)
		binary.BigEndian.PutUint32(b, uint32(len(dims)))
		binary.BigEndian.PutUint32(b[8:], elem)
		for _, d := range dims {
			b = binary.BigEndian.AppendUint32(b, d)
			b = binary.BigEndian.AppendUint32(b, 1)
		}
		for _, v := range vals {
			b = binary.BigEndian.AppendUint32(b, 4)
			b = binary.BigEndian.AppendUint32(b, uint32(v))
		}
		return b
	}
	if got := pgArray(arr(23, []uint32{3}, 1, 2, 3)); got != "{1,2,3}" {
		t.Errorf("1-D = %q", got)
	}
	if got := pgArray(arr(23, []uint32{2, 2}, 1, 2, 3, 4)); got != "{{1,2},{3,4}}" {
		t.Errorf("2-D = %q, want {{1,2},{3,4}}", got)
	}
}

type tempErrListener struct {
	net.Listener
	calls int
}

func (l *tempErrListener) Accept() (net.Conn, error) {
	l.calls++
	if l.calls == 1 {
		return nil, errors.New("accept tcp: too many open files")
	}
	return nil, net.ErrClosed
}

func TestAcceptSurvivesTransientError(t *testing.T) {
	l := &tempErrListener{}
	p := &Proxy{ln: l, closed: make(chan struct{})}
	done := make(chan struct{})
	go func() { p.accept(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("accept loop did not finish")
	}
	if l.calls < 2 {
		t.Fatalf("accept loop gave up after the first transient error (calls=%d)", l.calls)
	}
}
