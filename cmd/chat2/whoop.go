package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	whoopAuthorizationURL = "https://api.prod.whoop.com/oauth/oauth2/auth"
	whoopTokenURL         = "https://api.prod.whoop.com/oauth/oauth2/token"
	whoopAPIBaseURL       = "https://api.prod.whoop.com/developer/v2"
	whoopScopes           = "offline read:recovery read:sleep"
)

type whoopConfig struct {
	ClientID     string
	ClientSecret string
	TokenPath    string
	AuthURL      string
	TokenURL     string
	APIBaseURL   string
}

type whoopClient struct {
	config whoopConfig
	http   *http.Client
	mu     sync.Mutex
}

type whoopToken struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	TokenType    string    `json:"token_type,omitempty"`
	Scope        string    `json:"scope,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
}

type whoopTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	ExpiresIn    int    `json:"expires_in"`
}

type whoopHTTPError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *whoopHTTPError) Error() string {
	if e.Body == "" {
		return "Whoop returned " + e.Status
	}
	return fmt.Sprintf("Whoop returned %s: %s", e.Status, e.Body)
}

func whoopConfigFromEnvironment(dataDir string) whoopConfig {
	return whoopConfig{
		ClientID:     strings.TrimSpace(os.Getenv("WHOOP_CLIENT_ID")),
		ClientSecret: strings.TrimSpace(os.Getenv("WHOOP_CLIENT_SECRET")),
		TokenPath:    filepath.Join(dataDir, "whoop-token.json"),
		AuthURL:      whoopAuthorizationURL,
		TokenURL:     whoopTokenURL,
		APIBaseURL:   whoopAPIBaseURL,
	}
}

func newWhoopClient(config whoopConfig) *whoopClient {
	if config.AuthURL == "" {
		config.AuthURL = whoopAuthorizationURL
	}
	if config.TokenURL == "" {
		config.TokenURL = whoopTokenURL
	}
	if config.APIBaseURL == "" {
		config.APIBaseURL = whoopAPIBaseURL
	}
	return &whoopClient{
		config: config,
		http:   &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *whoopClient) Configured(now time.Time) bool {
	token, err := c.loadToken()
	if err != nil || token.AccessToken == "" {
		return false
	}
	if token.ExpiresAt.After(now.Add(30 * time.Second)) {
		return true
	}
	return token.RefreshToken != "" && c.config.ClientID != "" && c.config.ClientSecret != ""
}

func (c *whoopClient) RecoverySleep(ctx context.Context, days int, now time.Time) (map[string]any, error) {
	if days < 1 || days > 30 {
		return nil, errors.New("days must be from 1 to 30")
	}
	if strings.TrimSpace(c.config.TokenPath) == "" {
		return nil, errors.New("the Whoop token path is not configured")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	token, err := c.validToken(ctx, now, false)
	if err != nil {
		return nil, err
	}
	start := now.AddDate(0, 0, -days)
	recovery, recoveryErr := c.getCollection(ctx, token.AccessToken, "/recovery", start, now)
	sleep, sleepErr := c.getCollection(ctx, token.AccessToken, "/activity/sleep", start, now)
	if isWhoopUnauthorized(recoveryErr) || isWhoopUnauthorized(sleepErr) {
		token, err = c.validToken(ctx, now, true)
		if err != nil {
			return nil, err
		}
		recovery, recoveryErr = c.getCollection(ctx, token.AccessToken, "/recovery", start, now)
		sleep, sleepErr = c.getCollection(ctx, token.AccessToken, "/activity/sleep", start, now)
	}
	if recoveryErr != nil {
		return nil, fmt.Errorf("retrieve recovery records: %w", recoveryErr)
	}
	if sleepErr != nil {
		return nil, fmt.Errorf("retrieve sleep records: %w", sleepErr)
	}
	return map[string]any{
		"start":      start.Format(time.RFC3339),
		"end":        now.Format(time.RFC3339),
		"recoveries": recovery,
		"sleeps":     sleep,
	}, nil
}

func (c *whoopClient) getCollection(ctx context.Context, accessToken, path string, start, end time.Time) (map[string]any, error) {
	records := make([]any, 0)
	nextToken := ""
	for page := 0; page < 8; page++ {
		payload, err := c.getCollectionPage(ctx, accessToken, path, start, end, nextToken)
		if err != nil {
			return nil, err
		}
		pageRecords, ok := payload["records"].([]any)
		if !ok {
			return nil, errors.New("Whoop collection response did not contain a records array")
		}
		records = append(records, pageRecords...)
		nextToken, _ = payload["next_token"].(string)
		if nextToken == "" {
			return map[string]any{"records": records}, nil
		}
	}
	return map[string]any{
		"records":    records,
		"next_token": nextToken,
		"truncated":  true,
	}, nil
}

func (c *whoopClient) getCollectionPage(ctx context.Context, accessToken, path string, start, end time.Time, nextToken string) (map[string]any, error) {
	endpoint, err := url.Parse(strings.TrimSuffix(c.config.APIBaseURL, "/") + path)
	if err != nil {
		return nil, err
	}
	query := endpoint.Query()
	query.Set("limit", "25")
	query.Set("start", start.Format(time.RFC3339))
	query.Set("end", end.Format(time.RFC3339))
	if nextToken != "" {
		query.Set("nextToken", nextToken)
	}
	endpoint.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, readWhoopHTTPError(resp)
	}
	var payload map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode Whoop response: %w", err)
	}
	return payload, nil
}

func isWhoopUnauthorized(err error) bool {
	var apiErr *whoopHTTPError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnauthorized
}

func (c *whoopClient) validToken(ctx context.Context, now time.Time, forceRefresh bool) (whoopToken, error) {
	token, err := c.loadToken()
	if err != nil {
		if os.IsNotExist(err) {
			return whoopToken{}, errors.New("Whoop is not connected; run ./chat2 --whoop-auth after configuring the ngrok callback")
		}
		return whoopToken{}, fmt.Errorf("load Whoop token: %w", err)
	}
	if !forceRefresh && token.AccessToken != "" && token.ExpiresAt.After(now.Add(30*time.Second)) {
		return token, nil
	}
	if token.RefreshToken == "" {
		return whoopToken{}, errors.New("the Whoop session cannot be refreshed; run ./chat2 --whoop-auth again")
	}
	if c.config.ClientID == "" || c.config.ClientSecret == "" {
		return whoopToken{}, errors.New("WHOOP_CLIENT_ID and WHOOP_CLIENT_SECRET are required to refresh the Whoop connection")
	}
	values := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {token.RefreshToken},
		"client_id":     {c.config.ClientID},
		"client_secret": {c.config.ClientSecret},
		"scope":         {"offline"},
	}
	fresh, err := c.requestToken(ctx, values)
	if err != nil {
		return whoopToken{}, fmt.Errorf("refresh Whoop access: %w", err)
	}
	if fresh.RefreshToken == "" {
		fresh.RefreshToken = token.RefreshToken
	}
	if err := c.saveToken(fresh); err != nil {
		return whoopToken{}, fmt.Errorf("save refreshed Whoop token: %w", err)
	}
	return fresh, nil
}

func (c *whoopClient) exchangeAuthorizationCode(ctx context.Context, code, redirectURL string) error {
	values := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {c.config.ClientID},
		"client_secret": {c.config.ClientSecret},
		"redirect_uri":  {redirectURL},
	}
	token, err := c.requestToken(ctx, values)
	if err != nil {
		return err
	}
	if token.RefreshToken == "" {
		return errors.New("Whoop did not return a refresh token; confirm the app allows the offline scope")
	}
	return c.saveToken(token)
}

func (c *whoopClient) requestToken(ctx context.Context, values url.Values) (whoopToken, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.TokenURL, strings.NewReader(values.Encode()))
	if err != nil {
		return whoopToken{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return whoopToken{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return whoopToken{}, readWhoopHTTPError(resp)
	}
	var result whoopTokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return whoopToken{}, fmt.Errorf("decode token response: %w", err)
	}
	if result.AccessToken == "" || result.ExpiresIn <= 0 {
		return whoopToken{}, errors.New("Whoop returned an incomplete token response")
	}
	return whoopToken{
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
		TokenType:    result.TokenType,
		Scope:        result.Scope,
		ExpiresAt:    time.Now().Add(time.Duration(result.ExpiresIn) * time.Second),
	}, nil
}

func readWhoopHTTPError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	return &whoopHTTPError{
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Body:       strings.TrimSpace(string(body)),
	}
}

func (c *whoopClient) loadToken() (whoopToken, error) {
	file, err := os.Open(c.config.TokenPath)
	if err != nil {
		return whoopToken{}, err
	}
	defer file.Close()
	var token whoopToken
	if err := json.NewDecoder(io.LimitReader(file, 1<<20)).Decode(&token); err != nil {
		return whoopToken{}, err
	}
	return token, nil
}

func (c *whoopClient) saveToken(token whoopToken) error {
	dir := filepath.Dir(c.config.TokenPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(dir, ".whoop-token-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if err := json.NewEncoder(temporary).Encode(token); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, c.config.TokenPath)
}

func runWhoopAuthFlow(ctx context.Context, client *whoopClient, cfg appConfig) error {
	if client.config.ClientID == "" || client.config.ClientSecret == "" {
		return errors.New("set WHOOP_CLIENT_ID and WHOOP_CLIENT_SECRET in the environment")
	}
	redirect, err := url.Parse(cfg.WhoopRedirectURL)
	if err != nil || redirect.Scheme != "https" || redirect.Host == "" || redirect.Path == "" || redirect.RawQuery != "" || redirect.Fragment != "" {
		return errors.New("--whoop-redirect-url must be the public ngrok HTTPS callback URL without a query or fragment")
	}
	if err := validateLoopbackListenAddress(cfg.WhoopCallbackListen); err != nil {
		return err
	}
	state, err := newWhoopOAuthState()
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", cfg.WhoopCallbackListen)
	if err != nil {
		return fmt.Errorf("listen for callback on %s: %w", cfg.WhoopCallbackListen, err)
	}
	defer listener.Close()

	flowCtx, cancel := context.WithTimeout(ctx, cfg.WhoopAuthTimeout)
	defer cancel()
	result := make(chan error, 1)
	mux := http.NewServeMux()
	mux.HandleFunc(redirect.Path, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		receivedState := r.URL.Query().Get("state")
		if len(receivedState) != len(state) || subtle.ConstantTimeCompare([]byte(receivedState), []byte(state)) != 1 {
			http.Error(w, "invalid OAuth state", http.StatusBadRequest)
			return
		}
		if oauthError := strings.TrimSpace(r.URL.Query().Get("error")); oauthError != "" {
			description := strings.TrimSpace(r.URL.Query().Get("error_description"))
			if description != "" {
				oauthError += ": " + description
			}
			http.Error(w, "Whoop authorization was not completed.", http.StatusBadRequest)
			select {
			case result <- errors.New(oauthError):
			default:
			}
			return
		}
		code := strings.TrimSpace(r.URL.Query().Get("code"))
		if code == "" {
			http.Error(w, "missing authorization code", http.StatusBadRequest)
			return
		}
		err := client.exchangeAuthorizationCode(flowCtx, code, cfg.WhoopRedirectURL)
		if err != nil {
			http.Error(w, "Could not finish Whoop authorization. Return to the terminal for details.", http.StatusBadGateway)
		} else {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "<!doctype html><title>Whoop connected</title><p>Whoop is connected to chat2. You can close this window.</p>")
		}
		select {
		case result <- err:
		default:
		}
	})
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       15 * time.Second,
	}
	serverErrors := make(chan error, 1)
	go func() {
		serverErrors <- server.Serve(listener)
	}()

	authorize, err := url.Parse(client.config.AuthURL)
	if err != nil {
		return err
	}
	query := authorize.Query()
	query.Set("response_type", "code")
	query.Set("client_id", client.config.ClientID)
	query.Set("redirect_uri", cfg.WhoopRedirectURL)
	query.Set("scope", whoopScopes)
	query.Set("state", state)
	authorize.RawQuery = query.Encode()

	fmt.Printf("Waiting for Whoop on http://%s%s\n", cfg.WhoopCallbackListen, redirect.Path)
	fmt.Println("Open this URL to authorize chat2:")
	fmt.Println(authorize.String())
	if err := openCameraWindow(authorize.String()); err != nil {
		return fmt.Errorf("open the Whoop authorization page: %w", err)
	}

	var flowErr error
	select {
	case flowErr = <-result:
	case serveErr := <-serverErrors:
		if !errors.Is(serveErr, http.ErrServerClosed) {
			flowErr = serveErr
		}
	case <-flowCtx.Done():
		flowErr = fmt.Errorf("timed out waiting for the callback after %s", cfg.WhoopAuthTimeout)
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer shutdownCancel()
	_ = server.Shutdown(shutdownCtx)
	if flowErr != nil {
		return flowErr
	}
	fmt.Printf("Whoop connected. Token saved to %s\n", client.config.TokenPath)
	return nil
}

func runWhoopAuthWithNgrok(ctx context.Context, client *whoopClient, cfg appConfig) error {
	redirect, err := url.Parse(cfg.WhoopRedirectURL)
	if err != nil || redirect.Scheme != "https" || redirect.Host == "" {
		return errors.New("set WHOOP_REDIRECT_URL (or --whoop-redirect-url) to the registered ngrok HTTPS callback URL")
	}
	command, stopped, err := startWhoopNgrok(ctx, cfg.WhoopCallbackListen, redirect.Scheme+"://"+redirect.Host)
	if err != nil {
		return err
	}
	defer stopWhoopNgrok(command, stopped)
	return runWhoopAuthFlow(ctx, client, cfg)
}

func startWhoopNgrok(ctx context.Context, localAddress, publicOrigin string) (*exec.Cmd, <-chan error, error) {
	path, err := exec.LookPath("ngrok")
	if err != nil {
		return nil, nil, errors.New("ngrok is not installed or is not on PATH")
	}
	command := exec.CommandContext(
		ctx,
		path,
		"http",
		localAddress,
		"--url", publicOrigin,
		"--log=stdout",
		"--log-format=json",
	)
	command.Env = whoopSafeChildEnvironment()
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	command.Stderr = command.Stdout
	if err := command.Start(); err != nil {
		return nil, nil, fmt.Errorf("start ngrok: %w", err)
	}
	started := make(chan string, 1)
	stopped := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 16<<10), 1<<20)
		var lastError string
		for scanner.Scan() {
			var event struct {
				Level string `json:"lvl"`
				Msg   string `json:"msg"`
				URL   string `json:"url"`
				Err   string `json:"err"`
			}
			if json.Unmarshal(scanner.Bytes(), &event) != nil {
				continue
			}
			if event.Level == "eror" || event.Level == "crit" {
				lastError = strings.TrimSpace(event.Err)
				if lastError == "" {
					lastError = strings.TrimSpace(event.Msg)
				}
			}
			if event.Msg == "started tunnel" && event.URL != "" {
				select {
				case started <- event.URL:
				default:
				}
			}
		}
		waitErr := command.Wait()
		if waitErr != nil && lastError != "" {
			waitErr = fmt.Errorf("%s: %w", lastError, waitErr)
		}
		stopped <- waitErr
	}()

	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	select {
	case tunnelURL := <-started:
		if !strings.EqualFold(strings.TrimSuffix(tunnelURL, "/"), strings.TrimSuffix(publicOrigin, "/")) {
			stopWhoopNgrok(command, stopped)
			return nil, nil, fmt.Errorf("ngrok opened %s instead of configured endpoint %s", tunnelURL, publicOrigin)
		}
		return command, stopped, nil
	case waitErr := <-stopped:
		if waitErr == nil {
			waitErr = errors.New("ngrok exited before opening a tunnel")
		}
		return nil, nil, waitErr
	case <-timer.C:
		stopWhoopNgrok(command, stopped)
		return nil, nil, errors.New("timed out waiting for ngrok to open the Whoop callback tunnel")
	case <-ctx.Done():
		stopWhoopNgrok(command, stopped)
		return nil, nil, ctx.Err()
	}
}

func whoopSafeChildEnvironment() []string {
	result := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if name == "WHOOP_CLIENT_SECRET" {
			continue
		}
		result = append(result, entry)
	}
	return result
}

func stopWhoopNgrok(command *exec.Cmd, stopped <-chan error) {
	if command == nil || command.Process == nil {
		return
	}
	_ = command.Process.Signal(os.Interrupt)
	select {
	case <-stopped:
		return
	case <-time.After(2 * time.Second):
		_ = command.Process.Kill()
		select {
		case <-stopped:
		case <-time.After(time.Second):
		}
	}
}

func validateLoopbackListenAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil || port == "" {
		return errors.New("--whoop-callback-listen must be a loopback host and port such as 127.0.0.1:8787")
	}
	if !strings.EqualFold(host, "localhost") {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return errors.New("--whoop-callback-listen must bind to loopback; ngrok should provide public forwarding")
		}
	}
	return nil
}

func newWhoopOAuthState() (string, error) {
	bytes := make([]byte, 6)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate OAuth state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}
