// Package proxy captures the SQL an application sends by sitting between it
// and the database on the wire (Postgres and MySQL protocols). It needs no
// admin rights and no access to server logs, so it works for managed and
// shared databases: point the app at the proxy instead of the database.
package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/nazishasghar/routeperf/internal/analyze"
	"github.com/nazishasghar/routeperf/internal/db"
)

// record is one statement seen on the wire (parsed lazily in Window).
type record struct {
	sql    string
	params []string // Postgres bind values ($n); nil when inlined (MySQL)
	ms     float64
	conn   string
	failed bool
}

type Proxy struct {
	dialect   string
	listen    string
	target    string
	upTLS     *tls.Config // Postgres: TLS to the server
	rawURL    string
	ln        net.Listener
	catalog   func() map[string]*db.Table
	mu        sync.Mutex
	recs      []record
	base      int64 // offset of recs[0]
	capturing bool
	conns     int64
	seen      int64 // client connections ever accepted
	wg        sync.WaitGroup
	closed    chan struct{}
}

const maxRecords = 1 << 21

// New starts listening on listen and forwards to the database in dbURL.
func New(dbURL, listen string, catalog func() map[string]*db.Table) (*Proxy, error) {
	u, err := url.Parse(dbURL)
	if err != nil {
		return nil, err
	}
	p := &Proxy{listen: listen, rawURL: dbURL, catalog: catalog, closed: make(chan struct{})}
	host, port := u.Hostname(), u.Port()
	switch u.Scheme {
	case "postgres", "postgresql":
		p.dialect = "postgres"
		if port == "" {
			port = "5432"
		}
		switch u.Query().Get("sslmode") {
		case "require", "verify-ca":
			p.upTLS = &tls.Config{InsecureSkipVerify: true, ServerName: host}
		case "verify-full":
			p.upTLS = &tls.Config{ServerName: host}
		}
	case "mysql":
		p.dialect = "mysql"
		if port == "" {
			port = "3306"
		}
	default:
		return nil, fmt.Errorf("proxy: unsupported scheme %q", u.Scheme)
	}
	if host == "" {
		host = "127.0.0.1"
	}
	p.target = net.JoinHostPort(host, port)
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, fmt.Errorf("proxy listen %s: %w", listen, err)
	}
	p.ln = ln
	go p.accept()
	return p, nil
}

func (p *Proxy) accept() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			select {
			case <-p.closed:
				return
			default:
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
		p.wg.Add(1)
		p.mu.Lock()
		p.conns++
		p.seen++
		id := fmt.Sprintf("px%d", p.seen)
		p.mu.Unlock()
		go func() {
			defer p.wg.Done()
			defer func() {
				p.mu.Lock()
				p.conns--
				p.mu.Unlock()
			}()
			var err error
			if p.dialect == "postgres" {
				err = p.servePG(c, id)
			} else {
				err = p.serveMy(c, id)
			}
			_ = err
		}()
	}
}

// Close stops accepting; open connections keep forwarding until they end.
func (p *Proxy) Close() {
	select {
	case <-p.closed:
	default:
		close(p.closed)
		p.ln.Close()
	}
}

func (p *Proxy) add(r record) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.capturing {
		return
	}
	if len(p.recs) >= maxRecords { // drop the oldest half
		n := len(p.recs) / 2
		p.recs = append([]record(nil), p.recs[n:]...)
		p.base += int64(n)
	}
	p.recs = append(p.recs, r)
}

// Connections reports open and total client connections.
func (p *Proxy) Connections() (open, total int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conns, p.seen
}

// ---------------------------------------------------------------- db.Capture

func (p *Proxy) StartCapture(context.Context) error {
	p.mu.Lock()
	p.capturing = true
	p.mu.Unlock()
	return nil
}

func (p *Proxy) StopCapture(context.Context) error {
	p.mu.Lock()
	p.capturing = false
	p.mu.Unlock()
	return nil
}

func (p *Proxy) Mark(context.Context) (db.Mark, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return db.Mark{Offset: p.base + int64(len(p.recs)), Time: time.Now()}, nil
}

func (p *Proxy) Window(_ context.Context, from, to db.Mark) ([]analyze.Stmt, error) {
	p.mu.Lock()
	lo, hi := from.Offset-p.base, to.Offset-p.base
	if lo < 0 {
		lo = 0
	}
	if hi > int64(len(p.recs)) {
		hi = int64(len(p.recs))
	}
	var recs []record
	if hi > lo {
		recs = append(recs, p.recs[lo:hi]...)
	}
	p.mu.Unlock()
	cat := p.catalog()
	out := make([]analyze.Stmt, 0, len(recs))
	for _, r := range recs {
		st := db.MakeStmt(p.dialect, r.sql, cat)
		st.Params, st.DurMs, st.Conn = r.params, r.ms, r.conn
		out = append(out, st)
	}
	return out, nil
}

func (p *Proxy) LogSource() string {
	return fmt.Sprintf("wire proxy %s → %s", p.listen, p.target)
}

func (p *Proxy) TimingSource() string { return "proxy (wire time)" }

// ProxyURL is the database URL with the proxy as host (what the app should use).
func (p *Proxy) ProxyURL() string {
	u, err := url.Parse(p.rawURL)
	if err != nil {
		return ""
	}
	u.Host = p.listen
	q := u.Query()
	if p.dialect == "postgres" {
		q.Set("sslmode", "disable")
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// Probe connects through the proxy and checks a tagged query is captured.
func (p *Proxy) Probe(ctx context.Context) error {
	token := fmt.Sprintf("rp_probe_%d", time.Now().UnixNano())
	m0, _ := p.Mark(ctx)
	if err := p.probeQuery(ctx, token); err != nil {
		return fmt.Errorf("query through the proxy failed: %w", err)
	}
	for i := 0; i < 20; i++ {
		m1, _ := p.Mark(ctx)
		stmts, _ := p.Window(ctx, m0, m1)
		for _, st := range stmts {
			if strings.Contains(st.SQL, token) {
				return nil
			}
			for _, v := range st.Params {
				if v == token {
					return nil
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("probe query went through the proxy but was not captured")
}

// pipe copies until either side closes.
func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
}
