package web

import (
	"net/http"
	"strings"
	"testing"
	"testing/fstest"
)

// TestStaticAssetsAreRevalidated pins the header that keeps a browser from
// running last release's JavaScript against this release's markup.
//
// The files are not content-addressed — the templates name /static/app.js, not
// /static/app.<hash>.js — so the only safe thing to tell a browser is "ask me
// first". This is worth a test because getting it wrong fails silently and
// intermittently: one person sees the new page, the next sees the old one, and
// nothing appears in any log.
func TestStaticAssetsAreRevalidated(t *testing.T) {
	h := newHarness(t)

	resp := h.get("/static/app.js")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /static/app.js = %d, want 200", resp.StatusCode)
	}

	cache := resp.Header.Get("Cache-Control")
	if strings.Contains(cache, "max-age") {
		t.Errorf("Cache-Control = %q, want no max-age: these files are not content-addressed", cache)
	}
	if !strings.Contains(cache, "no-cache") {
		t.Errorf("Cache-Control = %q, want no-cache so the browser revalidates", cache)
	}

	etag := resp.Header.Get("ETag")
	if etag == "" {
		// Without one, no-cache means the whole file comes down every time.
		t.Fatal("no ETag: revalidation would cost a full download")
	}

	req, err := http.NewRequest(http.MethodGet, h.ts.URL+"/static/app.js", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("If-None-Match", etag)

	again, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("revalidate: %v", err)
	}
	if again.StatusCode != http.StatusNotModified {
		again.Body.Close()
		t.Fatalf("revalidation = %d, want 304", again.StatusCode)
	}
	if body := h.body(again); body != "" {
		t.Errorf("304 carried a body of %d bytes, want none", len(body))
	}
}

// TestStaticETagTracksContent is the property that makes the 304 mean
// something: the tag is a function of the bytes and nothing else. A tag that
// also depended on the filename, or on a counter, would still revalidate — and
// would still serve last release's stylesheet after an upgrade.
func TestStaticETagTracksContent(t *testing.T) {
	tags, err := staticETags(fstest.MapFS{
		"a.css": &fstest.MapFile{Data: []byte("body { margin: 0 }")},
		"b.css": &fstest.MapFile{Data: []byte("body { margin: 0 }")},
		"c.css": &fstest.MapFile{Data: []byte("body { margin: 1px }")},
	})
	if err != nil {
		t.Fatalf("staticETags: %v", err)
	}
	if len(tags) != 3 {
		t.Fatalf("hashed %d files, want 3: %v", len(tags), tags)
	}

	if tags["/a.css"] != tags["/b.css"] {
		t.Errorf("identical bytes got different tags: %q and %q", tags["/a.css"], tags["/b.css"])
	}
	if tags["/a.css"] == tags["/c.css"] {
		t.Errorf("different bytes got the same tag %q", tags["/a.css"])
	}
	for name, tag := range tags {
		if !strings.HasPrefix(tag, `"`) || !strings.HasSuffix(tag, `"`) {
			t.Errorf("%s: tag %q is not a quoted entity tag", name, tag)
		}
	}
}

// An unknown name under /static/ is not an asset and must not be handed an ETag
// from the table; FileServer still owns the 404.
func TestUnknownStaticPathIsNotFound(t *testing.T) {
	h := newHarness(t)

	resp := h.get("/static/no-such-file.css")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /static/no-such-file.css = %d, want 404", resp.StatusCode)
	}
	if etag := resp.Header.Get("ETag"); etag != "" {
		t.Errorf("ETag = %q on a file that does not exist", etag)
	}
}
