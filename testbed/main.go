// Command testbed is a small API with deliberately planted performance
// problems, used to verify routeperf. Works with Postgres and MySQL.
//
//	go run ./testbed --db-url postgres://localhost:5432/routeperf_testbed --seed
//	go run ./testbed --db-url postgres://localhost:5432/routeperf_testbed --addr :8088
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
)

var (
	db         *sql.DB
	dialect    string
	dbName     string
	sqlcomment bool
)

// sc tags a statement with the request's traceparent, as sqlcommenter does.
func sc(r *http.Request, s string) string {
	if !sqlcomment {
		return s
	}
	if tp := r.Header.Get("traceparent"); tp != "" {
		return s + " /*traceparent='" + url.QueryEscape(tp) + "'*/"
	}
	return s
}

// sq is q (placeholder conversion) plus the sqlcommenter tag.
func sq(r *http.Request, s string) string { return sc(r, q(s)) }

func main() {
	dbURL := flag.String("db-url", "", "postgres://… or mysql://…")
	addr := flag.String("addr", ":8088", "listen address")
	seed := flag.Bool("seed", false, "create schema + bulk data and exit")
	users := flag.Int("users", 20000, "users to seed")
	opu := flag.Int("orders-per-user", 10, "orders per user")
	ipo := flag.Int("items-per-order", 3, "items per order")
	spu := flag.Int("sessions-per-user", 2, "sessions per user")
	noise := flag.Duration("background-noise", 0, "run a background query at this interval (simulates a cron job), e.g. 100ms")
	grpcAddr := flag.String("grpc-addr", "", "also serve the gRPC API (with server reflection) on this address, e.g. 127.0.0.1:50051")
	flag.BoolVar(&sqlcomment, "sqlcommenter", false, "append /*traceparent='…'*/ from the request to every SQL statement (like OpenTelemetry sqlcommenter)")
	flag.Parse()
	if *dbURL == "" {
		log.Fatal("--db-url required")
	}
	driver, dsn, dbname, adminDSN := parse(*dbURL)
	dbName = dbname
	if *seed {
		admin, err := sql.Open(driver, adminDSN)
		must(err)
		if dialect == "postgres" {
			var n int
			_ = admin.QueryRow("SELECT count(*) FROM pg_database WHERE datname = $1", dbname).Scan(&n)
			if n == 0 {
				_, err = admin.Exec("CREATE DATABASE " + dbname)
				must(err)
			}
		} else {
			_, err = admin.Exec("CREATE DATABASE IF NOT EXISTS " + dbname)
			must(err)
		}
		admin.Close()
	}
	var err error
	db, err = sql.Open(driver, dsn)
	must(err)
	db.SetMaxOpenConns(10)
	must(db.Ping())
	if *seed {
		t0 := time.Now()
		seedData(*users, *opu, *ipo, *spu)
		log.Printf("seeded in %s", time.Since(t0).Round(time.Millisecond))
		for _, t := range []string{"users", "orders", "order_items", "sessions", "events"} {
			var n int
			_ = db.QueryRow("SELECT COUNT(*) FROM " + t).Scan(&n)
			log.Printf("  %-12s %d rows", t, n)
		}
		return
	}
	if *noise > 0 {
		go func() { // simulated background job hitting the same DB
			for range time.Tick(*noise) {
				var n int
				_ = db.QueryRow("SELECT COUNT(*) FROM sessions WHERE token LIKE 'a%'").Scan(&n)
			}
		}()
	}
	if *grpcAddr != "" {
		serveGRPC(*grpcAddr)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /graphql", authed(graphqlHandler))
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]any{"ok": true}) })
	mux.HandleFunc("GET /swagger.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(strings.ReplaceAll(specJSON, "{{ADDR}}", "http://localhost"+*addr)))
	})
	mux.HandleFunc("POST /auth/login", login)
	mux.HandleFunc("GET /users", authed(listUsers))
	mux.HandleFunc("POST /users", authed(createUser))
	mux.HandleFunc("GET /users/{id}", authed(getUser))
	mux.HandleFunc("PUT /users/{id}", authed(updateUser))
	mux.HandleFunc("DELETE /users/{id}", authed(deleteUser))
	mux.HandleFunc("GET /users/{id}/orders", authed(userOrders))
	mux.HandleFunc("GET /users/{id}/recommendations", authed(recommendations))
	mux.HandleFunc("GET /orders", authed(listOrders))
	mux.HandleFunc("POST /orders", authed(createOrder))
	mux.HandleFunc("GET /orders/search", authed(searchOrders))
	mux.HandleFunc("GET /reports/sales", authed(salesReport))
	mux.HandleFunc("POST /notes", authed(createNote))
	mux.HandleFunc("GET /notes/{id}", authed(getNote))
	mux.HandleFunc("PUT /notes/{id}", authed(updateNote))
	mux.HandleFunc("DELETE /notes/{id}", authed(deleteNote))
	mux.HandleFunc("GET /events/search", authed(searchEvents))
	mux.HandleFunc("GET /users/{id}/events", authed(userEvents))
	mux.HandleFunc("GET /users/{id}/totals", authed(userTotals))
	mux.HandleFunc("GET /orders/by-country", authed(ordersByCountry))
	mux.HandleFunc("GET /archive/orders/{id}", authed(archivedOrder))
	mux.HandleFunc("GET /events/feed", authed(eventFeed))
	mux.HandleFunc("POST /sessions", authed(createSession))
	mux.HandleFunc("POST /users/{id}/avatar", authed(uploadAvatar))
	log.Printf("testbed (%s) listening on %s", dialect, *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func parse(raw string) (driver, dsn, dbname, admin string) {
	u, err := url.Parse(raw)
	must(err)
	dbname = strings.TrimPrefix(u.Path, "/")
	switch u.Scheme {
	case "postgres", "postgresql":
		dialect = "postgres"
		a := *u
		a.Path = "/postgres"
		return "pgx", raw, dbname, a.String()
	case "mysql":
		dialect = "mysql"
		auth := u.User.Username()
		if p, ok := u.User.Password(); ok {
			auth += ":" + p
		}
		host := u.Host
		if u.Port() == "" {
			host += ":3306"
		}
		base := fmt.Sprintf("%s@tcp(%s)/", auth, host)
		return "mysql", base + dbname + "?parseTime=true&clientFoundRows=true", dbname, base + "?parseTime=true"
	}
	log.Fatalf("unsupported scheme %s", u.Scheme)
	return
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

// q converts ? placeholders to $n for Postgres.
func q(s string) string {
	if dialect != "postgres" {
		return s
	}
	var b strings.Builder
	n := 0
	for _, r := range s {
		if r == '?' {
			n++
			b.WriteString("$" + strconv.Itoa(n))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func seedData(u, opu, ipo, spu int) {
	var stmts []string
	year := time.Now().Year()
	if dialect == "postgres" {
		stmts = []string{
			"DROP TABLE IF EXISTS events, notes, order_items, orders, sessions, users CASCADE",
			"DROP SCHEMA IF EXISTS archive CASCADE",
			"CREATE TABLE users (id bigserial PRIMARY KEY, email varchar(255) NOT NULL UNIQUE, name varchar(100), country char(2), created_at timestamp NOT NULL DEFAULT now())",
			// planted: orders.user_id FK without index
			"CREATE TABLE orders (id bigserial PRIMARY KEY, user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE, status varchar(16) NOT NULL, total_cents int NOT NULL, created_at timestamp NOT NULL)",
			"CREATE TABLE order_items (id bigserial PRIMARY KEY, order_id bigint NOT NULL REFERENCES orders(id) ON DELETE CASCADE, sku varchar(32), qty int, price_cents int)",
			"CREATE INDEX order_items_order_id_idx ON order_items(order_id)",
			// planted: sessions.user_id FK without index
			"CREATE TABLE sessions (id bigserial PRIMARY KEY, user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE, token varchar(64), created_at timestamp)",
			fmt.Sprintf(`INSERT INTO users (email, name, country, created_at) SELECT 'user'||g||'@example.com', 'User '||g, (ARRAY['US','IN','DE','GB','FR'])[1+g%%5], now() - (g%%730) * interval '1 day' FROM generate_series(1,%d) g`, u),
			fmt.Sprintf(`INSERT INTO orders (user_id, status, total_cents, created_at) SELECT 1 + (g %% %d), (ARRAY['paid','pending','refunded','shipped'])[1+(g*7)%%4], (g*37)%%50000+100, now() - ((g*13)%%730) * interval '1 day' - (g%%86400) * interval '1 second' FROM generate_series(1,%d) g`, u, u*opu),
			fmt.Sprintf(`INSERT INTO order_items (order_id, sku, qty, price_cents) SELECT 1 + (g %% %d), 'SKU-'||(g%%500), 1+g%%5, (g*17)%%10000 FROM generate_series(1,%d) g`, u*opu, u*opu*ipo),
			fmt.Sprintf(`INSERT INTO sessions (user_id, token, created_at) SELECT 1 + (g %% %d), md5(g::text), now() FROM generate_series(1,%d) g`, u, u*spu),
			// UUID-keyed resource (indexed FK)
			"CREATE TABLE notes (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE, body text NOT NULL, created_at timestamp NOT NULL DEFAULT now())",
			"CREATE INDEX notes_user_id_idx ON notes(user_id)",
			fmt.Sprintf(`INSERT INTO notes (user_id, body) SELECT g, 'note '||g FROM generate_series(1,%d) g`, u),
			// partitioned table with a composite primary key; planted: no index on kind
			"CREATE TABLE events (id bigint NOT NULL, user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE, kind varchar(16) NOT NULL, created_at timestamp NOT NULL, PRIMARY KEY (id, created_at)) PARTITION BY RANGE (created_at)",
			fmt.Sprintf("CREATE TABLE events_old PARTITION OF events FOR VALUES FROM (MINVALUE) TO ('%d-01-01')", year-1),
			fmt.Sprintf("CREATE TABLE events_prev PARTITION OF events FOR VALUES FROM ('%d-01-01') TO ('%d-01-01')", year-1, year),
			fmt.Sprintf("CREATE TABLE events_cur PARTITION OF events FOR VALUES FROM ('%d-01-01') TO (MAXVALUE)", year),
			"CREATE INDEX events_user_id_idx ON events (user_id)",
			fmt.Sprintf(`INSERT INTO events SELECT g, 1 + (g %% %d), (ARRAY['click','view','buy','share'])[1+g%%4], now() - (g%%730) * interval '1 day' FROM generate_series(1,%d) g`, u, u*5),
			// a view over an unindexed join, and a same-named table in another schema
			"CREATE VIEW user_order_totals AS SELECT u.id, u.email, count(o.id) AS orders, coalesce(sum(o.total_cents), 0) AS total_cents FROM users u LEFT JOIN orders o ON o.user_id = u.id GROUP BY u.id, u.email",
			"CREATE SCHEMA archive",
			"CREATE TABLE archive.orders (id bigint PRIMARY KEY, user_id bigint NOT NULL, status varchar(16), total_cents int, created_at timestamp)",
			"INSERT INTO archive.orders SELECT id, user_id, status, total_cents, created_at FROM orders WHERE id % 5 = 0",
			"ANALYZE users", "ANALYZE orders", "ANALYZE order_items", "ANALYZE sessions", "ANALYZE notes", "ANALYZE events", "ANALYZE archive.orders",
		}
	} else {
		seq := func(n int) string {
			return fmt.Sprintf("WITH RECURSIVE seq(g) AS (SELECT 1 UNION ALL SELECT g+1 FROM seq WHERE g < %d) ", n)
		}
		stmts = []string{
			"SET SESSION cte_max_recursion_depth = 100000000",
			"DROP VIEW IF EXISTS user_order_totals",
			"DROP TABLE IF EXISTS events, notes, order_items, orders, sessions, users",
			"CREATE TABLE users (id BIGINT AUTO_INCREMENT PRIMARY KEY, email VARCHAR(255) NOT NULL UNIQUE, name VARCHAR(100), country CHAR(2), created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP)",
			// planted: no index (and no FK, which would auto-index) on orders.user_id
			"CREATE TABLE orders (id BIGINT AUTO_INCREMENT PRIMARY KEY, user_id BIGINT NOT NULL, status VARCHAR(16) NOT NULL, total_cents INT NOT NULL, created_at DATETIME NOT NULL)",
			"CREATE TABLE order_items (id BIGINT AUTO_INCREMENT PRIMARY KEY, order_id BIGINT NOT NULL, sku VARCHAR(32), qty INT, price_cents INT, KEY order_items_order_id_idx (order_id))",
			"CREATE TABLE sessions (id BIGINT AUTO_INCREMENT PRIMARY KEY, user_id BIGINT NOT NULL, token VARCHAR(64), created_at DATETIME)",
			fmt.Sprintf("INSERT INTO users (email, name, country, created_at) %s SELECT CONCAT('user', g, '@example.com'), CONCAT('User ', g), ELT(1+g%%5,'US','IN','DE','GB','FR'), NOW() - INTERVAL (g%%730) DAY FROM seq", seq(u)),
			fmt.Sprintf("INSERT INTO orders (user_id, status, total_cents, created_at) %s SELECT 1 + (g %% %d), ELT(1+(g*7)%%4,'paid','pending','refunded','shipped'), (g*37)%%50000+100, NOW() - INTERVAL ((g*13)%%730) DAY - INTERVAL (g%%86400) SECOND FROM seq", seq(u*opu), u),
			fmt.Sprintf("INSERT INTO order_items (order_id, sku, qty, price_cents) %s SELECT 1 + (g %% %d), CONCAT('SKU-', g%%500), 1+g%%5, (g*17)%%10000 FROM seq", seq(u*opu*ipo), u*opu),
			fmt.Sprintf("INSERT INTO sessions (user_id, token, created_at) %s SELECT 1 + (g %% %d), MD5(g), NOW() FROM seq", seq(u*spu), u),
			"CREATE TABLE notes (id CHAR(36) PRIMARY KEY, user_id BIGINT NOT NULL, body TEXT NOT NULL, created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP, KEY notes_user_id_idx (user_id))",
			fmt.Sprintf("INSERT INTO notes (id, user_id, body) %s SELECT UUID(), g, CONCAT('note ', g) FROM seq", seq(u)),
			fmt.Sprintf("CREATE TABLE events (id BIGINT NOT NULL, user_id BIGINT NOT NULL, kind VARCHAR(16) NOT NULL, created_at DATETIME NOT NULL, PRIMARY KEY (id, created_at), KEY events_user_id_idx (user_id)) PARTITION BY RANGE (YEAR(created_at)) (PARTITION p_old VALUES LESS THAN (%d), PARTITION p_prev VALUES LESS THAN (%d), PARTITION p_cur VALUES LESS THAN MAXVALUE)", year-1, year),
			fmt.Sprintf("INSERT INTO events %s SELECT g, 1 + (g %% %d), ELT(1+g%%4,'click','view','buy','share'), NOW() - INTERVAL (g%%730) DAY FROM seq", seq(u*5), u),
			"CREATE VIEW user_order_totals AS SELECT u.id, u.email, COUNT(o.id) AS orders, COALESCE(SUM(o.total_cents), 0) AS total_cents FROM users u LEFT JOIN orders o ON o.user_id = u.id GROUP BY u.id, u.email",
			fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s_archive", dbName),
			fmt.Sprintf("DROP TABLE IF EXISTS %s_archive.orders", dbName),
			fmt.Sprintf("CREATE TABLE %s_archive.orders (id BIGINT PRIMARY KEY, user_id BIGINT NOT NULL, status VARCHAR(16), total_cents INT, created_at DATETIME)", dbName),
			fmt.Sprintf("INSERT INTO %s_archive.orders SELECT id, user_id, status, total_cents, created_at FROM orders WHERE id %% 5 = 0", dbName),
			"ANALYZE TABLE users, orders, order_items, sessions, notes, events",
			fmt.Sprintf("ANALYZE TABLE %s_archive.orders", dbName),
		}
	}
	conn, err := db.Conn(context.Background())
	must(err)
	defer conn.Close()
	for _, s := range stmts {
		if _, err := conn.ExecContext(context.Background(), s); err != nil {
			log.Fatalf("%s\n  %v", s, err)
		}
	}
}

// ------------------------------------------------------------ helpers

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

func authed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ok := r.Header.Get("Authorization") == "Bearer testtoken" || r.Header.Get("X-Api-Key") == "testkey"
		if c, err := r.Cookie("session"); err == nil && c.Value == "testsession" {
			ok = true
		}
		if !ok {
			fail(w, 401, "unauthorized")
			return
		}
		h(w, r)
	}
}

func intParam(r *http.Request, name string, def, max int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil || v < 1 {
		return def
	}
	if v > max {
		return max
	}
	return v
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil
}

type User struct {
	ID        int64     `json:"id"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	Country   string    `json:"country"`
	CreatedAt time.Time `json:"created_at"`
}

type Item struct {
	ID         int64  `json:"id"`
	SKU        string `json:"sku"`
	Qty        int    `json:"qty"`
	PriceCents int    `json:"price_cents"`
}

type Order struct {
	ID         int64     `json:"id"`
	UserID     int64     `json:"user_id"`
	Status     string    `json:"status"`
	TotalCents int       `json:"total_cents"`
	CreatedAt  time.Time `json:"created_at"`
	Items      []Item    `json:"items,omitempty"`
}

func scanOrders(rows *sql.Rows) ([]Order, error) {
	defer rows.Close()
	var out []Order
	for rows.Next() {
		var o Order
		if err := rows.Scan(&o.ID, &o.UserID, &o.Status, &o.TotalCents, &o.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ------------------------------------------------------------ handlers

func login(w http.ResponseWriter, r *http.Request) {
	var b struct{ Email, Password string }
	_ = json.NewDecoder(r.Body).Decode(&b)
	if b.Email != "perf@test.dev" || b.Password != "secret" {
		fail(w, 401, "bad credentials")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "session", Value: "testsession", Path: "/", HttpOnly: true})
	writeJSON(w, 200, map[string]any{"data": map[string]any{"accessToken": "testtoken"}})
}

func listUsers(w http.ResponseWriter, r *http.Request) {
	limit := intParam(r, "limit", 20, 1000)
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	rows, err := db.Query(sq(r, "SELECT id, email, name, country, created_at FROM users ORDER BY id LIMIT ? OFFSET ?"), limit, offset)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		var u User
		_ = rows.Scan(&u.ID, &u.Email, &u.Name, &u.Country, &u.CreatedAt)
		out = append(out, u)
	}
	writeJSON(w, 200, out)
}

func getUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		fail(w, 400, "bad id")
		return
	}
	var u User
	err := db.QueryRow(sq(r, "SELECT id, email, name, country, created_at FROM users WHERE id = ?"), id).Scan(&u.ID, &u.Email, &u.Name, &u.Country, &u.CreatedAt)
	if err == sql.ErrNoRows {
		fail(w, 404, "not found")
		return
	} else if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, u)
}

func createUser(w http.ResponseWriter, r *http.Request) {
	var u User
	if err := json.NewDecoder(r.Body).Decode(&u); err != nil || u.Email == "" {
		fail(w, 400, "email required")
		return
	}
	var err error
	if dialect == "postgres" {
		err = db.QueryRow(sq(r, "INSERT INTO users (email, name, country) VALUES (?, ?, ?) RETURNING id"), u.Email, u.Name, u.Country).Scan(&u.ID)
	} else {
		var res sql.Result
		res, err = db.Exec(sc(r, "INSERT INTO users (email, name, country) VALUES (?, ?, ?)"), u.Email, u.Name, u.Country)
		if err == nil {
			u.ID, _ = res.LastInsertId()
		}
	}
	if err != nil {
		if strings.Contains(err.Error(), "uplicate") {
			fail(w, 409, "email exists")
			return
		}
		fail(w, 500, err.Error())
		return
	}
	w.Header().Set("Location", fmt.Sprintf("/users/%d", u.ID))
	writeJSON(w, 201, u)
}

func updateUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		fail(w, 400, "bad id")
		return
	}
	var u User
	_ = json.NewDecoder(r.Body).Decode(&u)
	res, err := db.Exec(sq(r, "UPDATE users SET name = ?, country = ? WHERE id = ?"), u.Name, u.Country, id)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		fail(w, 404, "not found")
		return
	}
	getUser(w, r)
}

func deleteUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		fail(w, 400, "bad id")
		return
	}
	var n int64
	if dialect == "postgres" {
		// planted: cascades scan unindexed orders.user_id and sessions.user_id
		res, err := db.Exec(sc(r, "DELETE FROM users WHERE id = $1"), id)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		n, _ = res.RowsAffected()
	} else {
		tx, err := db.Begin()
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		defer tx.Rollback()
		for _, s := range []string{
			"DELETE oi FROM order_items oi JOIN orders o ON o.id = oi.order_id WHERE o.user_id = ?",
			"DELETE FROM orders WHERE user_id = ?",
			"DELETE FROM sessions WHERE user_id = ?",
		} {
			if _, err := tx.Exec(sc(r, s), id); err != nil {
				fail(w, 500, err.Error())
				return
			}
		}
		res, err := tx.Exec(sc(r, "DELETE FROM users WHERE id = ?"), id)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		n, _ = res.RowsAffected()
		_ = tx.Commit()
	}
	if n == 0 {
		fail(w, 404, "not found")
		return
	}
	w.WriteHeader(204)
}

// planted: seq scan on orders.user_id + N+1 on order_items
func userOrders(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		fail(w, 400, "bad id")
		return
	}
	limit := intParam(r, "limit", 20, 1000)
	rows, err := db.Query(sq(r, "SELECT id, user_id, status, total_cents, created_at FROM orders WHERE user_id = ? ORDER BY created_at DESC LIMIT ?"), id, limit)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	orders, err := scanOrders(rows)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	for i := range orders {
		irows, err := db.Query(sq(r, "SELECT id, sku, qty, price_cents FROM order_items WHERE order_id = ?"), orders[i].ID)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		for irows.Next() {
			var it Item
			_ = irows.Scan(&it.ID, &it.SKU, &it.Qty, &it.PriceCents)
			orders[i].Items = append(orders[i].Items, it)
		}
		irows.Close()
	}
	if orders == nil {
		orders = []Order{}
	}
	writeJSON(w, 200, orders)
}

// planted: in-app O(k²) pairwise scoring
func recommendations(w http.ResponseWriter, r *http.Request) {
	limit := intParam(r, "limit", 20, 1000)
	rows, err := db.Query(sq(r, "SELECT id, user_id, status, total_cents, created_at FROM orders ORDER BY id DESC LIMIT ?"), limit)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	orders, err := scanOrders(rows)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	score := make([]float64, len(orders))
	for i := range orders {
		for j := range orders {
			if i == j {
				continue
			}
			d := float64(orders[i].TotalCents - orders[j].TotalCents)
			score[i] += 1 / (1 + math.Sqrt(math.Abs(d)+float64(j%7)))
			for k := 0; k < 8; k++ {
				score[i] += math.Sin(float64(k)+d) * 1e-9
			}
		}
	}
	idx := make([]int, len(orders))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return score[idx[a]] > score[idx[b]] })
	var top []Order
	for i := 0; i < len(idx) && i < 5; i++ {
		top = append(top, orders[idx[i]])
	}
	writeJSON(w, 200, map[string]any{"user_id": r.PathValue("id"), "recommended": top})
}

func listOrders(w http.ResponseWriter, r *http.Request) {
	limit := intParam(r, "limit", 20, 1000)
	rows, err := db.Query(sq(r, "SELECT id, user_id, status, total_cents, created_at FROM orders ORDER BY id DESC LIMIT ?"), limit)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	orders, err := scanOrders(rows)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, orders)
}

// planted: filter + sort on unindexed columns
func searchOrders(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "paid"
	}
	limit := intParam(r, "limit", 20, 1000)
	rows, err := db.Query(sq(r, "SELECT id, user_id, status, total_cents, created_at FROM orders WHERE status = ? ORDER BY created_at DESC LIMIT ?"), status, limit)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	orders, err := scanOrders(rows)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, orders)
}

// planted: full-table aggregate
func salesReport(w http.ResponseWriter, r *http.Request) {
	from, to := r.URL.Query().Get("from"), r.URL.Query().Get("to")
	if from == "" {
		from = "2000-01-01"
	}
	if to == "" {
		to = "2100-01-01"
	}
	day := "DATE(created_at)"
	if dialect == "postgres" {
		day = "date_trunc('day', created_at)"
	}
	rows, err := db.Query(sq(r, "SELECT "+day+" AS day, COUNT(*), SUM(total_cents) FROM orders WHERE created_at BETWEEN ? AND ? GROUP BY day ORDER BY day"), from, to)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer rows.Close()
	type row struct {
		Day   time.Time `json:"day"`
		Count int       `json:"count"`
		Sum   int64     `json:"sum_cents"`
	}
	out := []row{}
	for rows.Next() {
		var x row
		_ = rows.Scan(&x.Day, &x.Count, &x.Sum)
		out = append(out, x)
	}
	writeJSON(w, 200, out)
}

type Note struct {
	ID     string `json:"id"`
	UserID int64  `json:"user_id"`
	Body   string `json:"body"`
}

func createNote(w http.ResponseWriter, r *http.Request) {
	var n Note
	if err := json.NewDecoder(r.Body).Decode(&n); err != nil || n.UserID == 0 {
		fail(w, 400, "user_id and body required")
		return
	}
	var err error
	if dialect == "postgres" {
		err = db.QueryRow(sc(r, "INSERT INTO notes (user_id, body) VALUES ($1, $2) RETURNING id"), n.UserID, n.Body).Scan(&n.ID)
	} else {
		_ = db.QueryRow("SELECT UUID()").Scan(&n.ID)
		_, err = db.Exec(sc(r, "INSERT INTO notes (id, user_id, body) VALUES (?, ?, ?)"), n.ID, n.UserID, n.Body)
	}
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, n)
}

func getNote(w http.ResponseWriter, r *http.Request) {
	var n Note
	err := db.QueryRow(sq(r, "SELECT id, user_id, body FROM notes WHERE id = ?"), r.PathValue("id")).Scan(&n.ID, &n.UserID, &n.Body)
	if err == sql.ErrNoRows {
		fail(w, 404, "not found")
		return
	} else if err != nil {
		fail(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, n)
}

func updateNote(w http.ResponseWriter, r *http.Request) {
	var n Note
	_ = json.NewDecoder(r.Body).Decode(&n)
	res, err := db.Exec(sq(r, "UPDATE notes SET body = ? WHERE id = ?"), n.Body, r.PathValue("id"))
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	if c, _ := res.RowsAffected(); c == 0 {
		fail(w, 404, "not found")
		return
	}
	getNote(w, r)
}

func deleteNote(w http.ResponseWriter, r *http.Request) {
	res, err := db.Exec(sq(r, "DELETE FROM notes WHERE id = ?"), r.PathValue("id"))
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	if c, _ := res.RowsAffected(); c == 0 {
		fail(w, 404, "not found")
		return
	}
	w.WriteHeader(204)
}

// planted: no index on events.kind; scans every partition
func searchEvents(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	if kind == "" {
		kind = "buy"
	}
	limit := intParam(r, "limit", 20, 1000)
	rows, err := db.Query(sq(r, "SELECT id, user_id, kind, created_at FROM events WHERE kind = ? ORDER BY created_at DESC LIMIT ?"), kind, limit)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeRows(w, rows)
}

func userEvents(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		fail(w, 400, "bad id")
		return
	}
	limit := intParam(r, "limit", 20, 1000)
	rows, err := db.Query(sq(r, "SELECT id, user_id, kind, created_at FROM events WHERE user_id = ? ORDER BY created_at DESC LIMIT ?"), id, limit)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeRows(w, rows)
}

// planted: the view joins orders on an unindexed column
func userTotals(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		fail(w, 400, "bad id")
		return
	}
	rows, err := db.Query(sq(r, "SELECT id, email, orders, total_cents FROM user_order_totals WHERE id = ?"), id)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeRows(w, rows)
}

// planted: a join that scans both tables (cost grows with each)
func ordersByCountry(w http.ResponseWriter, r *http.Request) {
	country := r.URL.Query().Get("country")
	if country == "" {
		country = "US"
	}
	limit := intParam(r, "limit", 20, 1000)
	rows, err := db.Query(sq(r, "SELECT o.id, o.user_id, o.status, o.total_cents, o.created_at FROM orders o JOIN users u ON u.id = o.user_id WHERE u.country = ? ORDER BY o.created_at DESC LIMIT ?"), country, limit)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	orders, err := scanOrders(rows)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, orders)
}

// a same-named table in another schema (Postgres) / database (MySQL)
func archivedOrder(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		fail(w, 400, "bad id")
		return
	}
	table := "archive.orders"
	if dialect == "mysql" {
		table = dbName + "_archive.orders"
	}
	rows, err := db.Query(sq(r, "SELECT id, user_id, status, total_cents, created_at FROM "+table+" WHERE id = ?"), id)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeRows(w, rows)
}

// cursor (keyset) pagination over events
func eventFeed(w http.ResponseWriter, r *http.Request) {
	limit := intParam(r, "limit", 20, 1000)
	after, _ := strconv.ParseInt(r.URL.Query().Get("cursor"), 10, 64)
	rows, err := db.Query(sq(r, "SELECT id, user_id, kind, created_at FROM events WHERE id > ? ORDER BY id LIMIT ?"), after, limit)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer rows.Close()
	var items []map[string]any
	var last int64
	for rows.Next() {
		var id, uid int64
		var kind string
		var at time.Time
		if err := rows.Scan(&id, &uid, &kind, &at); err != nil {
			fail(w, 500, err.Error())
			return
		}
		items = append(items, map[string]any{"id": id, "user_id": uid, "kind": kind, "created_at": at})
		last = id
	}
	resp := map[string]any{"items": items, "next_cursor": nil}
	if len(items) == limit {
		resp["next_cursor"] = strconv.FormatInt(last, 10)
	}
	writeJSON(w, 200, resp)
}

// form-encoded body
func createSession(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		fail(w, 400, err.Error())
		return
	}
	uid, err := strconv.ParseInt(r.PostForm.Get("user_id"), 10, 64)
	if err != nil || r.PostForm.Get("token") == "" {
		fail(w, 400, "user_id and token required")
		return
	}
	if _, err := db.Exec(sq(r, "INSERT INTO sessions (user_id, token, created_at) VALUES (?, ?, ?)"), uid, r.PostForm.Get("token"), time.Now()); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"ok": true})
}

// multipart upload
func uploadAvatar(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		fail(w, 400, "bad id")
		return
	}
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		fail(w, 400, err.Error())
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		fail(w, 400, "file required")
		return
	}
	f.Close()
	caption := r.FormValue("caption")
	if len(caption) > 50 {
		caption = caption[:50]
	}
	res, err := db.Exec(sq(r, "UPDATE users SET name = ? WHERE id = ?"), caption, id)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		fail(w, 404, "not found")
		return
	}
	writeJSON(w, 200, map[string]any{"bytes": hdr.Size})
}

// writeRows renders any result set as a JSON array of objects.
func writeRows(w http.ResponseWriter, rows *sql.Rows) {
	defer rows.Close()
	cols, _ := rows.Columns()
	out := []map[string]any{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			fail(w, 500, err.Error())
			return
		}
		m := map[string]any{}
		for i, c := range cols {
			if b, ok := vals[i].([]byte); ok {
				m[c] = string(b)
			} else {
				m[c] = vals[i]
			}
		}
		out = append(out, m)
	}
	writeJSON(w, 200, out)
}

// planted: one INSERT per item
func createOrder(w http.ResponseWriter, r *http.Request) {
	var o Order
	if err := json.NewDecoder(r.Body).Decode(&o); err != nil || o.UserID == 0 || len(o.Items) == 0 {
		fail(w, 400, "user_id and items required")
		return
	}
	tx, err := db.Begin()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	total := 0
	for _, it := range o.Items {
		total += it.Qty * it.PriceCents
	}
	if o.Status == "" {
		o.Status = "pending"
	}
	if dialect == "postgres" {
		err = tx.QueryRow(sc(r, "INSERT INTO orders (user_id, status, total_cents, created_at) VALUES ($1, $2, $3, now()) RETURNING id"), o.UserID, o.Status, total).Scan(&o.ID)
	} else {
		var res sql.Result
		res, err = tx.Exec(sc(r, "INSERT INTO orders (user_id, status, total_cents, created_at) VALUES (?, ?, ?, NOW())"), o.UserID, o.Status, total)
		if err == nil {
			o.ID, _ = res.LastInsertId()
		}
	}
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	for _, it := range o.Items {
		if _, err := tx.Exec(sq(r, "INSERT INTO order_items (order_id, sku, qty, price_cents) VALUES (?, ?, ?, ?)"), o.ID, it.SKU, it.Qty, it.PriceCents); err != nil {
			fail(w, 500, err.Error())
			return
		}
	}
	if err := tx.Commit(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": o.ID})
}

const specJSON = `{
  "openapi": "3.0.3",
  "info": {"title": "routeperf testbed", "version": "1.0.0"},
  "servers": [{"url": "{{ADDR}}"}],
  "security": [{"bearerAuth": []}, {"apiKey": []}, {"cookieAuth": []}],
  "components": {
    "securitySchemes": {
      "bearerAuth": {"type": "http", "scheme": "bearer"},
      "apiKey": {"type": "apiKey", "in": "header", "name": "X-Api-Key"},
      "cookieAuth": {"type": "apiKey", "in": "cookie", "name": "session"}
    },
    "parameters": {
      "id": {"name": "id", "in": "path", "required": true, "schema": {"type": "integer", "minimum": 1}},
      "limit": {"name": "limit", "in": "query", "schema": {"type": "integer", "minimum": 1, "maximum": 1000, "default": 20}}
    },
    "schemas": {
      "User": {"type": "object", "properties": {
        "id": {"type": "integer", "readOnly": true},
        "email": {"type": "string", "format": "email", "example": "new.user@example.com"},
        "name": {"type": "string", "example": "New User"},
        "country": {"type": "string", "maxLength": 2, "example": "US"}}},
      "Order": {"type": "object", "properties": {
        "id": {"type": "integer", "readOnly": true},
        "user_id": {"type": "integer"},
        "status": {"type": "string", "enum": ["paid", "pending", "refunded", "shipped"]},
        "total_cents": {"type": "integer"}}},
      "OrderItemIn": {"type": "object", "properties": {
        "sku": {"type": "string", "example": "SKU-1"},
        "qty": {"type": "integer", "example": 2},
        "price_cents": {"type": "integer", "example": 500}}}
    }
  },
  "paths": {
    "/health": {"get": {"operationId": "health", "security": [], "responses": {"200": {"description": "ok"}}}},
    "/auth/login": {"post": {"operationId": "login", "security": [],
      "requestBody": {"content": {"application/json": {"schema": {"type": "object", "properties": {"email": {"type": "string"}, "password": {"type": "string"}}},
        "example": {"email": "perf@test.dev", "password": "secret"}}}},
      "responses": {"200": {"description": "token + session cookie"}}}},
    "/users": {
      "get": {"operationId": "listUsers", "parameters": [{"$ref": "#/components/parameters/limit"}, {"name": "offset", "in": "query", "schema": {"type": "integer", "minimum": 0}}],
        "responses": {"200": {"description": "users"}}},
      "post": {"operationId": "createUser", "requestBody": {"required": true, "content": {"application/json": {"schema": {"$ref": "#/components/schemas/User"}}}},
        "responses": {"201": {"description": "created"}}}},
    "/users/{id}": {
      "parameters": [{"$ref": "#/components/parameters/id"}],
      "get": {"operationId": "getUser", "responses": {"200": {"description": "user"}}},
      "put": {"operationId": "updateUser", "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/User"}}}},
        "responses": {"200": {"description": "updated"}}},
      "delete": {"operationId": "deleteUser", "responses": {"204": {"description": "deleted"}}}},
    "/users/{id}/orders": {"get": {"operationId": "getUserOrders", "parameters": [{"$ref": "#/components/parameters/id"}, {"$ref": "#/components/parameters/limit"}],
      "responses": {"200": {"description": "orders with items"}}}},
    "/users/{id}/recommendations": {"get": {"operationId": "getRecommendations", "parameters": [{"$ref": "#/components/parameters/id"}, {"$ref": "#/components/parameters/limit"}],
      "responses": {"200": {"description": "recommended orders"}}}},
    "/orders": {
      "get": {"operationId": "listOrders", "parameters": [{"$ref": "#/components/parameters/limit"}], "responses": {"200": {"description": "orders"}}},
      "post": {"operationId": "createOrder", "requestBody": {"required": true, "content": {"application/json": {"schema": {"type": "object", "required": ["user_id", "items"], "properties": {
        "user_id": {"type": "integer"}, "status": {"type": "string", "enum": ["pending", "paid"]},
        "items": {"type": "array", "minItems": 1, "maxItems": 1000, "items": {"$ref": "#/components/schemas/OrderItemIn"}}}}}}},
        "responses": {"201": {"description": "created"}}}},
    "/orders/search": {"get": {"operationId": "searchOrders", "parameters": [{"name": "status", "in": "query", "schema": {"type": "string", "enum": ["paid", "pending", "refunded", "shipped"]}}, {"$ref": "#/components/parameters/limit"}],
      "responses": {"200": {"description": "orders"}}}},
    "/notes": {"post": {"operationId": "createNote", "requestBody": {"required": true, "content": {"application/json": {"schema": {"type": "object", "required": ["user_id", "body"], "properties": {
        "user_id": {"type": "integer"}, "body": {"type": "string", "example": "remember the milk"}}}}}},
      "responses": {"201": {"description": "created", "links": {
        "GetNote": {"operationId": "getNote", "parameters": {"id": "$response.body#/id"}},
        "UpdateNote": {"operationId": "updateNote", "parameters": {"id": "$response.body#/id"}},
        "DeleteNote": {"operationId": "deleteNote", "parameters": {"id": "$response.body#/id"}}}}}}},
    "/events/feed": {"get": {"operationId": "eventFeed", "parameters": [{"name": "cursor", "in": "query", "schema": {"type": "string"}}, {"$ref": "#/components/parameters/limit"}],
      "responses": {"200": {"description": "a page of events and next_cursor"}}}},
    "/sessions": {"post": {"operationId": "createSession", "requestBody": {"required": true, "content": {"application/x-www-form-urlencoded": {"schema": {"type": "object", "required": ["user_id", "token"], "properties": {
        "user_id": {"type": "integer"}, "token": {"type": "string"}}}}}},
      "responses": {"201": {"description": "created"}}}},
    "/users/{id}/avatar": {"post": {"operationId": "uploadAvatar", "parameters": [{"$ref": "#/components/parameters/id"}],
      "requestBody": {"required": true, "content": {"multipart/form-data": {"schema": {"type": "object", "required": ["file"], "properties": {
        "file": {"type": "string", "format": "binary"}, "caption": {"type": "string", "example": "me"}}}}}},
      "responses": {"200": {"description": "stored"}}}},
    "/notes/{id}": {
      "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string", "format": "uuid"}}],
      "get": {"operationId": "getNote", "responses": {"200": {"description": "note"}}},
      "put": {"operationId": "updateNote", "requestBody": {"content": {"application/json": {"schema": {"type": "object", "properties": {"body": {"type": "string", "example": "edited"}}}}}},
        "responses": {"200": {"description": "updated"}}},
      "delete": {"operationId": "deleteNote", "responses": {"204": {"description": "deleted"}}}},
    "/events/search": {"get": {"operationId": "searchEvents", "parameters": [{"name": "kind", "in": "query", "schema": {"type": "string", "enum": ["buy", "click", "view", "share"]}}, {"$ref": "#/components/parameters/limit"}],
      "responses": {"200": {"description": "events"}}}},
    "/users/{id}/events": {"get": {"operationId": "getUserEvents", "parameters": [{"$ref": "#/components/parameters/id"}, {"$ref": "#/components/parameters/limit"}],
      "responses": {"200": {"description": "events"}}}},
    "/users/{id}/totals": {"get": {"operationId": "getUserTotals", "parameters": [{"$ref": "#/components/parameters/id"}],
      "responses": {"200": {"description": "order totals from a view"}}}},
    "/orders/by-country": {"get": {"operationId": "ordersByCountry", "parameters": [{"name": "country", "in": "query", "schema": {"type": "string", "enum": ["US", "IN", "DE", "GB", "FR"]}}, {"$ref": "#/components/parameters/limit"}],
      "responses": {"200": {"description": "orders"}}}},
    "/archive/orders/{id}": {"get": {"operationId": "getArchivedOrder", "parameters": [{"$ref": "#/components/parameters/id"}],
      "responses": {"200": {"description": "archived order"}}}},
    "/reports/sales": {"get": {"operationId": "salesReport", "parameters": [{"name": "from", "in": "query", "schema": {"type": "string", "format": "date"}, "example": "2000-01-01"}, {"name": "to", "in": "query", "schema": {"type": "string", "format": "date"}, "example": "2100-01-01"}],
      "responses": {"200": {"description": "daily sales"}}}}
  }
}`
