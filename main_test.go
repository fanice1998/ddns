package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cloudflare/cloudflare-go/v6"
	"github.com/cloudflare/cloudflare-go/v6/option"
)

func TestParseIP(t *testing.T) {
	cases := []struct {
		body   string
		wantV4 bool
		want   string
		ok     bool
	}{
		{"203.0.113.7\n", true, "203.0.113.7", true},
		{"  198.51.100.1  ", true, "198.51.100.1", true},
		{"2001:db8::1", true, "", false},
		{"<html>error</html>", true, "", false},
		{"", true, "", false},
		{"2001:db8::1\n", false, "2001:db8::1", true},
		{"2001:0db8:0000::0001", false, "2001:db8::1", true},
		{"203.0.113.7", false, "", false},
		{"::ffff:203.0.113.7", false, "", false},
	}
	for _, c := range cases {
		got, err := parseIP(c.body, c.wantV4)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("parseIP(%q, v4=%v) = %q, %v; want %q, ok=%v", c.body, c.wantV4, got, err, c.want, c.ok)
		}
	}
}

func TestGetPublicIPFallback(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "203.0.113.9", http.StatusServiceUnavailable)
	}))
	defer bad.Close()
	v6 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "2001:db8::1\n")
	}))
	defer v6.Close()
	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "<html>captive portal</html>")
	}))
	defer html.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "203.0.113.7\n")
	}))
	defer good.Close()

	urls := []string{bad.URL, v6.URL, html.URL, good.URL}
	ip, err := getPublicIP(urls, http.DefaultClient, true)
	if err != nil || ip != "203.0.113.7" {
		t.Fatalf("IPv4: got %q, %v", ip, err)
	}

	ip, err = getPublicIP(urls, http.DefaultClient, false)
	if err != nil || ip != "2001:db8::1" {
		t.Fatalf("IPv6: got %q, %v", ip, err)
	}

	if _, err := getPublicIP([]string{bad.URL, html.URL}, http.DefaultClient, true); err == nil {
		t.Fatal("expected error when every source fails")
	}
}

func TestNormalizeHostname(t *testing.T) {
	cases := []struct{ name, zone, want string }{
		{"home", "example.com", "home.example.com"},
		{"@", "example.com", "example.com"},
		{"example.com", "example.com", "example.com"},
		{"home.example.com", "example.com", "home.example.com"},
		{"home.example.com.", "example.com", "home.example.com"},
		{"Home.Example.com", "example.com", "Home.Example.com"},
		{"a.b", "example.com", "a.b.example.com"},
		{"notexample.com", "example.com", "notexample.com.example.com"},
		{"home", "", "home"},
		{"home.example.com", "", "home.example.com"},
	}
	for _, c := range cases {
		if got := normalizeHostname(c.name, c.zone); got != c.want {
			t.Errorf("normalizeHostname(%q, %q) = %q; want %q", c.name, c.zone, got, c.want)
		}
	}
}

func TestLoadConfigBackwardCompatible(t *testing.T) {
	dir := t.TempDir()
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	old := "api_token: tok\nzone_id: zid\nhostnames:\n  - home.example.com\n"
	if err := os.WriteFile(filepath.Join(dir, configFile), []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IPv6 || cfg.TTL != 0 || cfg.Proxied != nil {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}

	full := old + "ipv6: true\nttl: 300\nproxied: true\n"
	os.WriteFile(configFile, []byte(full), 0600)
	cfg, err = loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.IPv6 || cfg.TTL != 300 || cfg.Proxied == nil || !*cfg.Proxied {
		t.Fatalf("options not parsed: %+v", cfg)
	}

	os.WriteFile(configFile, []byte(old+"ttl: 10\n"), 0600)
	if _, err := loadConfig(); err == nil {
		t.Fatal("expected error for ttl 10")
	}

	os.WriteFile(configFile, []byte("api_token: tok\n"), 0600)
	if _, err := loadConfig(); err == nil {
		t.Fatal("expected error for incomplete config")
	}
}

// fakeCloudflare 模擬 DNS records 的 list / create / update
type fakeCloudflare struct {
	mu      sync.Mutex
	records []map[string]any
	updates []map[string]any
	creates []map[string]any
}

func (f *fakeCloudflare) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	envelope := func(result any) {
		json.NewEncoder(w).Encode(map[string]any{
			"success": true, "errors": []any{}, "messages": []any{}, "result": result,
			"result_info": map[string]any{"page": 1, "per_page": 100, "count": 1, "total_count": 1},
		})
	}
	var body map[string]any
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&body)
	}
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/dns_records"):
		var out []map[string]any
		for _, rec := range f.records {
			if rec["type"] == r.URL.Query().Get("type") && rec["name"] == r.URL.Query().Get("name.exact") {
				out = append(out, rec)
			}
		}
		envelope(out)
	case r.Method == http.MethodPost:
		f.creates = append(f.creates, body)
		body["id"] = "new"
		envelope(body)
	case r.Method == http.MethodPut:
		body["id"] = r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		f.updates = append(f.updates, body)
		envelope(body)
	default:
		http.Error(w, "unexpected", http.StatusNotFound)
	}
}

