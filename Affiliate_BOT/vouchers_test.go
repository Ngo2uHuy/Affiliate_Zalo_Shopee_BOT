package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

const testSignature = "74a6e5a2b8937ac739aeed2a2084baf2fcc6428376f18c0c6e7e647cd79e24b2"

func TestVoucherPage(t *testing.T) {
	cases := map[string]int{
		"@Bot của Nguyễn Nguyễn mã giảm giá": 1,
		"mã giảm giá 2":                      2,
		"MÃ GIẢM GIÁ":                        1,
		"mã giảm giá 3":                   3, // NFD input
		"/voucher":                           1,
		"magiamgia 0":                        1,
		"có mã giảm giá cho áo không":        0,
		"thơm giảm giá":                      0,
		"xin chào":                           0,
	}
	for text, want := range cases {
		if got := voucherPage(text); got != want {
			t.Errorf("%q -> %d; want %d", text, got, want)
		}
	}
}

func detail(description string) []byte {
	return []byte(fmt.Sprintf(`{"error":0,"data":{"voucher_usage_term":{"description":%q}}}`, description))
}

func TestParseVoucherDetail(t *testing.T) {
	src := VoucherSource{PromotionID: 15, Code: "OCT26ID37", Signature: testSignature}
	item, err := parseVoucherDetail(detail("Mã OCT26ID37 giảm 10%. HSD 31-10-2026 23:59"), src)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 10, 31, 23, 59, 0, 0, vietnamTime); !item.Expires.Equal(want) {
		t.Errorf("expires %v", item.Expires)
	}
	if !strings.Contains(item.URL, "promotionId=15") || !strings.Contains(item.URL, "evcode=T0NUMjZJRDM3") {
		t.Errorf("url %s", item.URL)
	}
	for body, reason := range map[string]string{
		`{"error":1}`: "api_error",
		string(detail("Mã OCT26ID37 giảm 10%")):            "missing_expiry",
		string(detail("Mã OCT26ID370 HSD: 1/1/2027 9:00")): "code_mismatch",
		string(detail("   ")):                              "missing_description",
	} {
		if _, err := parseVoucherDetail([]byte(body), src); err == nil || err.Error() != reason {
			t.Errorf("%s -> %v; want %s", body, err, reason)
		}
	}
}

func TestExtractVoucherSources(t *testing.T) {
	page := `<a href="https://shopee.vn/voucher/details?evcode=T0NUMjZJRDM3&amp;promotionId=11&amp;signature=` + testSignature + `">x</a>` +
		`<script>{"props":"{\"promotionId\":1519607980003328,\"voucher_code\":\"VCXUL1015\",\"signature\":\"` + testSignature + `\"}",` +
		`"collection":{"promotionid":99}}</script>`
	got := extractVoucherSources(page)
	if len(got) != 1 || got[0].Code != "OCT26ID37" || got[0].PromotionID != 11 {
		t.Fatalf("html: %+v", got)
	}
	got = extractVoucherSources(`{"data":{"items":[{"promotionId":1519607980003328,"voucher_code":"VCXUL1015","signature":"` +
		testSignature + `"},{"promotionid":99}]}}`)
	if len(got) != 1 || got[0].PromotionID != 1519607980003328 || got[0].Code != "VCXUL1015" {
		t.Fatalf("json: %+v", got)
	}
}

func TestListVouchersPaginates(t *testing.T) {
	var items []Voucher
	for i := 0; i < 12; i++ {
		items = append(items, Voucher{Code: fmt.Sprintf("CODE%d", i), Description: strings.Repeat("điều kiện ", 30), URL: "https://shopee.vn/voucher/details?x"})
	}
	items = append(items, Voucher{Code: "code0"}) // duplicate, case-insensitive
	first := listVouchers(items, 1)
	if !strings.Contains(first, "Danh sách 12 mã — trang 1/") || !strings.Contains(first, "Xem tiếp: @bot mã giảm giá 2") {
		t.Fatalf("page 1:\n%s", first)
	}
	if n := len([]rune(first)); n > 2000 {
		t.Errorf("page too long: %d", n)
	}
	if got := listVouchers(items, 99); !strings.HasPrefix(got, "Chỉ có") {
		t.Errorf("page 99: %s", got)
	}
	if got := listVouchers(nil, 1); !strings.Contains(got, "Chưa đọc được mã") {
		t.Errorf("empty: %s", got)
	}
}

