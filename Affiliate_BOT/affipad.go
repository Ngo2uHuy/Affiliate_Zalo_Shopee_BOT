package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

func isAffipadURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && len(raw) <= maxLinkLength && u.Scheme == "https" && u.Host == "s.afp.ad" && u.User == nil && u.Fragment == "" && u.Path != "" && u.Path != "/"
}

func isProviderShortURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > maxLinkLength || u.Scheme != "https" || u.User != nil || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") || u.Path == "" || u.Path == "/" {
		return false
	}
	return u.Path != "/an_redir" && (shortHosts[u.Hostname()] || u.Hostname() == "shp.ee" || u.Hostname() == "s.afp.ad")
}

func (s *Server) convertAffipad(ctx context.Context, origin, subID string) (string, error) {
	if !isShopeeURL(origin) {
		return "", errors.New("invalid Shopee input")
	}
	body, err := json.Marshal(map[string]any{
		"url": origin, "toolId": s.cfg.AffipadToolID, "useCache": true,
		"useShortLink": true, "resolveRedirect": false, "subId": subID,
		"tracking": map[string]string{"preset": "fb_reels"},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.affipad.com/v1/convert", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.AffipadAPIKey)
	req.Header.Set("Content-Type", "application/json")
	client := *s.http
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return "", errors.New("AffiPad request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("AffiPad HTTP %d", resp.StatusCode)
	}
	var result struct {
		Success bool `json:"success"`
		Data    struct {
			Results []struct {
				Link     string `json:"link"`
				ShortURL string `json:"shortUrl"`
			} `json:"results"`
		} `json:"data"`
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return "", errors.New("AffiPad response unreadable or too large")
	}
	if json.Unmarshal(data, &result) != nil || !result.Success || len(result.Data.Results) == 0 {
		return "", errors.New("AffiPad returned no conversion")
	}
	for _, item := range result.Data.Results {
		if isProviderShortURL(item.ShortURL) {
			return item.ShortURL, nil
		}
	}
	for _, item := range result.Data.Results {
		if isAffipadURL(item.Link) {
			return item.Link, nil
		}
	}
	for _, item := range result.Data.Results {
		if isShopeeURL(item.Link) {
			return item.Link, nil
		}
	}
	return "", errors.New("AffiPad returned no supported HTTPS affiliate link")
}