func newFake(t *testing.T, records ...map[string]any) (*fakeCloudflare, *cloudflare.Client) {
	f := &fakeCloudflare{records: records}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	client := cloudflare.NewClient(option.WithAPIToken("test"), option.WithBaseURL(srv.URL+"/"), option.WithMaxRetries(0))
	return f, client
}

func TestUpdateRecordsUpdatesAllAndPreservesProxied(t *testing.T) {
	f, client := newFake(t,
		map[string]any{"id": "r1", "type": "A", "name": "home.example.com", "content": "198.51.100.1", "proxied": true, "ttl": 1},
		map[string]any{"id": "r2", "type": "A", "name": "home.example.com", "content": "198.51.100.2", "proxied": false, "ttl": 300},
		map[string]any{"id": "r3", "type": "A", "name": "other.example.com", "content": "198.51.100.3", "proxied": false, "ttl": 1},
	)
	cfg := &Config{ZoneID: "zid"}
	err := updateRecords(context.Background(), client, cfg, "home.example.com", recordKind{label: "A", ip: "203.0.113.7"})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.updates) != 2 || len(f.creates) != 0 {
		t.Fatalf("updates=%d creates=%d", len(f.updates), len(f.creates))
	}
	if f.updates[0]["id"] != "r1" || f.updates[0]["proxied"] != true || f.updates[0]["ttl"] != float64(1) {
		t.Errorf("r1 update wrong: %v", f.updates[0])
	}
	if f.updates[1]["id"] != "r2" || f.updates[1]["proxied"] != false || f.updates[1]["ttl"] != float64(300) {
		t.Errorf("r2 update wrong: %v", f.updates[1])
	}
	for _, u := range f.updates {
		if u["content"] != "203.0.113.7" {
			t.Errorf("content not updated: %v", u)
		}
	}
}

func TestUpdateRecordsExplicitOptionsAndSkip(t *testing.T) {
	no := false
	f, client := newFake(t,
		map[string]any{"id": "r1", "type": "A", "name": "home.example.com", "content": "203.0.113.7", "proxied": true, "ttl": 1},
	)
	// 同 IP、未指定選項 → 略過
	if err := updateRecords(context.Background(), client, &Config{ZoneID: "zid"}, "home.example.com", recordKind{"A", "203.0.113.7"}); err != nil {
		t.Fatal(err)
	}
	if len(f.updates) != 0 {
		t.Fatalf("expected skip, got %v", f.updates)
	}
	// 同 IP、明確指定 proxied=false、ttl=120 → 更新
	cfg := &Config{ZoneID: "zid", Proxied: &no, TTL: 120}
	if err := updateRecords(context.Background(), client, cfg, "home.example.com", recordKind{"A", "203.0.113.7"}); err != nil {
		t.Fatal(err)
	}
	if len(f.updates) != 1 || f.updates[0]["proxied"] != false || f.updates[0]["ttl"] != float64(120) {
		t.Fatalf("explicit options not applied: %v", f.updates)
	}
}

func TestUpdateRecordsCreatesAAAA(t *testing.T) {
	f, client := newFake(t)
	err := updateRecords(context.Background(), client, &Config{ZoneID: "zid"}, "home.example.com", recordKind{"AAAA", "2001:db8::1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.creates) != 1 {
		t.Fatalf("creates=%d", len(f.creates))
	}
	c := f.creates[0]
	if c["type"] != "AAAA" || c["content"] != "2001:db8::1" || c["proxied"] != false || c["ttl"] != float64(1) {
		t.Errorf("create wrong: %v", c)
	}
}
