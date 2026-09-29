package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReadinessUnavailable(t *testing.T) {
	h := New(Dependencies{})
	for _, tc := range []struct {
		path string
		want int
	}{{"/health/live", http.StatusOK}, {"/health/ready", http.StatusServiceUnavailable}} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != tc.want {
			t.Errorf("%s status = %d, want %d", tc.path, rec.Code, tc.want)
		}
	}
}

func TestMeWithoutAuthorizationIsUnauthorized(t *testing.T) {
	h := New(Dependencies{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
