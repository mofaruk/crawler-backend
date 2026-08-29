package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// run sends one request through the auth middleware and reports the status
// plus whether the protected handler was actually reached.
func run(t *testing.T, expected string, headers map[string]string) (int, bool) {
	t.Helper()

	reached := false
	r := gin.New()
	r.Use(APIKeyAuth(expected))
	r.GET("/sites", func(c *gin.Context) {
		reached = true
		c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/sites", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code, reached
}

// This middleware is the only thing between the public internet and an API
// that fetches user-supplied URLs server-side. Each rejection path is pinned
// individually because a single wrong branch opens the whole surface.
func TestAPIKeyAuth(t *testing.T) {
	const key = "s3cret-key"

	cases := []struct {
		name       string
		expected   string
		headers    map[string]string
		wantStatus int
		wantPass   bool
	}{
		{
			name:       "correct key in X-API-Key is accepted",
			expected:   key,
			headers:    map[string]string{"X-API-Key": key},
			wantStatus: http.StatusOK, wantPass: true,
		},
		{
			name:       "correct key as a Bearer token is accepted",
			expected:   key,
			headers:    map[string]string{"Authorization": "Bearer " + key},
			wantStatus: http.StatusOK, wantPass: true,
		},
		{
			name:       "X-API-Key wins when both headers are present",
			expected:   key,
			headers:    map[string]string{"X-API-Key": key, "Authorization": "Bearer wrong"},
			wantStatus: http.StatusOK, wantPass: true,
		},
		{
			name:       "no credentials at all is rejected",
			expected:   key,
			headers:    nil,
			wantStatus: http.StatusUnauthorized, wantPass: false,
		},
		{
			name:       "a wrong key is rejected",
			expected:   key,
			headers:    map[string]string{"X-API-Key": "wrong"},
			wantStatus: http.StatusUnauthorized, wantPass: false,
		},
		{
			name:       "an empty X-API-Key header is rejected",
			expected:   key,
			headers:    map[string]string{"X-API-Key": ""},
			wantStatus: http.StatusUnauthorized, wantPass: false,
		},
		{
			name:       "a prefix of the key is rejected",
			expected:   key,
			headers:    map[string]string{"X-API-Key": "s3cret"},
			wantStatus: http.StatusUnauthorized, wantPass: false,
		},
		{
			name:       "the key with trailing whitespace is rejected",
			expected:   key,
			headers:    map[string]string{"X-API-Key": key + " "},
			wantStatus: http.StatusUnauthorized, wantPass: false,
		},
		{
			name:       "key comparison is case-sensitive",
			expected:   key,
			headers:    map[string]string{"X-API-Key": "S3CRET-KEY"},
			wantStatus: http.StatusUnauthorized, wantPass: false,
		},
		{
			name:       "a Bearer scheme with no token is rejected",
			expected:   key,
			headers:    map[string]string{"Authorization": "Bearer "},
			wantStatus: http.StatusUnauthorized, wantPass: false,
		},
		{
			name:       "Basic auth is not accepted as a fallback",
			expected:   key,
			headers:    map[string]string{"Authorization": "Basic " + key},
			wantStatus: http.StatusUnauthorized, wantPass: false,
		},
		{
			name:       "the scheme is matched case-sensitively",
			expected:   key,
			headers:    map[string]string{"Authorization": "bearer " + key},
			wantStatus: http.StatusUnauthorized, wantPass: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, reached := run(t, tc.expected, tc.headers)
			if status != tc.wantStatus {
				t.Errorf("status = %d, want %d", status, tc.wantStatus)
			}
			if reached != tc.wantPass {
				t.Errorf("handler reached = %v, want %v", reached, tc.wantPass)
			}
		})
	}
}

// An empty configured key disables auth entirely. That is deliberate for local
// development, and cmd/api warns loudly at startup — but the behaviour must be
// exactly "allow everything", not "allow only an empty key", or a partial
// implementation would look secure while being open.
func TestAPIKeyAuthDisabledByEmptyConfig(t *testing.T) {
	cases := []map[string]string{
		nil,
		{"X-API-Key": "anything"},
		{"Authorization": "Bearer whatever"},
	}
	for _, headers := range cases {
		status, reached := run(t, "", headers)
		if status != http.StatusOK || !reached {
			t.Fatalf("empty API_KEY must disable the check; got status %d reached %v", status, reached)
		}
	}
}
