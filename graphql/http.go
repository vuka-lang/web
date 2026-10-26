package graphql

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/vuka-lang/web/v2/graphql/language"
)

// The media types of GraphQL over HTTP.
const (
	MediaGraphQLResponse = "application/graphql-response+json"
	MediaJSON            = "application/json"
)

// ServeHTTP answers a GraphQL request over HTTP, as the GraphQL-over-HTTP
// specification has it:
//
//   - GET carries query, operationName, variables and extensions (the last
//     two as JSON) in the URL, and runs queries only: a mutation answers
//     405 with Allow: POST.
//   - POST carries them as a JSON object, Content-Type application/json;
//     another content type answers 415, a body beyond Limits.Bytes plus
//     1 MiB 413, one that isn't such an object 400.
//   - The response is application/graphql-response+json when the Accept
//     header prefers it, application/json when it prefers that, names
//     */*, or is missing; 406 when it accepts neither.
//   - An application/json response is 200 for any well-formed request,
//     its errors in the body. An application/graphql-response+json one is
//     400 when the request was refused before execution (it doesn't parse
//     or validate, its variables or operation name are wrong, a limit) and
//     200 once execution started.
//   - Other methods answer 405.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		writeResponse(w, MediaJSON, http.StatusMethodNotAllowed, requestError(CodeBadRequest, "GraphQL only supports GET and POST requests."))
		return
	}
	media, ok := negotiate(r.Header.Values("Accept"))
	if !ok {
		writeResponse(w, MediaJSON, http.StatusNotAcceptable, requestError(CodeBadRequest, "The Accept header names no media type this server answers with: use application/graphql-response+json or application/json."))
		return
	}
	var req Request
	if r.Method == http.MethodGet {
		q := r.URL.Query()
		req.Query, req.OperationName = q.Get("query"), q.Get("operationName")
		for _, p := range []struct {
			name string
			dst  *map[string]any
		}{{"variables", &req.Variables}, {"extensions", &req.Extensions}} {
			raw := q.Get(p.name)
			if raw == "" {
				continue
			}
			v, err := decodeJSON([]byte(raw))
			if m, ok := v.(map[string]any); err == nil && (ok || v == nil) {
				*p.dst = m
				continue
			}
			writeResponse(w, media, http.StatusBadRequest, requestError(CodeBadRequest, "The %s parameter must be a JSON object.", p.name))
			return
		}
		if req.Query == "" {
			writeResponse(w, media, http.StatusBadRequest, requestError(CodeBadRequest, "Must provide query string."))
			return
		}
		if c := s.document(req.Query); len(c.errs) == 0 {
			if op, bad := operationOf(c.doc, req.OperationName); bad == nil && op.Operation != language.OpQuery {
				w.Header().Set("Allow", "POST")
				writeResponse(w, media, http.StatusMethodNotAllowed, requestError(CodeBadRequest, "Can only perform a %s operation from a POST request.", op.Operation))
				return
			}
		}
	} else {
		ct, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || ct != MediaJSON {
			writeResponse(w, media, http.StatusUnsupportedMediaType, requestError(CodeBadRequest, "A POST request's Content-Type must be application/json."))
			return
		}
		limit := int64(s.limits.Bytes)
		if limit <= 0 {
			limit = 1 << 30
		}
		limit += 1 << 20
		body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
		if err != nil {
			writeResponse(w, media, http.StatusBadRequest, requestError(CodeBadRequest, "The request body couldn't be read."))
			return
		}
		if int64(len(body)) > limit {
			writeResponse(w, media, http.StatusRequestEntityTooLarge, requestError(CodeSizeLimit, "The request body is larger than the limit of %d bytes.", limit))
			return
		}
		if msg := parseBody(body, &req); msg != "" {
			writeResponse(w, media, http.StatusBadRequest, requestError(CodeBadRequest, "%s", msg))
			return
		}
	}
	res := s.Execute(r.Context(), req)
	status := http.StatusOK
	if media == MediaGraphQLResponse && !res.Executed {
		status = http.StatusBadRequest
	}
	writeResponse(w, media, status, res)
}

// parseBody reads a POST body into req; a message saying what is wrong.
func parseBody(body []byte, req *Request) string {
	v, err := decodeJSON(body)
	if err != nil {
		return "The request body is not valid JSON."
	}
	m, ok := v.(map[string]any)
	if !ok {
		return "The request body must be a JSON object."
	}
	q, ok := m["query"].(string)
	if !ok || q == "" {
		if _, there := m["query"]; there && !ok {
			return "The query must be a string."
		}
		return "Must provide query string."
	}
	req.Query = q
	if v, there := m["operationName"]; there && v != nil {
		name, ok := v.(string)
		if !ok {
			return "The operationName must be a string."
		}
		req.OperationName = name
	}
	for _, p := range []struct {
		name string
		dst  *map[string]any
	}{{"variables", &req.Variables}, {"extensions", &req.Extensions}} {
		v, there := m[p.name]
		if !there || v == nil {
			continue
		}
		obj, ok := v.(map[string]any)
		if !ok {
			return "The " + p.name + " must be a JSON object."
		}
		*p.dst = obj
	}
	return ""
}

func decodeJSON(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing data")
	}
	return v, nil
}

// negotiate picks the response's media type from the Accept header(s):
// ok false when it accepts neither.
func negotiate(accept []string) (string, bool) {
	header := strings.Join(accept, ",")
	if strings.TrimSpace(header) == "" {
		return MediaJSON, true
	}
	bestQ, best := -1.0, ""
	for _, part := range strings.Split(header, ",") {
		mt, params, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil {
			continue
		}
		q := 1.0
		if qs, ok := params["q"]; ok {
			if f, err := strconv.ParseFloat(qs, 64); err == nil {
				q = f
			}
		}
		if q <= 0 {
			continue
		}
		var pick string
		switch mt {
		case MediaGraphQLResponse:
			pick = MediaGraphQLResponse
		case MediaJSON, "application/*", "*/*":
			pick = MediaJSON
		default:
			continue
		}
		if q > bestQ || q == bestQ && pick == MediaGraphQLResponse && best != MediaGraphQLResponse && mt == MediaGraphQLResponse {
			bestQ, best = q, pick
		}
	}
	return best, best != ""
}

func writeResponse(w http.ResponseWriter, media string, status int, res *Response) {
	b, err := json.Marshal(res)
	if err != nil {
		status = http.StatusInternalServerError
		b, _ = json.Marshal(requestError(CodeInternal, "the response couldn't be written as JSON"))
	}
	w.Header().Set("Content-Type", media+"; charset=utf-8")
	w.WriteHeader(status)
	w.Write(b)
}
