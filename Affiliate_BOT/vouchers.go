package main

// Lists public Shopee vouchers, ported from legacy/shopee_vouchers.py. Uses
// website endpoints without cookies; they are not a supported partner API and
// do not prove a voucher fits a product or account.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	voucherDetailAPI = "https://shopee.vn/api/v2/voucher_wallet/get_voucher_detail"
	vouchersPageURL  = "https://shopee.vn/m/ma-giam-gia"
	voucherPageChars = 1700
)

var (
	voucherSourcePages = []string{"https://shopee.vn/m/ma-giam-gia", "https://shopee.vn/m/kol-affiliate"}
	voucherCodeFormat  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)
	signatureFormat    = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)
	voucherLinkFinder  = regexp.MustCompile(`https://shopee\.vn/voucher/details\?[^\s<>"']+`)
	expiryFinder       = regexp.MustCompile(`(?i)HSD\s*:?\s*(\d{1,2}[/-]\d{1,2}[/-]\d{4}\s+\d{1,2}:\d{2})`)
	voucherCommand     = regexp.MustCompile(`(?:^|[^\p{L}\p{N}_])(?:/?voucher|/?magiamgia|ma giam gia)(?:\s+(\d{1,6}))?\s*$`)
	vietnamTime        = time.FixedZone("ICT", 7*3600)
	voucherPercent     = regexp.MustCompile(`(?i)giảm\s+(\d+(?:[.,]\d+)?)\s*%(?:\s+tối đa\s+([\d.,]+\s*(?:k|triệu|tr|vnd|đ|₫)))?`)
	voucherFixed       = regexp.MustCompile(`(?i)giảm\s+([\d.,]+\s*(?:k|triệu|tr|vnd|đ|₫))`)
	voucherMinSpend    = regexp.MustCompile(`(?i)đơn[^.]*?\stừ\s+([\d.,]+\s*(?:k|triệu|tr|vnd|đ|₫))`)
	vietnameseFold     = buildFold(map[rune]string{'a': "àáạảãâầấậẩẫăằắặẳẵ", 'e': "èéẹẻẽêềếệểễ", 'i': "ìíịỉĩ",
		'o': "òóọỏõôồốộổỗơờớợởỡ", 'u': "ùúụủũưừứựửữ", 'y': "ỳýỵỷỹ", 'd': "đ"})
)

type VoucherSource struct {
	PromotionID int64  `json:"promotionid"`
	Code        string `json:"voucher_code"`
	Signature   string `json:"signature"`
}

type Voucher struct {
	Code        string
	Description string
	URL         string
	Expires     time.Time
}

type VoucherList struct {
	sources  []VoucherSource
	fetch    func(context.Context, VoucherSource) (*Voucher, error)
	discover func(context.Context) []VoucherSource

	mu          sync.Mutex
	cached      []Voucher
	nextRefresh time.Time
}

func NewVoucherList(sources []VoucherSource, client *http.Client) *VoucherList {
	return &VoucherList{
		sources:  sources,
		fetch:    func(ctx context.Context, src VoucherSource) (*Voucher, error) { return fetchVoucher(ctx, client, src) },
		discover: func(ctx context.Context) []VoucherSource { return discoverVouchers(ctx, client) },
	}
}

func buildFold(groups map[rune]string) map[rune]rune {
	fold := map[rune]rune{}
	for base, letters := range groups {
		for _, r := range letters {
			fold[r] = base
		}
	}
	return fold
}

// voucherPage returns the requested list page, or 0 when text is not a voucher command.
func voucherPage(text string) int {
	var b strings.Builder
	for _, r := range strings.ToLower(text) {
		switch {
		case r >= 0x300 && r <= 0x36f: // combining accents from NFD input
		case vietnameseFold[r] != 0:
			b.WriteRune(vietnameseFold[r])
		default:
			b.WriteRune(r)
		}
	}
	m := voucherCommand.FindStringSubmatch(b.String())
	if m == nil {
		return 0
	}
	page, _ := strconv.Atoi(m[1])
	return max(page, 1)
}

func readVoucherSources(path string) ([]VoucherSource, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var sources []VoucherSource
	if err := json.Unmarshal(data, &sources); err != nil {
		return nil, fmt.Errorf("%s phải là danh sách voucher JSON", path)
	}
	for _, src := range sources {
		if !src.valid() {
			return nil, fmt.Errorf("%s có voucher thiếu promotionid, voucher_code hoặc signature hợp lệ", path)
		}
	}
	return sources, nil
}

