package letterboxd

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// TestDiagnoseBlock probes why a specific URL gets blocked: it varies the
// accept-encoding header and the profile, and reports raw status + body prefix
// so we can tell a real Cloudflare challenge from a decoding artifact.
//
//	MEMENTO_LIVE=1 go test ./internal/letterboxd/ -run TestDiagnoseBlock -v
func TestDiagnoseBlock(t *testing.T) {
	if os.Getenv("MEMENTO_LIVE") != "1" {
		t.Skip("set MEMENTO_LIVE=1 to run live network tests")
	}

	url := BaseURL + "/film/koyaanisqatsi/"

	encodings := []string{
		"gzip, deflate, br",
		"gzip, deflate",
		"gzip",
		"",
	}

	for _, enc := range encodings {
		label := enc
		if label == "" {
			label = "(none)"
		}
		t.Run("accept-encoding="+label, func(t *testing.T) {
			hc, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(),
				tls_client.WithClientProfile(profiles.Chrome_152_PSK),
				tls_client.WithTimeoutSeconds(30),
				tls_client.WithCookieJar(tls_client.NewCookieJar()),
			)
			if err != nil {
				t.Fatalf("client: %v", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
			defer cancel()

			req, err := fhttp.NewRequestWithContext(ctx, fhttp.MethodGet, url, nil)
			if err != nil {
				t.Fatalf("req: %v", err)
			}
			req.Header.Set("user-agent", userAgents["chrome_152"])
			req.Header.Set("accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
			req.Header.Set("accept-language", "en-US,en;q=0.9")
			if enc != "" {
				req.Header.Set("accept-encoding", enc)
			}
			req.Header.Set("upgrade-insecure-requests", "1")
			req.Header.Set("sec-fetch-dest", "document")
			req.Header.Set("sec-fetch-mode", "navigate")
			req.Header.Set("sec-fetch-site", "none")
			req.Header.Set("sec-fetch-user", "?1")
			req.Header.Set("sec-ch-ua", chromeUAHint("152"))
			req.Header.Set("sec-ch-ua-mobile", "?0")
			req.Header.Set("sec-ch-ua-platform", platformHint)

			resp, err := hc.Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			defer resp.Body.Close()

			buf := make([]byte, 400)
			n, _ := resp.Body.Read(buf)
			prefix := strings.ReplaceAll(string(buf[:n]), "\n", " ")
			printable := true
			for _, b := range buf[:n] {
				if b < 9 || (b > 13 && b < 32) {
					printable = false
					break
				}
			}

			t.Logf("status=%d content-encoding=%q printable=%v",
				resp.StatusCode, resp.Header.Get("content-encoding"), printable)
			if printable {
				t.Logf("body prefix: %.200s", prefix)
			} else {
				t.Logf("body prefix: <binary, %d bytes read>", n)
			}
			t.Logf("challenge-detected=%v", looksLikeChallenge(buf[:n]))
		})
		time.Sleep(3 * time.Second)
	}
}
