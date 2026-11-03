package web

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vuka-lang/web/v2/internal/chrome"
)

// chooseFile picks a file in the avatar field's input, as the visitor
// choosing one does: n bytes, a PNG's first ones.
func chooseFile(name string, n int) string {
	return fmt.Sprintf(`(() => {
		const bytes = new Uint8Array(%d); bytes.set([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]);
		const dt = new DataTransfer(); dt.items.add(new File([bytes], %q, { type: "image/png" }));
		const input = document.querySelector('input[name="_vk_file.avatar"]');
		input.files = dt.files;
		input.dispatchEvent(new Event("change", { bubbles: true }));
	})()`, n, name)
}

// TestBrowserUpload: on a live page and on one that isn't, a chosen file
// is sent at once and shown by name; one too large shows its error; a
// submit sent while a file is still being sent waits for it, and Save
// reads the file.
func TestBrowserUpload(t *testing.T) {
	srv := httptest.NewServer(avatarApp(t).Handler())
	defer srv.Close()
	b := chrome.Launch(t)
	for _, path := range []string{"/avatar", "/live-avatar"} {
		t.Run(strings.TrimPrefix(path, "/"), func(t *testing.T) {
			avatarGot.Store("")
			p := b.Visit(srv.URL + path)
			if path == "/live-avatar" {
				p.Wait(`document.documentElement.dataset.vkState === "connected"`)
			}
			p.Eval(chooseFile("big.png", 4000), nil)
			p.Wait(`(document.querySelector("#avatar-form-avatar-error") || {}).textContent?.includes("smaller than 1 KB")`)
			var got any
			p.Eval(`[document.querySelector('input[name="avatar"]').value, document.querySelector(".vk-file__held").hidden, document.querySelector(".vk-file__progress").hidden]`, &got)
			if g, _ := got.([]any); len(g) != 3 || g[0] != "" || g[1] != true || g[2] != true {
				t.Errorf("after a file too large: %v", got)
			}
			p.Eval(chooseFile("photo.png", 300), nil)
			p.Wait(`!document.querySelector(".vk-file__held").hidden && document.querySelector(".vk-file__name").textContent === "photo.png"`)
			p.Wait(`document.querySelector("#avatar-form-avatar-error") === null`)
			p.Eval(`[document.querySelector('input[name="avatar"]').value !== "", document.querySelector('input[name="_vk_file.avatar"]').value,
				document.querySelector('input[name="_vk_file.avatar"]').classList.contains("vk-file__input--held"), document.querySelector(".vk-file__change").textContent]`, &got)
			if g, _ := got.([]any); len(g) != 4 || g[0] != true || g[1] != "" || g[2] != true || !strings.HasPrefix(g[3].(string), "Change") {
				t.Errorf("after an upload: %v", got)
			}
			// a new file, the form submitted while it is still being sent
			p.Type(`input[name="name"]`, "Pa")
			p.Eval(chooseFile("second.png", 500)+`; document.querySelector("form").requestSubmit()`, nil)
			p.Wait(`location.pathname === "/done"`)
			if got := avatarGot.Load(); got != "second.png:500:Pa" {
				t.Errorf("Save read %v", got)
			}
		})
	}
}
