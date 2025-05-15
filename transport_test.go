package mirageecs_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	mirageecs "github.com/acidlemon/mirage-ecs/v2"
)

func TestRoundTrip(t *testing.T) {
	tests := []struct {
		name              string
		serverDelay       time.Duration
		timeout           time.Duration
		wantStatus        int
		wantBody          string
		bodyContains      string
		requireAuthCookie bool
		sendCookie        bool
	}{
		{
			name:        "Success pattern",
			serverDelay: 50 * time.Millisecond,
			timeout:     100 * time.Millisecond,
			wantStatus:  http.StatusOK,
			wantBody:    "OK",
		},
		{
			name:         "Timeout failure pattern",
			serverDelay:  150 * time.Millisecond,
			timeout:      100 * time.Millisecond,
			wantStatus:   http.StatusGatewayTimeout,
			bodyContains: "test-subdomain upstream timeout: ",
		},
		{
			name:              "Success pattern with auth cookie",
			timeout:           100 * time.Millisecond,
			wantStatus:        http.StatusOK,
			wantBody:          "OK",
			requireAuthCookie: true,
			sendCookie:        true,
		},
		{
			name:              "Forbidden pattern with auth cookie",
			timeout:           100 * time.Millisecond,
			wantStatus:        http.StatusForbidden,
			wantBody:          "Forbidden",
			requireAuthCookie: true,
			sendCookie:        false,
		},
		{
			name:       "Large content",
			timeout:    100 * time.Second,
			wantStatus: http.StatusOK,
			wantBody:   strings.Repeat("a", 1024*100),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Setup mock server
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				time.Sleep(tt.serverDelay)
				buf := strings.NewReader(tt.wantBody)
				io.Copy(w, buf)
			}))
			defer server.Close()

			// Setup transport
			tr := &mirageecs.Transport{
				Counter:   mirageecs.NewAccessCounter(time.Second),
				Transport: mirageecs.NewHTTPTransport(tt.timeout),
				Subdomain: "test-subdomain",
			}
			if tt.requireAuthCookie {
				tr.AuthCookieValidateFunc = func(c *http.Cookie) error {
					if c.Value == "ok" {
						return nil
					}
					return fmt.Errorf("invalid cookie value: %s", c.Value)
				}
			}

			req, _ := http.NewRequest("GET", server.URL, nil)
			if tt.sendCookie {
				req.AddCookie(&http.Cookie{
					Name:  "mirage-ecs-auth",
					Value: "ok",
				})
			}

			resp, err := tr.RoundTrip(req)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			defer resp.Body.Close()

			buf := new(strings.Builder)
			if n, err := io.Copy(buf, resp.Body); err != nil {
				t.Fatalf("unexpected error: %v", err)
			} else {
				t.Logf("read body %d bytes", n)
			}
			body := buf.String()

			if resp.StatusCode != tt.wantStatus {
				t.Errorf("wanted status %v, got %v", tt.wantStatus, resp.StatusCode)
			}

			if tt.bodyContains != "" {
				if !strings.Contains(string(body), tt.bodyContains) {
					t.Errorf("wanted body to contain %v, got %v", tt.bodyContains, string(body))
				}
			} else {
				if len(tt.wantBody) != len(string(body)) {
					t.Errorf("wanted body length %v, got %v", len(tt.wantBody), len(string(body)))
				}
			}
		})
	}
}
