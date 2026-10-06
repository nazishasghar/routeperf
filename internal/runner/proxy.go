package runner

import (
	"context"

	"github.com/nazishasghar/routeperf/internal/db"
	"github.com/nazishasghar/routeperf/internal/proxy"
)

// startProxy captures SQL on the wire instead of from the server log: it
// attaches to a running `routeperf proxy` (the app stays connected to it) or
// starts one in-process for apps that connect lazily.
func (r *Runner) startProxy(ctx context.Context) error {
	cat := func() map[string]*db.Table { return r.tables }
	if rm, err := proxy.Attach(proxy.ControlAddr(r.cfg.Capture.ProxyListen), r.cfg.DB.URL, cat); err == nil {
		r.cap = rm
		return nil
	}
	px, err := proxy.New(r.cfg.DB.URL, r.cfg.Capture.ProxyListen, cat)
	if err != nil {
		return err
	}
	r.cap = px
	r.closers = append(r.closers, px.Close)
	return nil
}

// ProxyURL is the database URL the app should use in proxy capture mode.
func (r *Runner) ProxyURL() string {
	if px, ok := r.cap.(interface{ ProxyURL() string }); ok {
		return redactURL(px.ProxyURL())
	}
	return ""
}
