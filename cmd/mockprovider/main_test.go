package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMockProviderAnswersTheContractAndCountsPeaks(t *testing.T) {
	srv := httptest.NewServer(newHandler(0))
	defer srv.Close()
	for _, ch := range []string{"sms", "sms", "sms", "email"} {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/some-uuid",
			strings.NewReader(`{"to":"x","channel":"`+ch+`","content":"c"}`))
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/stats", nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var stats struct {
		Received int            `json:"received"`
		Peaks    map[string]int `json:"maxInAnyOneSecondWindow"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		t.Fatal(err)
	}
	if stats.Received != 4 || stats.Peaks["sms"] != 3 || stats.Peaks["email"] != 1 || stats.Peaks["push"] != 0 || stats.Peaks["all"] != 4 {
		t.Fatalf("stats %+v", stats)
	}
}