func (src VoucherSource) valid() bool {
	return src.PromotionID > 0 && voucherCodeFormat.MatchString(src.Code) && signatureFormat.MatchString(src.Signature)
}

// Get refreshes at most every 15 minutes and never reuses results from before a failed refresh.
func (v *VoucherList) Get(ctx context.Context) []Voucher {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := time.Now()
	if !now.Before(v.nextRefresh) {
		sources, seen := []VoucherSource{}, map[string]bool{}
		for _, src := range append(append([]VoucherSource{}, v.sources...), v.discover(ctx)...) {
			key := strconv.FormatInt(src.PromotionID, 10) + ":" + src.Code
			if !seen[key] {
				seen[key] = true
				sources = append(sources, src)
			}
		}
		results := make([]*Voucher, len(sources))
		var wg sync.WaitGroup
		limit := make(chan struct{}, 4)
		for i, src := range sources {
			wg.Add(1)
			go func() {
				defer wg.Done()
				limit <- struct{}{}
				defer func() { <-limit }()
				item, err := v.fetch(ctx, src)
				if err != nil {
					log.Printf("Shopee %s: bỏ qua (%v).", src.Code, err)
				}
				results[i] = item
			}()
		}
		wg.Wait()
		v.cached = v.cached[:0]
		for _, item := range results {
			if item != nil {
				v.cached = append(v.cached, *item)
			}
		}
		if len(v.cached) > 0 {
			v.nextRefresh = now.Add(15 * time.Minute)
		} else {
			v.nextRefresh = now.Add(time.Minute)
		}
		log.Printf("Shopee: đọc được chi tiết %d/%d voucher.", len(v.cached), len(sources))
	}
	active := []Voucher{}
	for _, item := range v.cached {
		if item.Expires.After(now) {
			active = append(active, item)
		}
	}
	return active
}

func fetchVoucher(ctx context.Context, client *http.Client, src VoucherSource) (*Voucher, error) {
	body, _ := json.Marshal(src)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, voucherDetailAPI, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	buf, status, err := readLimited(client, req, 1<<20)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", status)
	}
	return parseVoucherDetail(buf, src)
}

func parseVoucherDetail(buf []byte, src VoucherSource) (*Voucher, error) {
	var body struct {
		Error *int `json:"error"`
		Data  struct {
			Terms struct {
				Description string `json:"description"`
			} `json:"voucher_usage_term"`
		} `json:"data"`
	}
	if json.Unmarshal(buf, &body) != nil || body.Error == nil || *body.Error != 0 {
		return nil, errors.New("api_error")
	}
	description := strings.TrimSpace(body.Data.Terms.Description)
	if description == "" {
		return nil, errors.New("missing_description")
	}
	// Fail closed when expiry is missing or in an unfamiliar format.
	m := expiryFinder.FindStringSubmatch(description)
	if m == nil {
		return nil, errors.New("missing_expiry")
	}
	expires, err := time.ParseInLocation("2/1/2006 15:04", strings.Join(strings.Fields(strings.ReplaceAll(m[1], "-", "/")), " "), vietnamTime)
	if err != nil {
		return nil, errors.New("invalid_expiry")
	}
	if !regexp.MustCompile(`(^|[^A-Za-z0-9_])` + regexp.QuoteMeta(src.Code) + `([^A-Za-z0-9_]|$)`).MatchString(description) {
		return nil, errors.New("code_mismatch")
	}
	link := "https://shopee.vn/voucher/details?" + url.Values{
		"evcode": {base64.StdEncoding.EncodeToString([]byte(src.Code))}, "from_source": {"microsite"},
		"promotionId": {strconv.FormatInt(src.PromotionID, 10)}, "signature": {src.Signature}, "source": {"0"},
	}.Encode()
	return &Voucher{Code: src.Code, Description: description, URL: link, Expires: expires}, nil
}

