package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

func backendCookies(ctx context.Context, baseURL, password string) ([]*http.Cookie, error) {
	if password == "" {
		return nil, nil
	}
	payload, _ := json.Marshal(map[string]string{"password": password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/auth", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("soundsvc login: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		return nil, fmt.Errorf("soundsvc login: HTTP %d", res.StatusCode)
	}
	return res.Cookies(), nil
}
