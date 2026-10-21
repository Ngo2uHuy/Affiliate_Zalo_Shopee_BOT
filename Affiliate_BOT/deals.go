package main

// Facebook deal post: every converted link is also commented on the owner's
// Page post. Customers open the product from that comment, so the click really
// comes from Facebook and Shopee's Facebook/Instagram-only vouchers can apply.

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
)

const (
	// Unversioned calls use the oldest Graph API version the app may call,
	// so the bot does not break when a pinned version expires.
	graphAPIBase   = "https://graph.facebook.com"
	dealNotesFile  = "deal_vouchers.txt"
	maxDealNoteLen = 4000
)

var postIDFormat = regexp.MustCompile(`^[0-9]+_[0-9]+$`)

// commentDeal posts link as a new comment on the deal post with the Page token.
func (s *Server) commentDeal(ctx context.Context, link string) error {
	form := url.Values{"message": {link}, "access_token": {s.cfg.PageToken}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.graphBase+"/"+s.cfg.DealPostID+"/comments",
		strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	_, status, err := s.do(req)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("Facebook HTTP %d", status)
	}
	return nil
}

// dealLines points the customer to the Facebook deal post, plus the owner's
// voucher notes read fresh from deal_vouchers.txt so they can be edited live.
func (s *Server) dealLines(ctx context.Context, link string) string {
	if s.cfg.DealPostURL == "" {
		return ""
	}
	line := "► Link mã Facebook/Instagram: vào bài này, bấm link sản phẩm trong bình luận:\n" + s.cfg.DealPostURL
	if s.cfg.DealPostID != "" {
		if err := s.commentDeal(ctx, link); err != nil {
			log.Printf("Chưa comment được link lên bài Facebook: %v", err)
		} else {
			line = "► Link mã Facebook/Instagram (cmt mới nhất):\n" + s.cfg.DealPostURL
		}
	}
	if notes, err := os.ReadFile(s.dealNotes); err == nil {
		if text := strings.TrimSpace(string(notes)); text != "" && len(text) <= maxDealNoteLen {
			line += "\n\n" + text
		}
	}
	return line
}
