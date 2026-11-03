package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/vuka-lang/ui/v2"
)

// Uploads: a struct form's ui.File field. The runtime sends a chosen file
// ahead of the submit to uploadPath, its body the file's bytes and its
// headers the ticket the field rendered (UploadHeader) and the file's name
// (UploadNameHeader); the answer is the reference the form then carries
// (ui.Uploaded), or why the file was refused ({"error": "max"}). Without
// script the form posts as multipart and formFiles stores its files the
// same way before the submit runs.

const (
	uploadPath = livePrefix + "upload"
	// UploadHeader carries the ticket of the field a file is sent for.
	UploadHeader = "Vk-Upload"
	// UploadNameHeader carries the file's name, URL-encoded.
	UploadNameHeader = "Vk-Upload-Name"
)

// FileStore keeps the files sent to the app's forms (ui.File fields) until
// their forms are submitted, instead of in a directory of this machine: an
// app with several replicas gives them a store they share.
func FileStore(s ui.FileStore) Option {
	return OptionFunc(func(a *App) {
		a.mutable("FileStore")
		a.fileStore = s
	})
}

func (h formHost) FileStore() ui.FileStore { return h.a.fileStore }

type uploadRefusal struct {
	Error string `json:"error"`
}

func (a *App) serveUpload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	name, err := url.QueryUnescape(r.Header.Get(UploadNameHeader))
	if err != nil {
		name = ""
	}
	ctx := ui.WithFormHost(r.Context(), formHost{a})
	up, err := ui.Upload(ctx, r.Header.Get(UploadHeader), name, r.Header.Get("Content-Type"), r.Body)
	if err != nil {
		var ue *ui.UploadError
		if !errors.As(err, &ue) {
			ue = &ui.UploadError{Rule: "failed", Status: http.StatusInternalServerError}
		}
		if ue.Status >= 500 {
			slog.ErrorContext(r.Context(), "storing an upload", "err", err)
		}
		writeJSON(w, ue.Status, uploadRefusal{ue.Rule})
		return
	}
	writeJSON(w, http.StatusOK, up)
}

// formFiles stores the files a form posted without script (multipart, a
// ui.File field's file input) as the runtime's uploads would have: each
// sets its field's reference, or the reason it was refused, in vals.
func formFiles(ctx context.Context, r *http.Request, vals url.Values) {
	if r.MultipartForm == nil {
		return
	}
	for name, files := range r.MultipartForm.File {
		path, ok := strings.CutPrefix(name, ui.FileField+".")
		if !ok || len(files) == 0 {
			continue
		}
		fh := files[len(files)-1]
		if fh.Filename == "" && fh.Size == 0 {
			continue // no file chosen: the field keeps the one it holds
		}
		f, err := fh.Open()
		if err != nil {
			vals.Set(ui.FileErrorField+"."+path, "failed")
			continue
		}
		up, err := ui.Upload(ctx, vals.Get(ui.FileTicketField+"."+path), fh.Filename, fh.Header.Get("Content-Type"), f)
		f.Close()
		var ue *ui.UploadError
		switch {
		case errors.As(err, &ue):
			vals.Set(ui.FileErrorField+"."+path, ue.Rule)
		case err != nil:
			vals.Set(ui.FileErrorField+"."+path, "failed")
		default:
			vals.Set(path, up.Ref)
		}
	}
}