func discoverVouchers(ctx context.Context, client *http.Client) []VoucherSource {
	var found []VoucherSource
	for _, page := range voucherSourcePages {
		slug := page[strings.LastIndex(page, "/")+1:]
		count := 0
		for _, target := range []string{page, "https://shopee.vn/api/v4/pagebuilder/get_csr_page?" +
			url.Values{"page_url": {slug}, "platform": {"5"}, "timestamp": {"0"}}.Encode()} {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
			if err != nil {
				continue
			}
			buf, status, err := readLimited(client, req, 2<<20)
			if err != nil || status != http.StatusOK {
				log.Printf("Shopee /%s: không đọc được nguồn (HTTP %d).", slug, status)
				continue
			}
			sources := extractVoucherSources(string(buf))
			count += len(sources)
			found = append(found, sources...)
		}
		note := ""
		if count == 0 {
			note = " Trang tải danh sách bằng JavaScript nên chưa đọc được; không có nghĩa trang không có mã."
		}
		log.Printf("Shopee /%s: đọc được %d tham chiếu mã công khai.%s", slug, count, note)
	}
	return found
}

// extractVoucherSources only accepts complete voucher references exposed by public pages.
func extractVoucherSources(content string) []VoucherSource {
	found, order := map[string]VoucherSource{}, []string{}
	add := func(src VoucherSource) {
		key := strconv.FormatInt(src.PromotionID, 10) + ":" + src.Code
		if src.valid() && found[key] == (VoucherSource{}) {
			order = append(order, key)
			found[key] = src
		}
	}
	var visit func(value any, depth int)
	visitString := func(s string, depth int) {
		decoded := html.UnescapeString(strings.ReplaceAll(s, `\/`, "/"))
		if trimmed := strings.TrimSpace(decoded); strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
			dec := json.NewDecoder(strings.NewReader(trimmed))
			dec.UseNumber()
			var nested any
			if dec.Decode(&nested) == nil {
				visit(nested, depth+1)
			}
		}
		for _, link := range voucherLinkFinder.FindAllString(decoded, -1) {
			u, err := url.Parse(link)
			if err != nil {
				continue
			}
			q := u.Query()
			code := q.Get("voucher_code")
			if code == "" {
				raw, err := base64.StdEncoding.DecodeString(q.Get("evcode"))
				if err != nil || !utf8.Valid(raw) {
					continue
				}
				code = string(raw)
			}
			id, _ := strconv.ParseInt(q.Get("promotionId"), 10, 64)
			add(VoucherSource{PromotionID: id, Code: code, Signature: q.Get("signature")})
		}
	}
	visit = func(value any, depth int) {
		if depth > 30 {
			return
		}
		switch v := value.(type) {
		case map[string]any:
			pid, ok := v["promotionid"]
			if !ok {
				pid = v["promotionId"]
			}
			id, _ := strconv.ParseInt(fmt.Sprint(pid), 10, 64)
			code, _ := v["voucher_code"].(string)
			signature, _ := v["signature"].(string)
			add(VoucherSource{PromotionID: id, Code: code, Signature: signature})
			for _, child := range v {
				visit(child, depth+1)
			}
		case []any:
			for _, child := range v {
				visit(child, depth+1)
			}
		case string:
			visitString(v, depth)
		}
	}
	visit(content, 0)
	result := make([]VoucherSource, 0, len(order))
	for _, key := range order {
		result = append(result, found[key])
	}
	return result
}

func readLimited(client *http.Client, req *http.Request, limit int64) ([]byte, int, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, errors.New("lỗi kết nối")
	}
	defer resp.Body.Close()
	buf, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, resp.StatusCode, errors.New("lỗi đọc phản hồi")
	}
	if int64(len(buf)) > limit {
		return nil, resp.StatusCode, errors.New("dữ liệu vượt giới hạn kích thước")
	}
	return buf, resp.StatusCode, nil
}

