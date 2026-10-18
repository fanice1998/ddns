package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/cloudflare/cloudflare-go/v6"
	"github.com/cloudflare/cloudflare-go/v6/dns"
	"github.com/cloudflare/cloudflare-go/v6/option"
	"github.com/cloudflare/cloudflare-go/v6/zones"
	"gopkg.in/yaml.v3"
)

const configFile = "config.yaml"

// 每個 hostname（每種紀錄類型）的 Cloudflare API 逾時
const perHostTimeout = 30 * time.Second

type Config struct {
	APIToken  string   `yaml:"api_token"`
	ZoneID    string   `yaml:"zone_id"`
	Hostnames []string `yaml:"hostnames"`

	// 以下為選填，未設定時維持原本行為
	IPv6    bool  `yaml:"ipv6,omitempty"`    // true 時同時更新 AAAA 紀錄
	TTL     int   `yaml:"ttl,omitempty"`     // 1 = 自動，或 30~86400 秒；未設定：新建為 1，更新時保留原值
	Proxied *bool `yaml:"proxied,omitempty"` // 未設定：新建為 false，更新時保留原值
}

// 公網 IP 來源（依序嘗試）
var (
	ipv4Sources = []string{
		"https://api.ipify.org",
		"https://ipv4.icanhazip.com",
		"https://ifconfig.me/ip",
	}
	ipv6Sources = []string{
		"https://api6.ipify.org",
		"https://ipv6.icanhazip.com",
		"https://ifconfig.me/ip",
	}
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[ddns] ")

	if len(os.Args) > 1 && os.Args[1] == "init" {
		runInit()
		return
	}

	cfg, err := loadConfig()
	if err != nil {
		// 沒有 config 就安靜結束，適合開機自動執行
		if os.IsNotExist(err) {
			log.Println("config.yaml 不存在，跳過更新")
			return
		}
		log.Fatalf("讀取設定失敗: %v", err)
	}

	if err := updateDNS(cfg); err != nil {
		log.Fatalf("更新失敗: %v", err)
	}

	log.Println("全部處理完成")
}

func runInit() {
	reader := bufio.NewReader(os.Stdin)

	log.Println("=== Cloudflare DDNS 初始化 ===")

	apiToken := prompt(reader, "Cloudflare API Token")
	zoneID := prompt(reader, "Zone ID")
	hostsInput := prompt(reader, "要更新的 DNS 名稱（多個用逗號分隔，例如 home.example.com,nas,@；短名稱與 @ 會自動補上 zone 網域）")

	hostnames := []string{}
	for _, h := range strings.Split(hostsInput, ",") {
		h = strings.TrimSpace(h)
		if h != "" {
			hostnames = append(hostnames, h)
		}
	}

	if apiToken == "" {
		log.Fatal("API Token 不可為空")
	}
	if zoneID == "" {
		log.Fatal("Zone ID 不可為空")
	}
	if len(hostnames) == 0 {
		log.Fatal("至少需要一個 DNS 名稱")
	}

	cfg := Config{
		APIToken:  apiToken,
		ZoneID:    zoneID,
		Hostnames: hostnames,
	}

	data, err := yaml.Marshal(&cfg)
	if err != nil {
		log.Fatalf("產生設定檔失敗: %v", err)
	}

	if err := os.WriteFile(configFile, data, 0600); err != nil {
		log.Fatalf("寫入 config.yaml 失敗: %v", err)
	}

	log.Println("已成功寫入 config.yaml")
	log.Println("現在可以直接執行 ./ddns 來更新 DNS")
}

func loadConfig() (*Config, error) {
	data, err := os.ReadFile(configFile)
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析 config.yaml 失敗: %w", err)
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// validate 檢查必填欄位並套用選填欄位的預設值
func (c *Config) validate() error {
	if c.APIToken == "" || c.ZoneID == "" || len(c.Hostnames) == 0 {
		return fmt.Errorf("config.yaml 內容不完整，請重新執行 init")
	}
	if c.TTL != 0 && c.TTL != 1 && (c.TTL < 30 || c.TTL > 86400) {
		return fmt.Errorf("ttl 必須是 1（自動）或 30~86400 秒，目前為 %d", c.TTL)
	}
	return nil
}

// normalizeHostname 把短名稱或 "@" 轉成完整網域名稱（FQDN）。
// zone 為空時（無法取得 zone 名稱）原樣回傳。
func normalizeHostname(name, zone string) string {
	name = strings.TrimSuffix(strings.TrimSpace(name), ".")
	zone = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(zone), "."))
	if zone == "" {
		return name
	}
	lower := strings.ToLower(name)
	if lower == "@" || lower == zone {
		return zone
	}
	if strings.HasSuffix(lower, "."+zone) {
		return name
	}
	return name + "." + zone
}

