package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testPost = "https://www.facebook.com/mypage/posts/123"

func writePosts(t *testing.T, path, content string, mod time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func TestReadFacebookPostsValidates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "posts.json")
	cases := map[string]bool{
		`[{"product":"https://shopee.vn/Ao-i.1.2?sp_atk=x","post":"` + testPost + `"}]`: true,
		`[{"product":"https://s.shopee.vn/abc","post":"` + testPost + `"}]`:              false,
		`[{"product":"https://shopee.vn/product/1/2","post":"https://evil.com/x"}]`:      false,
		`[{"product":"https://shopee.vn/product/1/2","post":"http://facebook.com/x"}]`:   false,
		`{"product":"x"}`: false,
	}
	for content, ok := range cases {
		writePosts(t, path, content, time.Now())
		if _, _, err := readFacebookPosts(path); (err == nil) != ok {
			t.Errorf("%s -> err %v; want ok=%v", content, err, ok)
		}
	}
	if posts, _, err := readFacebookPosts(filepath.Join(t.TempDir(), "missing.json")); err != nil || len(posts) != 0 {
		t.Fatalf("missing file: %v %v", posts, err)
	}
}

func TestBacklinkLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "posts.json")
	writePosts(t, path, `[{"product":"https://shopee.vn/product/1/2","post":"`+testPost+`"}]`, time.Now())
	b, err := NewBacklinks(path, "https://www.facebook.com/mypage")
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Line("https://shopee.vn/product/1/2"); !strings.Contains(got, testPost) {
		t.Errorf("matching product: %q", got)
	}
	if got := b.Line("https://shopee.vn/product/1/3"); !strings.Contains(got, "https://www.facebook.com/mypage") {
		t.Errorf("page fallback: %q", got)
	}
	b.page = ""
	if got := b.Line("https://shopee.vn/m/ma-giam-gia"); got != "" {
		t.Errorf("no page configured: %q", got)
	}
	if got := (*Backlinks)(nil).Line("https://shopee.vn/product/1/2"); got != "" {
		t.Errorf("nil backlinks: %q", got)
	}
}

func TestBacklinksReloadKeepsOldOnBrokenEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "posts.json")
	start := time.Now().Add(-time.Hour)
	writePosts(t, path, `[]`, start)
	b, err := NewBacklinks(path, "")
	if err != nil {
		t.Fatal(err)
	}
	writePosts(t, path, `[{"product":"https://shopee.vn/product/1/2","post":"`+testPost+`"}]`, start.Add(time.Minute))
	if got := b.Line("https://shopee.vn/product/1/2"); !strings.Contains(got, testPost) {
		t.Fatalf("new post not loaded: %q", got)
	}
	writePosts(t, path, `[{"product":`, start.Add(2*time.Minute))
	if got := b.Line("https://shopee.vn/product/1/2"); !strings.Contains(got, testPost) {
		t.Fatalf("broken edit dropped posts: %q", got)
	}
}

func TestAnswerAppendsBacklink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "posts.json")
	writePosts(t, path, `[{"product":"https://shopee.vn/product/1/2","post":"`+testPost+`"}]`, time.Now())
	s := NewServer(Config{ShopeeMode: "demo"})
	var err error
	if s.backlinks, err = NewBacklinks(path, ""); err != nil {
		t.Fatal(err)
	}
	got := s.answer(context.Background(), Job{Link: "https://shopee.vn/Ao-i.1.2?utm_source=x"})
	if !strings.HasSuffix(got, "\n📌 Review sản phẩm trên Facebook: "+testPost) {
		t.Fatalf("reply missing backlink: %q", got)
	}
}
