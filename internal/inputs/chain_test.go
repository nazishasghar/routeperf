package inputs

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/nazishasghar/routeperf/internal/spec"
)

func TestEvalExpr(t *testing.T) {
	var body any
	_ = json.Unmarshal([]byte(`{"id": 42, "data": {"items": [{"uuid": "a-b"}]}}`), &body)
	req := &Request{Method: "POST", PathParams: map[string]string{"org": "acme"}, Query: url.Values{"q": {"x"}}, Body: map[string]any{"name": "n1"}}
	hdr := http.Header{"Location": {"/users/42"}}
	cases := map[string]any{
		"$response.body#/id":                 int64(42),
		"$response.body#/data/items/0/uuid":  "a-b",
		"$response.header.Location":          "/users/42",
		"$request.path.org":                  "acme",
		"$request.query.q":                   "x",
		"$request.body#/name":                "n1",
		"$statusCode":                        201,
		"prefix-{$response.body#/id}-suffix": "prefix-42-suffix",
	}
	for expr, want := range cases {
		got, ok := evalExpr(expr, req, 201, body, hdr)
		if !ok || got != want {
			t.Errorf("%s = %v (%v), want %v", expr, got, ok, want)
		}
	}
	if _, ok := evalExpr("$response.body#/missing", req, 201, body, hdr); ok {
		t.Error("missing pointer must not resolve")
	}
}

func TestObserveLinksAndCursor(t *testing.T) {
	r := New(nil, nil, nil, nil)
	post := &spec.Operation{ID: "createNote", Links: []spec.Link{{Status: "201", Target: "deleteNote", Params: map[string]any{"id": "$response.body#/id"}}}}
	r.Observe(post, &Request{}, 201, []byte(`{"id":"n-1"}`), http.Header{})
	r.Observe(post, &Request{}, 500, []byte(`{"id":"n-2"}`), http.Header{})
	if v := r.Linked("deleteNote", "id"); len(v) != 1 || v[0] != "n-1" {
		t.Fatalf("linked = %v", v)
	}
	feed := &spec.Operation{ID: "feed", Params: []spec.Param{{Name: "cursor", In: "query"}}}
	r.Observe(feed, &Request{}, 200, []byte(`{"items":[1],"next_cursor":"c2"}`), http.Header{})
	r.Observe(feed, &Request{Page: 1}, 200, []byte(`{"items":[1],"meta":{"next_cursor":"c3"}}`), http.Header{})
	if r.cursor["feed"] != "c3" || r.Pages(feed) != 3 {
		t.Errorf("cursor %q pages %d", r.cursor["feed"], r.Pages(feed))
	}
	r.Observe(feed, &Request{Page: 2}, 200, []byte(`{"items":[],"next_cursor":null}`), http.Header{})
	if r.cursor["feed"] != "" {
		t.Errorf("the last page must restart the walk")
	}
	link := http.Header{"Link": {`<https://api.test/items?after=xyz&limit=20>; rel="next"`}}
	if c := nextCursor(nil, link, "after"); c != "xyz" {
		t.Errorf("Link header cursor = %q", c)
	}
}

func TestEncode(t *testing.T) {
	b, ct, err := Encode(&Request{ContentType: "application/x-www-form-urlencoded", Body: map[string]any{"user_id": int64(5), "tags": []any{"a", "b"}, "addr": map[string]any{"city": "X"}}})
	if err != nil || ct != "application/x-www-form-urlencoded" {
		t.Fatal(err, ct)
	}
	v, _ := url.ParseQuery(string(b))
	if v.Get("user_id") != "5" || len(v["tags"]) != 2 || v.Get("addr[city]") != "X" {
		t.Errorf("form = %s", b)
	}
	b, ct, err = Encode(&Request{ContentType: "multipart/form-data", Body: map[string]any{"file": []byte("hello"), "caption": "me"}})
	if err != nil || !strings.HasPrefix(ct, "multipart/form-data; boundary=") || !strings.Contains(string(b), `filename="file.txt"`) || !strings.Contains(string(b), "hello") {
		t.Errorf("multipart: %s %s %v", ct, b, err)
	}
}
