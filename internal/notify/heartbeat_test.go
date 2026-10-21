package notify

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSendHeartbeat(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		switch r.URL.Path {
		case "/missing":
			w.WriteHeader(http.StatusNotFound)
		case "/moved":
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		}
	}))
	defer srv.Close()

	if err := SendHeartbeat(t.Context(), srv.URL+"/m123-token"); err != nil {
		t.Fatalf("SendHeartbeat = %v, want nil", err)
	}
	if gotMethod != http.MethodGet || gotPath != "/m123-token" {
		t.Fatalf("request = %s %s, want GET /m123-token", gotMethod, gotPath)
	}

	if err := SendHeartbeat(t.Context(), srv.URL+"/missing"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("SendHeartbeat on 404 = %v, want status error", err)
	}

	// Redirects are not followed, so the token never goes to another location.
	if err := SendHeartbeat(t.Context(), srv.URL+"/moved"); err == nil || gotPath != "/moved" {
		t.Fatalf("SendHeartbeat on redirect = %v (last path %s), want status error without following", err, gotPath)
	}
}

func TestSendHeartbeatErrorsOmitURL(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL + "/api/push/secret-token"
	closed.Close()

	err := SendHeartbeat(t.Context(), closedURL)
	if err == nil {
		t.Fatal("SendHeartbeat to a closed server succeeded")
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("error leaks the URL token: %v", err)
	}

	// A malformed redirect target makes net/http quote the Location in its error.
	badRedirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://example.com"+r.URL.Path+"%zz")
		w.WriteHeader(http.StatusFound)
	}))
	defer badRedirect.Close()
	err = SendHeartbeat(t.Context(), badRedirect.URL+"/api/push/secret-token")
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("SendHeartbeat on malformed redirect = %v, want error without URL", err)
	}
}
