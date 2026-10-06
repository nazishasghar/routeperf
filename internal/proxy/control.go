package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/nazishasghar/routeperf/internal/analyze"
	"github.com/nazishasghar/routeperf/internal/db"
)

type wireRecord struct {
	SQL    string   `json:"sql"`
	Params []string `json:"params,omitempty"`
	Ms     float64  `json:"ms"`
	Conn   string   `json:"conn"`
}

// ControlAddr is the default control address next to a proxy listen address
// (port + 1).
func ControlAddr(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return ""
	}
	n, _ := strconv.Atoi(port)
	return net.JoinHostPort(host, strconv.Itoa(n+1))
}

// ServeControl exposes capture over HTTP so `routeperf run` can attach to a
// long-lived `routeperf proxy` the app is already connected to.
func (p *Proxy) ServeControl(addr string) (*http.Server, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		open, total := p.Connections()
		_ = json.NewEncoder(w).Encode(map[string]any{"dialect": p.dialect, "target": p.target, "listen": p.listen, "open": open, "total": total})
	})
	mux.HandleFunc("POST /capture", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("on") == "1" {
			_ = p.StartCapture(r.Context())
		} else {
			_ = p.StopCapture(r.Context())
		}
	})
	mux.HandleFunc("GET /mark", func(w http.ResponseWriter, r *http.Request) {
		m, _ := p.Mark(r.Context())
		_ = json.NewEncoder(w).Encode(m.Offset)
	})
	mux.HandleFunc("GET /window", func(w http.ResponseWriter, r *http.Request) {
		from, _ := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
		to, _ := strconv.ParseInt(r.URL.Query().Get("to"), 10, 64)
		p.mu.Lock()
		lo, hi := from-p.base, to-p.base
		if lo < 0 {
			lo = 0
		}
		if hi > int64(len(p.recs)) {
			hi = int64(len(p.recs))
		}
		out := []wireRecord{}
		for _, rc := range p.recs[min(lo, hi):hi] {
			out = append(out, wireRecord{rc.sql, rc.params, rc.ms, rc.conn})
		}
		p.mu.Unlock()
		_ = json.NewEncoder(w).Encode(out)
	})
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("control listen %s: %w", addr, err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	return srv, nil
}

// Remote is a db.Capture backed by a running `routeperf proxy`.
type Remote struct {
	base    string
	dialect string
	listen  string
	target  string
	dbURL   string
	catalog func() map[string]*db.Table
	cl      *http.Client
}

// Attach connects to the control API of a running proxy (error if none).
func Attach(control, dbURL string, catalog func() map[string]*db.Table) (*Remote, error) {
	r := &Remote{base: "http://" + control, dbURL: dbURL, catalog: catalog, cl: &http.Client{Timeout: 10 * time.Second}}
	resp, err := r.cl.Get(r.base + "/health")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var h struct{ Dialect, Target, Listen string }
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return nil, err
	}
	r.dialect, r.target, r.listen = h.Dialect, h.Target, h.Listen
	return r, nil
}

func (r *Remote) StartCapture(ctx context.Context) error {
	resp, err := r.cl.Post(r.base+"/capture?on=1", "", nil)
	if err == nil {
		resp.Body.Close()
	}
	return err
}

func (r *Remote) StopCapture(ctx context.Context) error {
	resp, err := r.cl.Post(r.base+"/capture?on=0", "", nil)
	if err == nil {
		resp.Body.Close()
	}
	return err
}

func (r *Remote) Mark(ctx context.Context) (db.Mark, error) {
	resp, err := r.cl.Get(r.base + "/mark")
	if err != nil {
		return db.Mark{}, err
	}
	defer resp.Body.Close()
	var off int64
	err = json.NewDecoder(resp.Body).Decode(&off)
	return db.Mark{Offset: off, Time: time.Now()}, err
}

func (r *Remote) Window(ctx context.Context, from, to db.Mark) ([]analyze.Stmt, error) {
	if to.Offset <= from.Offset {
		return nil, nil
	}
	resp, err := r.cl.Get(fmt.Sprintf("%s/window?from=%d&to=%d", r.base, from.Offset, to.Offset))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var recs []wireRecord
	if err := json.NewDecoder(resp.Body).Decode(&recs); err != nil {
		return nil, err
	}
	cat := r.catalog()
	out := make([]analyze.Stmt, 0, len(recs))
	for _, rc := range recs {
		st := db.MakeStmt(r.dialect, rc.SQL, cat)
		st.Params, st.DurMs, st.Conn = rc.Params, rc.Ms, rc.Conn
		out = append(out, st)
	}
	return out, nil
}

func (r *Remote) LogSource() string {
	return fmt.Sprintf("routeperf proxy %s → %s", r.listen, r.target)
}

func (r *Remote) TimingSource() string { return "proxy (wire time)" }

// ProxyURL is the database URL the app should use.
func (r *Remote) ProxyURL() string {
	p := &Proxy{dialect: r.dialect, listen: r.listen, rawURL: r.dbURL}
	return p.ProxyURL()
}

func (r *Remote) Probe(ctx context.Context) error {
	p := &Proxy{dialect: r.dialect, listen: r.listen, rawURL: r.dbURL}
	token := fmt.Sprintf("rp_probe_%d", time.Now().UnixNano())
	m0, _ := r.Mark(ctx)
	if err := p.probeQuery(ctx, token); err != nil {
		return fmt.Errorf("query through the proxy failed: %w", err)
	}
	for i := 0; i < 20; i++ {
		m1, _ := r.Mark(ctx)
		stmts, _ := r.Window(ctx, m0, m1)
		for _, st := range stmts {
			if containsToken(st, token) {
				return nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("probe query went through the proxy but was not captured")
}

func containsToken(st analyze.Stmt, token string) bool {
	if len(st.SQL) >= len(token) && contains(st.SQL, token) {
		return true
	}
	for _, v := range st.Params {
		if v == token {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