type recordKind struct {
	label string // "A" 或 "AAAA"
	ip    string
}

func updateDNS(cfg *Config) error {
	client := cloudflare.NewClient(
		option.WithAPIToken(cfg.APIToken),
	)

	publicIP, err := getPublicIP(ipv4Sources, newHTTPClient("tcp4"), true)
	if err != nil {
		return fmt.Errorf("無法取得公網 IP，請檢查網路連線: %w", err)
	}
	log.Printf("目前公網 IP: %s", publicIP)

	kinds := []recordKind{{label: "A", ip: publicIP}}

	if cfg.IPv6 {
		ip6, err := getPublicIP(ipv6Sources, newHTTPClient("tcp6"), false)
		if err != nil {
			log.Printf("無法取得公網 IPv6，略過 AAAA 紀錄: %v", err)
		} else {
			log.Printf("目前公網 IPv6: %s", ip6)
			kinds = append(kinds, recordKind{label: "AAAA", ip: ip6})
		}
	}

	zoneName := lookupZoneName(client, cfg.ZoneID)

	successCount := 0

	for _, h := range cfg.Hostnames {
		hostname := normalizeHostname(h, zoneName)
		for _, k := range kinds {
			ctx, cancel := context.WithTimeout(context.Background(), perHostTimeout)
			err := updateRecords(ctx, client, cfg, hostname, k)
			cancel()
			if err != nil {
				log.Printf("處理 %s (%s) 失敗: %v", hostname, k.label, err)
				continue
			}
			successCount++
		}
	}

	if successCount == 0 {
		return fmt.Errorf("所有 hostname 都更新失敗")
	}

	return nil
}

// lookupZoneName 取得 zone 的網域名稱，用來把短名稱補成 FQDN。
// 失敗時（例如 token 沒有 Zone Read 權限）回傳空字串，hostname 會原樣使用。
func lookupZoneName(client *cloudflare.Client, zoneID string) string {
	ctx, cancel := context.WithTimeout(context.Background(), perHostTimeout)
	defer cancel()
	z, err := client.Zones.Get(ctx, zones.ZoneGetParams{ZoneID: cloudflare.F(zoneID)})
	if err != nil {
		log.Printf("無法取得 zone 名稱，hostname 將原樣使用（請填完整網域名稱）: %v", classifyCloudflareError(err))
		return ""
	}
	return z.Name
}

func recordBody(hostname string, k recordKind, ttl dns.TTL, proxied bool) (dns.RecordNewParamsBodyUnion, dns.RecordUpdateParamsBodyUnion) {
	if k.label == "AAAA" {
		b := dns.AAAARecordParam{
			Name:    cloudflare.F(hostname),
			Type:    cloudflare.F(dns.AAAARecordTypeAAAA),
			Content: cloudflare.F(k.ip),
			TTL:     cloudflare.F(ttl),
			Proxied: cloudflare.F(proxied),
		}
		return b, b
	}
	b := dns.ARecordParam{
		Name:    cloudflare.F(hostname),
		Type:    cloudflare.F(dns.ARecordTypeA),
		Content: cloudflare.F(k.ip),
		TTL:     cloudflare.F(ttl),
		Proxied: cloudflare.F(proxied),
	}
	return b, b
}

