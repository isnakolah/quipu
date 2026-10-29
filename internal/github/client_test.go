package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestAuthTokenPrefersEnvironment(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", " test-token ")
	if got := authToken(); got != "test-token" {
		t.Fatalf("authToken() = %q, want environment token", got)
	}
}

func TestSetIssueStatePatchesCanonicalEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/repos/o/r/issues/7" {
			t.Fatalf("request %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, HTTP: server.Client()}
	if err := client.SetIssueState(context.Background(), "o", "r", 7, "closed"); err != nil {
		t.Fatal(err)
	}
	if err := client.SetIssueState(context.Background(), "o", "r", 7, "invalid"); err == nil {
		t.Fatal("expected invalid state rejection")
	}
}

func TestNewClientDoesNotPersistToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	client := NewClient()
	if client.Token != "test-token" {
		t.Fatalf("client token = %q", client.Token)
	}
	if _, err := os.Stat(".quipu-token"); !os.IsNotExist(err) {
		t.Fatalf("token cache was created: %v", err)
	}
}

func TestListCommentsPaginates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "1" {
			comments := make([]Comment, 100)
			for i := range comments {
				comments[i].ID = int64(i + 1)
			}
			_ = json.NewEncoder(w).Encode(comments)
			return
		}
		_ = json.NewEncoder(w).Encode([]Comment{{ID: 101}})
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, HTTP: server.Client()}
	comments, err := client.ListComments(context.Background(), "octo", "repo", 1)
	if err != nil || len(comments) != 101 || comments[100].ID != 101 {
		t.Fatalf("comments=%d err=%v", len(comments), err)
	}
}
