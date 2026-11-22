package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSignAndVerifyXunhu(t *testing.T) {
	t.Parallel()
	secret := "test-secret-key-123"
	params := map[string]string{
		"version":        "1.1",
		"appid":          "201906123456",
		"trade_order_id": "CP202611220001ABCD",
		"total_fee":      "30.00",
		"title":          "Monthly Pro",
		"empty_ignored":  "",
	}
	sig := SignXunhu(params, secret)
	if len(sig) != 32 {
		t.Fatalf("signature len = %d, want 32", len(sig))
	}
	// Empty values and existing hash must not affect signature.
	paramsWithHash := map[string]string{
		"version":        "1.1",
		"appid":          "201906123456",
		"trade_order_id": "CP202611220001ABCD",
		"total_fee":      "30.00",
		"title":          "Monthly Pro",
		"hash":           sig,
	}
	if !VerifyXunhuSignature(paramsWithHash, secret) {
		t.Fatal("expected valid signature to verify")
	}
	paramsWithHash["total_fee"] = "0.01"
	if VerifyXunhuSignature(paramsWithHash, secret) {
		t.Fatal("expected tampered amount to fail verification")
	}
	if VerifyXunhuSignature(paramsWithHash, "") {
		t.Fatal("expected empty secret to fail verification")
	}
}

func TestCNYCentsConversion(t *testing.T) {
	t.Parallel()
	if got := FormatCNYYuan(3050); got != "30.50" {
		t.Fatalf("FormatCNYYuan(3050) = %q, want 30.50", got)
	}
	cents, err := ParseCNYCents("30.50")
	if err != nil || cents != 3050 {
		t.Fatalf("ParseCNYCents(30.50) = %d, %v", cents, err)
	}
	if _, err := ParseCNYCents("-5.00"); err == nil {
		t.Fatal("expected negative amount to error")
	}
}

func TestXunhuClientCreateQueryAndTest(t *testing.T) {
	t.Parallel()
	secret := "my-xunhu-secret"
	appID := "app-999"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		got := map[string]string{}
		for k, v := range r.PostForm {
			if len(v) > 0 {
				got[k] = v[0]
			}
		}
		if !VerifyXunhuSignature(got, secret) {
			_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 401, "errmsg": "invalid hash signature"})
			return
		}
		switch r.URL.Path {
		case "/do":
			if got["appid"] != appID || got["total_fee"] != "29.90" {
				_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 500, "errmsg": "unexpected params"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"errcode":    0,
				"errmsg":     "success!",
				"openid":     "XH-OPEN-1",
				"url_qrcode": "https://pay.example.test/qr/1.png",
				"url":        "https://pay.example.test/h5/1",
			})
		case "/query":
			if got["out_trade_order"] == "ORDER-PAID" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"errcode": 0,
					"data": map[string]any{
						"status":        "OD",
						"open_order_id": "XH-OPEN-1",
						"total_fee":     "29.90",
					},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 404, "errmsg": "order not found"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewXunhuClient(server.Client())
	ctx := context.Background()

	created, err := client.CreatePayment(ctx, XunhuCreateRequest{
		GatewayURL:   server.URL + "/do",
		AppID:        appID,
		AppSecret:    secret,
		Channel:      ChannelWeChat,
		TradeOrderID: "ORDER-PAID",
		AmountCents:  2990,
		Title:        "Pro Monthly",
		NotifyURL:    "https://relay.example.test/callback",
	})
	if err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	if created.QRCodeURL != "https://pay.example.test/qr/1.png" || created.OpenOrderID != "XH-OPEN-1" {
		t.Fatalf("unexpected create response: %+v", created)
	}

	queried, err := client.QueryOrder(ctx, XunhuQueryRequest{
		QueryURL:     server.URL + "/query",
		AppID:        appID,
		AppSecret:    secret,
		TradeOrderID: "ORDER-PAID",
	})
	if err != nil {
		t.Fatalf("QueryOrder: %v", err)
	}
	if queried.Status != "OD" || queried.AmountCents != 2990 {
		t.Fatalf("unexpected query response: %+v", queried)
	}

	if err := client.TestChannel(ctx, server.URL+"/query", appID, secret); err != nil {
		t.Fatalf("TestChannel valid secret: %v", err)
	}
	if err := client.TestChannel(ctx, server.URL+"/query", appID, "wrong-secret"); err == nil {
		t.Fatal("expected TestChannel with wrong secret to fail")
	}
}
