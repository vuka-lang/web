package graphql

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type httpCase struct {
	name, method, url, ctype, accept, body string
	status                                 int
	media                                  string // the response's Content-Type, without charset
	contains                               string
	allow                                  string
}

func TestHTTP(t *testing.T) {
	srv, err := NewServer(Config{Schema: newTestSchema(t).Schema, Limits: Limits{Bytes: 60}})
	if err != nil {
		t.Fatal(err)
	}
	q := func(s string) string { return "/graphql?query=" + url.QueryEscape(s) }
	for _, c := range []httpCase{
		{name: "GET query", method: "GET", url: q(`{ me { name } }`), status: 200, media: MediaJSON, contains: `{"data":{"me":{"name":"Alice"}}}`},
		{name: "GET variables and operation", method: "GET", url: q(`query A($m: Mood!) { mood(m: $m) } query B { me { id } }`) + "&operationName=A&variables=" + url.QueryEscape(`{"m":"SAD"}`), status: 200, media: MediaJSON, contains: `{"data":{"mood":"SAD"}}`},
		{name: "GET bad variables", method: "GET", url: q(`{ me { id } }`) + "&variables=[1]", status: 400, media: MediaJSON, contains: `The variables parameter must be a JSON object.`},
		{name: "GET without query", method: "GET", url: "/graphql", status: 400, media: MediaJSON, contains: `Must provide query string.`},
		{name: "GET mutation", method: "GET", url: q(`mutation { second }`), status: 405, allow: "POST", media: MediaJSON, contains: `Can only perform a mutation operation from a POST request.`},
		{name: "GET mutation by name", method: "GET", url: q(`query A { me { id } } mutation B { second }`) + "&operationName=B", status: 405, allow: "POST", media: MediaJSON},
		{name: "POST query", method: "POST", url: "/graphql", ctype: "application/json", body: `{"query":"{ me { name } }"}`, status: 200, media: MediaJSON, contains: `{"data":{"me":{"name":"Alice"}}}`},
		{name: "POST charset", method: "POST", url: "/graphql", ctype: "application/json; charset=utf-8", body: `{"query":"mutation { second }","variables":null,"operationName":null}`, status: 200, media: MediaJSON, contains: `"data":{"second":`},
		{name: "POST form", method: "POST", url: "/graphql", ctype: "application/x-www-form-urlencoded", body: `query=%7B+me+%7B+name+%7D+%7D`, status: 415, media: MediaJSON},
		{name: "POST no content type", method: "POST", url: "/graphql", body: `{"query":"{ me { name } }"}`, status: 415, media: MediaJSON},
		{name: "POST malformed", method: "POST", url: "/graphql", ctype: "application/json", body: `{"query":`, status: 400, media: MediaJSON, contains: `The request body is not valid JSON.`},
		{name: "POST not an object", method: "POST", url: "/graphql", ctype: "application/json", body: `["x"]`, status: 400, media: MediaJSON, contains: `The request body must be a JSON object.`},
		{name: "POST query not a string", method: "POST", url: "/graphql", ctype: "application/json", body: `{"query":1}`, status: 400, media: MediaJSON, contains: `The query must be a string.`},
		{name: "POST variables not an object", method: "POST", url: "/graphql", ctype: "application/json", body: `{"query":"{ me { id } }","variables":"x"}`, status: 400, media: MediaJSON},
		{name: "POST too large", method: "POST", url: "/graphql", ctype: "application/json", body: `{"query":"{ me { id } }","variables":{"x":"` + strings.Repeat("x", 2<<20) + `"}}`, status: 413, media: MediaJSON},
		{name: "PUT", method: "PUT", url: "/graphql", status: 405, allow: "GET, POST", media: MediaJSON},
		// application/json: 200 for a well-formed request, errors and all.
		{name: "json parse error", method: "POST", url: "/graphql", ctype: "application/json", body: `{"query":"{ me "}`, status: 200, media: MediaJSON, contains: `GRAPHQL_PARSE_FAILED`},
		{name: "json validation error", method: "POST", url: "/graphql", ctype: "application/json", accept: "application/json", body: `{"query":"{ nope }"}`, status: 200, media: MediaJSON, contains: `GRAPHQL_VALIDATION_FAILED`},
		{name: "json field error", method: "POST", url: "/graphql", ctype: "application/json", body: `{"query":"{ nonNullFail }"}`, status: 200, media: MediaJSON, contains: `"data":null`},
		// application/graphql-response+json: 400 before execution, 200 after.
		{name: "gql response", method: "POST", url: "/graphql", ctype: "application/json", accept: MediaGraphQLResponse, body: `{"query":"{ me { id } }"}`, status: 200, media: MediaGraphQLResponse},
		{name: "gql parse error", method: "POST", url: "/graphql", ctype: "application/json", accept: MediaGraphQLResponse, body: `{"query":"{ me "}`, status: 400, media: MediaGraphQLResponse},
		{name: "gql validation error", method: "POST", url: "/graphql", ctype: "application/json", accept: MediaGraphQLResponse + ", application/json;q=0.9", body: `{"query":"{ nope }"}`, status: 400, media: MediaGraphQLResponse},
		{name: "gql variable error", method: "POST", url: "/graphql", ctype: "application/json", accept: MediaGraphQLResponse, body: `{"query":"query ($m: Mood!) { mood(m: $m) }","variables":{"m":"NO"}}`, status: 400, media: MediaGraphQLResponse, contains: `BAD_USER_INPUT`},
		{name: "gql limit", method: "POST", url: "/graphql", ctype: "application/json", accept: MediaGraphQLResponse, body: `{"query":"{ me { id name nick best { id name nick best { id name nick best { id name nick } } } } }"}`, status: 400, media: MediaGraphQLResponse, contains: `SIZE_LIMIT`},
		{name: "gql field error", method: "POST", url: "/graphql", ctype: "application/json", accept: MediaGraphQLResponse, body: `{"query":"{ nonNullFail }"}`, status: 200, media: MediaGraphQLResponse, contains: `"data":null`},
		{name: "gql unknown operation", method: "POST", url: "/graphql", ctype: "application/json", accept: MediaGraphQLResponse, body: `{"query":"query A { me { id } }","operationName":"B"}`, status: 400, media: MediaGraphQLResponse},
		// Negotiation.
		{name: "accept wildcard", method: "GET", url: q(`{ me { id } }`), accept: "*/*", status: 200, media: MediaJSON},
		{name: "accept json preferred", method: "GET", url: q(`{ me { id } }`), accept: MediaGraphQLResponse + ";q=0.5, application/json", status: 200, media: MediaJSON},
		{name: "accept gql preferred", method: "GET", url: q(`{ me { id } }`), accept: "application/json;q=0.5, " + MediaGraphQLResponse, status: 200, media: MediaGraphQLResponse},
		{name: "accept html only", method: "GET", url: q(`{ me { id } }`), accept: "text/html", status: 406, media: MediaJSON},
		{name: "accept refused json", method: "GET", url: q(`{ me { id } }`), accept: "application/json;q=0", status: 406, media: MediaJSON},
	} {
		t.Run(c.name, func(t *testing.T) {
			var body io.Reader
			if c.body != "" {
				body = strings.NewReader(c.body)
			}
			r := httptest.NewRequest(c.method, c.url, body)
			if c.ctype != "" {
				r.Header.Set("Content-Type", c.ctype)
			}
			if c.accept != "" {
				r.Header.Set("Accept", c.accept)
			}
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, r)
			if w.Code != c.status {
				t.Errorf("status %d, want %d: %s", w.Code, c.status, w.Body)
			}
			if got := w.Header().Get("Content-Type"); got != c.media+"; charset=utf-8" {
				t.Errorf("content type %q", got)
			}
			if c.allow != "" && w.Header().Get("Allow") != c.allow {
				t.Errorf("Allow %q", w.Header().Get("Allow"))
			}
			if !strings.Contains(w.Body.String(), c.contains) {
				t.Errorf("body %s lacks %s", w.Body, c.contains)
			}
		})
	}
}

func TestHTTPWithContext(t *testing.T) {
	srv, _ := NewServer(Config{Schema: newTestSchema(t).Schema})
	ts := httptest.NewServer(srv)
	defer ts.Close()
	res, err := http.Post(ts.URL, "application/json", strings.NewReader(`{"query":"{ me { name friends { name } } }"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if string(b) != `{"data":{"me":{"name":"Alice","friends":[{"name":"Bob"}]}}}` {
		t.Errorf("got %s", b)
	}
}
