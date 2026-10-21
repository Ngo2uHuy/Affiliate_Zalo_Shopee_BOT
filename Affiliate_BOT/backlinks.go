package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

var facebookHosts = map[string]bool{"facebook.com": true, "www.facebook.com": true, "m.facebook.com": true,
	"web.facebook.com": true, "fb.com": true, "www.fb.com": true, "fb.watch": true}

// FacebookPost links a Shopee product to the owner's Facebook review post.
type FacebookPost struct {
	Product string `json:"product"`
	Post    string `json:"post"`
}

// Backlinks reloads its file when the modification time changes, so posts can
// be added while the bot is running.
type Backlinks struct {
	path  string
	page  string
	mu    sync.Mutex
	mod   time.Time
	posts map[string]string // "shop/item" -> post URL
}

func isFacebookURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.User == nil && (u.Port() == "" || u.Port() == "443") &&
		facebookHosts[strings.ToLower(u.Hostname())]
}

func readFacebookPosts(path string) (map[string]string, time.Time, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, time.Time{}, nil
	}
	if err != nil {
		return nil, time.Time{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, time.Time{}, err
	}
	var list []FacebookPost
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, time.Time{}, fmt.Errorf("%s phải là danh sách JSON dạng [{\"product\":...,\"post\":...}]", path)
	}
	posts := make(map[string]string, len(list))
	for i, p := range list {
		shop, item, ok := productID(p.Product)
		if !ok {
			return nil, time.Time{}, fmt.Errorf("%s mục %d: product phải là link sản phẩm Shopee đầy đủ", path, i+1)
		}
		if !isFacebookURL(p.Post) {
			return nil, time.Time{}, fmt.Errorf("%s mục %d: post phải là link https Facebook", path, i+1)
		}
		posts[shop+"/"+item] = p.Post
	}
	return posts, info.ModTime(), nil
}

func NewBacklinks(path, page string) (*Backlinks, error) {
	posts, mod, err := readFacebookPosts(path)
	if err != nil {
		return nil, err
	}
	return &Backlinks{path: path, page: page, mod: mod, posts: posts}, nil
}

// Line returns the reply line pointing to the product's review post, or to the
// Facebook page when the product has none.
func (b *Backlinks) Line(product string) string {
	link, isPost := b.Target(product)
	switch {
	case isPost:
		return "📌 Review sản phẩm trên Facebook: " + link
	case link != "":
		return "📌 Xem thêm review trên Facebook: " + link
	}
	return ""
}

// Target returns the product's review post (isPost true), else the Facebook page, else "".
func (b *Backlinks) Target(product string) (link string, isPost bool) {
	if b == nil {
		return "", false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reload()
	if shop, item, ok := productID(product); ok {
		if post := b.posts[shop+"/"+item]; post != "" {
			return post, true
		}
	}
	return b.page, false
}

// reload keeps the previous posts when an edit leaves the file invalid.
func (b *Backlinks) reload() {
	info, err := os.Stat(b.path)
	var mod time.Time
	if err == nil {
		mod = info.ModTime()
	}
	if mod.Equal(b.mod) {
		return
	}
	// Remember the broken version's time so the error is logged once, not per message.
	b.mod = mod
	posts, _, err := readFacebookPosts(b.path)
	if err != nil {
		log.Printf("Giữ danh sách bài Facebook cũ: %v", err)
		return
	}
	b.posts = posts
}
