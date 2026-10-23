package runner

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/nazishasghar/routeperf/internal/analyze"
	"github.com/nazishasghar/routeperf/internal/db"
	"github.com/nazishasghar/routeperf/internal/inputs"
)

type slowTransport struct{ delay time.Duration }

func (s slowTransport) Send(ctx context.Context, _ *inputs.Request) (int, []byte, http.Header, float64, error) {
	select {
	case <-time.After(s.delay):
		return 200, nil, nil, float64(s.delay.Milliseconds()), nil
	case <-ctx.Done():
		return 0, nil, nil, 0, ctx.Err()
	}
}

func (slowTransport) Close() {}

type loadFakeDB struct{ db.DB }

func (loadFakeDB) Mark(context.Context) (db.Mark, error) { return db.Mark{}, nil }
func (loadFakeDB) Window(context.Context, db.Mark, db.Mark) ([]analyze.Stmt, error) {
	return nil, nil
}
func (loadFakeDB) LoadSample(ctx context.Context) (db.LoadSample, error) {
	return db.LoadSample{}, nil
}

func TestLoadLevelKeepsRequestsInFlightAtDeadline(t *testing.T) {
	f := loadFakeDB{}
	r := &Runner{cfg: &Config{}, db: f, cap: f, tr: slowTransport{delay: 150 * time.Millisecond}}
	pool := []*inputs.Request{{Method: "GET", Path: "/slow", Header: map[string]string{}}}

	step := r.loadLevel(context.Background(), pool, 2, 100*time.Millisecond)

	if step.Requests == 0 || step.P50 < 100 {
		t.Fatalf("requests running at the deadline were dropped: %+v", step)
	}
}
