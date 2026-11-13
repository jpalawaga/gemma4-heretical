package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWhoopToolIsExposedOnlyWithUsableToken(t *testing.T) {
	store := testMemoryStore(t)
	client := newAPIClient(modelChoice{}, 16384, store, time.Second)
	if _, _, err := client.EnableCapability("whoop"); err != nil {
		t.Fatal(err)
	}
	if hasTool(client.completionTools(true), "get_whoop_recovery_sleep") {
		t.Fatal("Whoop tool was exposed before authorization")
	}
	if err := client.whoop.saveToken(whoopToken{
		AccessToken:  "access",
		RefreshToken: "refresh",
		ExpiresAt:    time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if !hasTool(client.completionTools(true), "get_whoop_recovery_sleep") {
		t.Fatal("Whoop tool was hidden after authorization")
	}
}

func TestWhoopInstructionsAreInjectedOnlyWhenConnected(t *testing.T) {
	store := testMemoryStore(t)
	messages, _, _, err := store.InitialSystemMessages("")
	if err != nil {
		t.Fatal(err)
	}
	if messagesContain(messages, "get_whoop_recovery_sleep") {
		t.Fatal("Whoop instructions were injected before authorization")
	}
	client := newWhoopClient(whoopConfig{TokenPath: filepath.Join(store.dir, "whoop-token.json")})
	if err := client.saveToken(whoopToken{AccessToken: "access", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	messages, _, _, err = store.InitialSystemMessages("")
	if err != nil {
		t.Fatal(err)
	}
	if !messagesContain(messages, "get_whoop_recovery_sleep") {
		t.Fatal("Whoop instructions were not injected after authorization")
	}
}

func TestWhoopRecoverySleepRetrieval(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer current-access" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		if r.URL.Query().Get("limit") != "25" || r.URL.Query().Get("start") == "" || r.URL.Query().Get("end") == "" {
			t.Errorf("query = %q", r.URL.RawQuery)
		}
		switch r.URL.Path {
		case "/recovery":
			_, _ = w.Write([]byte(`{"records":[{"cycle_id":7,"score":{"recovery_score":82}}]}`))
		case "/activity/sleep":
			_, _ = w.Write([]byte(`{"records":[{"id":"sleep-7","score":{"sleep_performance_percentage":91}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newWhoopClient(whoopConfig{TokenPath: filepath.Join(t.TempDir(), "token.json"), APIBaseURL: server.URL})
	if err := client.saveToken(whoopToken{AccessToken: "current-access", ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	result, err := client.RecoverySleep(context.Background(), 7, now)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result)
	if requests != 2 || !strings.Contains(string(encoded), `"recovery_score":82`) || !strings.Contains(string(encoded), `"sleep_performance_percentage":91`) {
		t.Fatalf("requests=%d result=%s", requests, encoded)
	}
}

func TestWhoopRefreshRotatesAndPersistsTokens(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.Form.Get("refresh_token") != "old-refresh" || r.Form.Get("client_id") != "client" || r.Form.Get("client_secret") != "secret" || r.Form.Get("scope") != "offline" {
				t.Errorf("refresh form = %#v", r.Form)
			}
			_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600,"scope":"offline read:recovery read:sleep","token_type":"bearer"}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer new-access" {
			t.Errorf("API authorization = %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"records":[]}`))
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "token.json")
	client := newWhoopClient(whoopConfig{
		ClientID: "client", ClientSecret: "secret", TokenPath: path,
		TokenURL: server.URL + "/token", APIBaseURL: server.URL,
	})
	if err := client.saveToken(whoopToken{AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: now.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.RecoverySleep(context.Background(), 1, now); err != nil {
		t.Fatal(err)
	}
	persisted, err := client.loadToken()
	if err != nil {
		t.Fatal(err)
	}
	if persisted.AccessToken != "new-access" || persisted.RefreshToken != "new-refresh" {
		t.Fatalf("persisted token = %#v", persisted)
	}
}

func TestWhoopOAuthStateHasRequiredLength(t *testing.T) {
	for range 10 {
		state, err := newWhoopOAuthState()
		if err != nil {
			t.Fatal(err)
		}
		if len(state) != 8 {
			t.Fatalf("state %q has length %d", state, len(state))
		}
	}
}

func hasTool(specs []toolSpec, name string) bool {
	for _, spec := range specs {
		if spec.Function.Name == name {
			return true
		}
	}
	return false
}

func messagesContain(messages []apiMessage, content string) bool {
	for _, message := range messages {
		if strings.Contains(message.Content.(string), content) {
			return true
		}
	}
	return false
}
