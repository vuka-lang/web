package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vuka-lang/ui/v2"
)

type avatarForm struct {
	ui.FormState
	Name   string  `validate:"required"`
	Avatar ui.File `accept:"image/*" max:"1KB" validate:"required"`
}

// avatarGot is what the last Save read: the file's name, its size, and
// the form's Name.
var avatarGot atomic.Value

func saveAvatar(f avatarForm) error {
	r, err := f.Avatar.Open()
	if err != nil {
		return err
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	avatarGot.Store(fmt.Sprintf("%s:%d:%s", f.Avatar.Name(), len(b), f.Name))
	return Redirect("/done")
}

func avatarDoc(form ui.Node) ui.Node {
	return ui.El("html", nil, ui.El("head", nil, ui.El("title", nil, ui.Text("Avatar"))),
		ui.El("body", nil, ui.StyleTag(), ui.El("main", nil, form)))
}

// avatarPage is the form on a page that isn't live: checked and submitted
// over HTTP.
type avatarPage struct{ Form avatarForm }

func (p *avatarPage) Save(f avatarForm) error { return saveAvatar(f) }

func (p *avatarPage) Render() ui.Node {
	return avatarDoc(ui.Form{For: ui.For(&p.Form), OnSubmit: p.Save})
}

// liveAvatarPage is the form on a live page.
type liveAvatarPage struct {
	ui.Live
	Form avatarForm
}

func (p *liveAvatarPage) Save(f avatarForm) error { return saveAvatar(f) }

func (p *liveAvatarPage) Render() ui.Node {
	return ui.El("main", nil, ui.StyleTag(), ui.Form{For: ui.For(&p.Form), OnSubmit: p.Save})
}

func avatarApp(t *testing.T) *App {
	a := New(Only("none"), FileStore(ui.DirFileStore(t.TempDir())))
	a.Get("/avatar", func() ui.Node { return (&avatarPage{}).Render() })
	a.Live("/live-avatar", func() *liveAvatarPage { return &liveAvatarPage{} })
	a.Get("/done", func() ui.Node { return ui.El("p", []ui.Attr{{Name: "id", Value: "done"}}, ui.Text("done")) })
	return a
}

var (
	pngBytes    = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{1}, 100)...)
	avatarInput = regexp.MustCompile(`<input type="hidden" name="(_vk_[\w.]+|avatar)" value="([^"]*)"`)
)

// avatarForm is the hidden fields of the avatar page's form, and the
// cookie its GET set.
func avatarFields(t *testing.T, h http.Handler, path string) (url.Values, string) {
	t.Helper()
	code, body, hdr := do(t, h, "GET", path, "")
	if code != 200 {
		t.Fatalf("GET %s: %d", path, code)
	}
	v := url.Values{}
	for _, m := range avatarInput.FindAllStringSubmatch(body, -1) {
		v.Set(m[1], html.UnescapeString(m[2]))
	}
	if v.Get("_vk_upload.avatar") == "" {
		t.Fatalf("no upload ticket in\n%s", body)
	}
	cookie := ""
	for _, c := range (&http.Response{Header: hdr}).Cookies() {
		cookie = c.Name + "=" + c.Value
	}
	return v, cookie
}

func sendUpload(t *testing.T, h http.Handler, ticket, name, ctype string, body []byte, header ...string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("POST", uploadPath, bytes.NewReader(body))
	req.Header.Set(UploadHeader, ticket)
	req.Header.Set(UploadNameHeader, url.QueryEscape(name))
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// TestUploadRoute: a file sent ahead of its form's submit is stored and
// answered with its reference; one too large, of a type the field doesn't
// take, or sent with a forged ticket, or from another site, is refused.
func TestUploadRoute(t *testing.T) {
	h := avatarApp(t).Handler()
	v, _ := avatarFields(t, h, "/avatar")
	ticket := v.Get("_vk_upload.avatar")
	code, out := sendUpload(t, h, ticket, "my photo.png", "image/png", pngBytes)
	if code != 200 || out["ref"] == "" || out["name"] != "my photo.png" || out["size"] != float64(len(pngBytes)) {
		t.Errorf("an upload: %d %v", code, out)
	}
	for _, c := range []struct {
		what   string
		ticket string
		ctype  string
		body   []byte
		code   int
		err    string
	}{
		{"too large", ticket, "image/png", append(pngBytes, make([]byte, 2000)...), 413, "max"},
		{"not an image", ticket, "text/plain", []byte("hello"), 415, "accept"},
		{"a forged ticket", "x.y", "image/png", pngBytes, 400, "failed"},
	} {
		if code, out := sendUpload(t, h, c.ticket, "a.png", c.ctype, c.body); code != c.code || out["error"] != c.err {
			t.Errorf("%s: %d %v", c.what, code, out)
		}
	}
	if code, _ := sendUpload(t, h, ticket, "a.png", "image/png", pngBytes, "Sec-Fetch-Site", "cross-site", "Origin", "https://evil.test"); code != http.StatusForbidden {
		t.Errorf("an upload from another site: %d", code)
	}
}

// TestUploadPlainPost: without script the form posts as multipart; the
// first post stores the file, a failed one renders it kept by reference
// ("photo.png ✓ Change"), and the next post sends the reference only.
func TestUploadPlainPost(t *testing.T) {
	avatarGot.Store("")
	h := avatarApp(t).Handler()
	v, cookie := avatarFields(t, h, "/avatar")
	multi := func(fields url.Values, file string, body []byte) (int, string, http.Header) {
		var b bytes.Buffer
		w := multipart.NewWriter(&b)
		for k, vs := range fields {
			for _, x := range vs {
				w.WriteField(k, x)
			}
		}
		fw, _ := w.CreateFormFile("_vk_file.avatar", file)
		fw.Write(body)
		w.Close()
		return do(t, h, "POST", "/avatar", b.String(), "Content-Type", w.FormDataContentType(), "Cookie", cookie,
			"Accept", "text/html", "Sec-Fetch-Dest", "document", "Sec-Fetch-Site", "same-origin")
	}
	code, page, _ := multi(v, "photo.png", pngBytes)
	if code != 422 || !strings.Contains(page, `<span class="vk-file__name">photo.png</span>`) || !strings.Contains(page, ">Change<") || !strings.Contains(page, "Enter name") {
		t.Fatalf("a first post missing a name: %d\n%s", code, page)
	}
	ref := ""
	for _, m := range avatarInput.FindAllStringSubmatch(page, -1) {
		if m[1] == "avatar" {
			ref = html.UnescapeString(m[2])
		}
	}
	if ref == "" {
		t.Fatalf("the failed post doesn't keep the file:\n%s", page)
	}
	v.Set("avatar", ref)
	v.Set("name", "Pa")
	code, _, hdr := multi(v, "", nil) // no file chosen again: the reference stands
	if code != http.StatusSeeOther || hdr.Get("Location") != "/done" || avatarGot.Load() != fmt.Sprintf("photo.png:%d:Pa", len(pngBytes)) {
		t.Fatalf("the second post: %d %q %v", code, hdr.Get("Location"), avatarGot.Load())
	}
	v, cookie = avatarFields(t, h, "/avatar")
	v.Set("name", "Pa")
	if code, page, _ := multi(v, "big.png", append(pngBytes, make([]byte, 2000)...)); code != 422 || !strings.Contains(page, "Avatar must be smaller than 1 KB") {
		t.Errorf("a file over max posted plainly: %d\n%s", code, page)
	}
}