// updateRecords 讓 hostname 所有同類型（A/AAAA）紀錄都指向 k.ip；不存在就建立。
func updateRecords(ctx context.Context, client *cloudflare.Client, cfg *Config, hostname string, k recordKind) error {
	listType := dns.RecordListParamsTypeA
	if k.label == "AAAA" {
		listType = dns.RecordListParamsTypeAAAA
	}

	page, err := client.DNS.Records.List(ctx, dns.RecordListParams{
		ZoneID: cloudflare.F(cfg.ZoneID),
		Type:   cloudflare.F(listType),
		Name: cloudflare.F(dns.RecordListParamsName{
			Exact: cloudflare.F(hostname),
		}),
	})
	if err != nil {
		return classifyCloudflareError(err)
	}

	if len(page.Result) == 0 {
		// 不存在 → 建立（未設定 proxied 時為 false）
		proxied := cfg.Proxied != nil && *cfg.Proxied
		ttl := dns.TTL1
		if cfg.TTL != 0 {
			ttl = dns.TTL(cfg.TTL)
		}
		body, _ := recordBody(hostname, k, ttl, proxied)
		if _, err := client.DNS.Records.New(ctx, dns.RecordNewParams{
			ZoneID: cloudflare.F(cfg.ZoneID),
			Body:   body,
		}); err != nil {
			return classifyCloudflareError(err)
		}
		log.Printf("已建立 %s (%s) → %s", hostname, k.label, k.ip)
		return nil
	}

	if len(page.Result) > 1 {
		log.Printf("%s 有 %d 筆 %s 紀錄，全部更新", hostname, len(page.Result), k.label)
	}

	var firstErr error
	for _, record := range page.Result {
		// 未設定 proxied / ttl 時保留紀錄原值
		proxied := record.Proxied
		if cfg.Proxied != nil {
			proxied = *cfg.Proxied
		}
		ttl := record.TTL
		if cfg.TTL != 0 {
			ttl = dns.TTL(cfg.TTL)
		}

		if record.Content == k.ip && record.Proxied == proxied && record.TTL == ttl {
			log.Printf("%s (%s) 已是最新 IP (%s)，略過", hostname, k.label, k.ip)
			continue
		}

		_, body := recordBody(hostname, k, ttl, proxied)
		if _, err := client.DNS.Records.Update(ctx, record.ID, dns.RecordUpdateParams{
			ZoneID: cloudflare.F(cfg.ZoneID),
			Body:   body,
		}); err != nil {
			if firstErr == nil {
				firstErr = classifyCloudflareError(err)
			}
			continue
		}
		log.Printf("已更新 %s (%s)：%s → %s", hostname, k.label, record.Content, k.ip)
	}

	return firstErr
}

// 把 Cloudflare 常見錯誤轉成比較好懂的訊息

func classifyCloudflareError(err error) error {
	if err == nil {
		return nil
	}

	// 正確的方式
	var apiErr *cloudflare.Error
	if errors.As(err, &apiErr) {
		// 這裡可以拿到 StatusCode 和詳細錯誤
		if len(apiErr.Errors) > 0 {
			return fmt.Errorf("Cloudflare API 錯誤: %s", apiErr.Errors[0].Message)
		}
		return fmt.Errorf("Cloudflare API 錯誤 (HTTP %d)", apiErr.StatusCode)
	}

	// 下面繼續用字串判斷網路相關錯誤
	errStr := err.Error()

	switch {
	case strings.Contains(errStr, "Authentication error") ||
		strings.Contains(errStr, "Invalid API Token") ||
		strings.Contains(errStr, "401"):
		return fmt.Errorf("Cloudflare 認證失敗，請檢查 API Token")

	case strings.Contains(errStr, "Could not route to") ||
		strings.Contains(errStr, "zone not found") ||
		strings.Contains(errStr, "403"):
		return fmt.Errorf("無法存取 Zone，請確認 Zone ID 與 Token 權限（需要 DNS Edit）")

	case strings.Contains(errStr, "timeout") ||
		strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "no such host"):
		return fmt.Errorf("無法連線到 Cloudflare API，請檢查網路")

	default:
		return err
	}
}

// newHTTPClient 回傳只用指定網路（"tcp4" 或 "tcp6"）連線的 HTTP client
func newHTTPClient(network string) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, _, addr string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, addr)
	}
	return &http.Client{Timeout: 8 * time.Second, Transport: transport}
}

// parseIP 驗證回應內容是否為指定版本的 IP，回傳標準格式
func parseIP(body string, wantV4 bool) (string, error) {
	s := strings.TrimSpace(body)
	ip := net.ParseIP(s)
	if ip == nil {
		return "", fmt.Errorf("回應不是有效的 IP: %q", s)
	}
	if wantV4 && ip.To4() == nil {
		return "", fmt.Errorf("回應不是 IPv4: %s", s)
	}
	if !wantV4 && ip.To4() != nil {
		return "", fmt.Errorf("回應不是 IPv6: %s", s)
	}
	return ip.String(), nil
}

// getPublicIP 依序向各來源查詢公網 IP，第一個 HTTP 200 且通過驗證的結果為準
func getPublicIP(urls []string, client *http.Client, wantV4 bool) (string, error) {
	lastErr := errors.New("沒有可用的 IP 來源")
	for _, u := range urls {
		resp, err := client.Get(u)
		if err != nil {
			lastErr = err
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("%s 回應 HTTP %d", u, resp.StatusCode)
			continue
		}
		ip, err := parseIP(string(body), wantV4)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", u, err)
			continue
		}
		return ip, nil
	}

	return "", fmt.Errorf("所有 IP 來源都失敗: %v", lastErr)
}

func prompt(reader *bufio.Reader, label string) string {
	fmt.Printf("%s: ", label)
	text, _ := reader.ReadString('\n')
	return strings.TrimSpace(text)
}
