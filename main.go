package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/cloudflare/cloudflare-go/v6"
	"github.com/cloudflare/cloudflare-go/v6/dns"
	"github.com/cloudflare/cloudflare-go/v6/option"
	"gopkg.in/yaml.v3"
)

const configFile = "config.yaml"

type Config struct {
	APIToken  string   `yaml:"api_token"`
	ZoneID    string   `yaml:"zone_id"`
	Hostnames []string `yaml:"hostnames"`
}

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
	hostsInput := prompt(reader, "要更新的 DNS 名稱（多個用逗號分隔，例如 home,nas,@）")

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

	if cfg.APIToken == "" || cfg.ZoneID == "" || len(cfg.Hostnames) == 0 {
		return nil, fmt.Errorf("config.yaml 內容不完整，請重新執行 init")
	}

	return &cfg, nil
}

func updateDNS(cfg *Config) error {
	client := cloudflare.NewClient(
		option.WithAPIToken(cfg.APIToken),
	)

	publicIP, err := getPublicIP()
	if err != nil {
		return fmt.Errorf("無法取得公網 IP，請檢查網路連線: %w", err)
	}
	log.Printf("目前公網 IP: %s", publicIP)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	successCount := 0

	for _, hostname := range cfg.Hostnames {
		if err := updateSingleRecord(ctx, client, cfg.ZoneID, hostname, publicIP); err != nil {
			log.Printf("處理 %s 失敗: %v", hostname, err)
			continue
		}
		successCount++
	}

	if successCount == 0 {
		return fmt.Errorf("所有 hostname 都更新失敗")
	}

	return nil
}

func updateSingleRecord(ctx context.Context, client *cloudflare.Client, zoneID, hostname, ip string) error {
	// 查詢現有 A record
	page, err := client.DNS.Records.List(ctx, dns.RecordListParams{
		ZoneID: cloudflare.F(zoneID),
		Type:   cloudflare.F(dns.RecordListParamsTypeA),
		Name: cloudflare.F(dns.RecordListParamsName{
			Exact: cloudflare.F(hostname),
		}),
	})
	if err != nil {
		return classifyCloudflareError(err)
	}

	if len(page.Result) == 0 {
		// 不存在 → 建立
		_, err := client.DNS.Records.New(ctx, dns.RecordNewParams{
			ZoneID: cloudflare.F(zoneID),
			Body: dns.ARecordParam{
				Name:    cloudflare.F(hostname),
				Type:    cloudflare.F(dns.ARecordTypeA),
				Content: cloudflare.F(ip),
				TTL:     cloudflare.F(dns.TTL1),
				Proxied: cloudflare.F(false),
			},
		})
		if err != nil {
			return classifyCloudflareError(err)
		}
		log.Printf("已建立 %s → %s", hostname, ip)
		return nil
	}

	record := page.Result[0]

	// IP 沒變
	if record.Content == ip {
		log.Printf("%s 已是最新 IP (%s)，略過", hostname, ip)
		return nil
	}

	// 需要更新
	_, err = client.DNS.Records.Update(ctx, record.ID, dns.RecordUpdateParams{
		ZoneID: cloudflare.F(zoneID),
		Body: dns.ARecordParam{
			Name:    cloudflare.F(hostname),
			Type:    cloudflare.F(dns.ARecordTypeA),
			Content: cloudflare.F(ip),
			TTL:     cloudflare.F(dns.TTL1),
			Proxied: cloudflare.F(false),
		},
	})
	if err != nil {
		return classifyCloudflareError(err)
	}

	log.Printf("已更新 %s：%s → %s", hostname, record.Content, ip)
	return nil
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

func getPublicIP() (string, error) {
	urls := []string{
		"https://api.ipify.org",
		"https://ipv4.icanhazip.com",
		"https://ifconfig.me/ip",
	}

	client := &http.Client{Timeout: 8 * time.Second}

	var lastErr error
	for _, u := range urls {
		resp, err := client.Get(u)
		if err != nil {
			lastErr = err
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		ip := strings.TrimSpace(string(body))
		if ip != "" {
			return ip, nil
		}
	}

	return "", fmt.Errorf("所有 IP 來源都失敗: %v", lastErr)
}

func prompt(reader *bufio.Reader, label string) string {
	fmt.Printf("%s: ", label)
	text, _ := reader.ReadString('\n')
	return strings.TrimSpace(text)
}
