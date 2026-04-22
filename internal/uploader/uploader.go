package uploader

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Send hace POST del payload JSON al servidor. Retorna nil si el servidor responde 2xx.
func Send(serverURL, apiKey string, data []byte) error {
	client := &http.Client{Timeout: 15 * time.Second}

	req, err := http.NewRequest("POST", serverURL, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("construir request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", serverURL, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("servidor respondió %d", resp.StatusCode)
	}

	return nil
}