// parseVND reads amounts written as "50000VND", "35K", "1.000.000đ" or "1,5tr"; 0 when unreadable.
func parseVND(s string) int64 {
	s = strings.ToLower(strings.TrimSpace(s))
	mult := 1.0
	for _, unit := range []struct {
		suffix string
		mult   float64
	}{{"k", 1e3}, {"triệu", 1e6}, {"tr", 1e6}} {
		if strings.HasSuffix(s, unit.suffix) {
			s, mult = strings.TrimSuffix(s, unit.suffix), unit.mult
			break
		}
	}
	s = strings.TrimSpace(strings.TrimRight(s, "vndđ₫ "))
	if mult == 1 {
		s = strings.NewReplacer(".", "", ",", "").Replace(s)
	} else {
		s = strings.ReplaceAll(s, ",", ".")
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v <= 0 {
		return 0
	}
	return int64(v*mult + 0.5)
}

func shortVND(v int64) string {
	amount, unit := float64(v), "đ"
	switch {
	case v >= 1e6:
		amount, unit = amount/1e6, "tr"
	case v >= 1e3:
		amount, unit = amount/1e3, "k"
	}
	return strings.Replace(strconv.FormatFloat(amount, 'f', -1, 64), ".", ",", 1) + unit
}

// voucherHeadline condenses Shopee's terms, e.g. "Giảm 30% tối đa 50k · Đơn từ 50k"; empty when unrecognised.
func voucherHeadline(description string) string {
	head := ""
	if m := voucherPercent.FindStringSubmatch(description); m != nil {
		head = "Giảm " + strings.Replace(m[1], ".", ",", 1) + "%"
		if v := parseVND(m[2]); v > 0 {
			head += " tối đa " + shortVND(v)
		}
	} else if m := voucherFixed.FindStringSubmatch(description); m != nil && parseVND(m[1]) > 0 {
		head = "Giảm " + shortVND(parseVND(m[1]))
	}
	if head == "" {
		return ""
	}
	if m := voucherMinSpend.FindStringSubmatch(description); m != nil && parseVND(m[1]) > 0 {
		head += " · Đơn từ " + shortVND(parseVND(m[1]))
	}
	return head
}

// productVoucherLines lists active public vouchers under a product link. Shopee exposes no public
// check that a voucher fits a given product or account, so the list says so.
func productVoucherLines(items []Voucher, link func(string) string, limit int) string {
	var b strings.Builder
	seen, count := map[string]bool{}, 0
	for _, item := range items {
		key := strings.ToLower(item.Code)
		if seen[key] || count == limit {
			continue
		}
		seen[key] = true
		head := voucherHeadline(item.Description)
		if head == "" {
			head = "Mã " + item.Code
		}
		fmt.Fprintf(&b, "\n👉 %s · HSD %s\n%s\n", head, item.Expires.In(vietnamTime).Format("02/01"), link(item.URL))
		count++
	}
	if count == 0 {
		return ""
	}
	return "🎟️ Mã giảm giá đang có:" + b.String() + "(Tuỳ sản phẩm và tài khoản, kiểm tra lại ở bước thanh toán.)\n\n"
}

// listVouchers formats one page; whole vouchers only, conditions are never cut to fit.
func listVouchers(items []Voucher, page int) string {
	var blocks []string
	seen := map[string]bool{}
	for _, item := range items {
		key := strings.ToLower(item.Code)
		if seen[key] {
			continue
		}
		seen[key] = true
		block := fmt.Sprintf("Mã: %s (nguồn Shopee)\n%s\n%s", item.Code, item.Description, item.URL)
		if utf8.RuneCountInString(block) > 1600 {
			block = fmt.Sprintf("Mã: %s (nguồn Shopee)\nĐiều kiện đầy đủ: %s", item.Code, item.URL)
		}
		blocks = append(blocks, block)
	}
	if len(blocks) == 0 {
		return "Chưa đọc được mã còn hạn dùng từ nguồn đã cấu hình.\nKho mã Shopee: " + vouchersPageURL
	}
	var pages []string
	current := ""
	for i, block := range blocks {
		block = fmt.Sprintf("%d. %s", i+1, block)
		if current != "" && utf8.RuneCountInString(current)+2+utf8.RuneCountInString(block) > voucherPageChars {
			pages = append(pages, current)
			current = block
		} else if current == "" {
			current = block
		} else {
			current += "\n\n" + block
		}
	}
	pages = append(pages, current)
	if page > len(pages) {
		return fmt.Sprintf("Chỉ có %d trang. Gửi @bot mã giảm giá 1 để xem từ đầu.", len(pages))
	}
	text := fmt.Sprintf("🎟️ Danh sách %d mã — trang %d/%d\nChưa xác nhận đủ điều kiện, đã bắt đầu hoặc còn lượt dùng.\n\n%s",
		len(blocks), page, len(pages), pages[page-1])
	if page < len(pages) {
		text += fmt.Sprintf("\n\nXem tiếp: @bot mã giảm giá %d", page+1)
	}
	return text + "\n\nKho mã Shopee: " + vouchersPageURL
}