func TestVoucherListCachesAndDropsFailedRefresh(t *testing.T) {
	fail, calls := false, 0
	future := time.Now().Add(time.Hour)
	v := &VoucherList{
		sources: []VoucherSource{{PromotionID: 1, Code: "A", Signature: testSignature}},
		discover: func(context.Context) []VoucherSource {
			return []VoucherSource{{PromotionID: 1, Code: "A", Signature: testSignature}}
		},
		fetch: func(_ context.Context, src VoucherSource) (*Voucher, error) {
			calls++
			if fail {
				return nil, errors.New("HTTP 403")
			}
			return &Voucher{Code: src.Code, Expires: future}, nil
		},
	}
	if got := v.Get(context.Background()); len(got) != 1 || calls != 1 {
		t.Fatalf("first: %d items, %d calls", len(got), calls)
	}
	v.Get(context.Background())
	if calls != 1 {
		t.Fatalf("cache not used: %d calls", calls)
	}
	fail, v.nextRefresh = true, time.Time{}
	if got := v.Get(context.Background()); len(got) != 0 {
		t.Fatalf("stale vouchers reused after failed refresh: %d", len(got))
	}
}

func TestProductVoucherLines(t *testing.T) {
	cases := map[string]string{
		"Mã METAPAROCTC1218 giảm 30% tối đa 50000VND cho đơn hàng từ 50000VND. HSD 18/10/2026 23:59.":                          "Giảm 30% tối đa 50k · Đơn từ 50k",
		"Chỉ áp dụng cho một số sản phẩm. Mã OCT26ID38 giảm 20% tối đa 100K cho đơn hàng hợp lệ từ 299K trên ứng dụng Shopee.": "Giảm 20% tối đa 100k · Đơn từ 299k",
		"Mã X giảm 15.000đ cho đơn từ 1,5tr. HSD 18/10/2026 23:59.":                                                            "Giảm 15k · Đơn từ 1,5tr",
		"Mã X ưu đãi đặc biệt. HSD 18/10/2026 23:59.":                                                                          "",
	}
	for description, want := range cases {
		if got := voucherHeadline(description); got != want {
			t.Errorf("voucherHeadline(%q) = %q, want %q", description, got, want)
		}
	}
	expires := time.Date(2026, 10, 18, 23, 59, 0, 0, vietnamTime)
	items := []Voucher{
		{Code: "A", Description: "Mã A giảm 30% tối đa 50000VND cho đơn hàng từ 50000VND.", URL: "https://shopee.vn/voucher/details?a", Expires: expires},
		{Code: "a", Description: "duplicate", URL: "https://shopee.vn/voucher/details?dup", Expires: expires},
		{Code: "B", Description: "ưu đãi", URL: "https://shopee.vn/voucher/details?b", Expires: expires},
		{Code: "C", Description: "ưu đãi", URL: "https://shopee.vn/voucher/details?c", Expires: expires},
	}
	got := productVoucherLines(items, func(u string) string { return "short:" + u }, 2)
	want := "🎟️ Mã giảm giá đang có:\n👉 Giảm 30% tối đa 50k · Đơn từ 50k · HSD 18/10\nshort:https://shopee.vn/voucher/details?a\n" +
		"\n👉 Mã B · HSD 18/10\nshort:https://shopee.vn/voucher/details?b\n(Tuỳ sản phẩm và tài khoản, kiểm tra lại ở bước thanh toán.)\n\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if productVoucherLines(nil, func(u string) string { return u }, 5) != "" {
		t.Fatal("empty list must add nothing")
	}
}
