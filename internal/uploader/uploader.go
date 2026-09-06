package uploader

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// SendBatch envía un conjunto de archivos JSON empaquetados bajo la clave "readings".
// Retorna deleteFiles=true si el servidor responde 2xx o 4xx (datos corruptos).
// Retorna deleteFiles=false si hay error de red o 5xx/429 (backoff).
func SendBatch(serverURL, apiKey string, skipTLS bool, batch [][]byte) (bool, error) {
	if len(batch) == 0 {
		return true, nil
	}

	// Construir el payload JSON unificado
	var rawJSONs []json.RawMessage
	for _, data := range batch {
		rawJSONs = append(rawJSONs, json.RawMessage(data))
	}

	payload := map[string]interface{}{
		"readings": rawJSONs,
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return true, fmt.Errorf("error serializando batch: %w", err) // Data corrupta local, borrar
	}

	// Firma HMAC SHA-256
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	signature := ""
	if apiKey != "" {
		h := sha256.New()
		h.Write([]byte(apiKey))
		hashedKey := hex.EncodeToString(h.Sum(nil))

		mac := hmac.New(sha256.New, []byte(hashedKey))
		mac.Write(bodyBytes)
		mac.Write([]byte(timestamp))
		signature = hex.EncodeToString(mac.Sum(nil))
	}

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: skipTLS},
	}
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: tr,
	}

	req, err := http.NewRequest("POST", serverURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return false, fmt.Errorf("construir request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if apiKey != "" {
		req.Header.Set("X-Agent-Key", apiKey)
		req.Header.Set("X-Timestamp", timestamp)
		req.Header.Set("X-Signature", signature)
	}

	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("POST %s: %w", serverURL, err) // Falla red, no borrar
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	// Éxito
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return true, nil
	}

	// Too Many Requests
	if resp.StatusCode == 429 {
		return false, fmt.Errorf("servidor respondió %d (Too Many Requests)", resp.StatusCode)
	}

	// Client Error (Rechazo de payload)
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return true, fmt.Errorf("datos rechazados por el servidor (4xx): status %d", resp.StatusCode)
	}

	// Server Error (5xx)
	return false, fmt.Errorf("falla del servidor (5xx): status %d", resp.StatusCode)
}

// RemoteConfig es el subconjunto de /api/agent/config que le sirve a un agente USB.
// El backend devuelve más campos (snmp_community, discovery, etc.) pensados para
// AgenteSNMP — se ignoran sin problema, Go no falla por campos JSON no mapeados.
type RemoteConfig struct {
	Active       bool `json:"active"`
	ScanInterval int  `json:"scan_interval"` // segundos
}

// FetchConfig realiza el handshake GET para obtener la configuración del agente,
// y la parsea a RemoteConfig. El caller decide qué hacer con `active`/`ScanInterval`.
//
// machineID: sin esto, el servidor no puede distinguir CUÁL notebook está
// preguntando cuando varias comparten una misma Llave (el caso normal de
// USB — una Llave por sucursal, N máquinas). resolveAgentStatusRow() del
// backend cae a devolverles a todas la primera fila por id, así que
// last_poll_at y la versión reportada solo se actualizan para una de las N
// instancias. La telemetría (POST) no tiene este problema: manda el
// machine_id real en el body (source.agent_id), no en un header.
func FetchConfig(baseURL, apiKey, machineID string, skipTLS bool) (*RemoteConfig, error) {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: skipTLS},
	}
	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: tr,
	}

	// Mejor construirlo explícitamente si sabemos el patrón
	handshakeURL := strings.Replace(baseURL, "/telemetry", "/config", 1)

	req, err := http.NewRequest("GET", handshakeURL, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/json")
	if apiKey != "" {
		req.Header.Set("X-Agent-Key", apiKey)
	}
	if machineID != "" {
		req.Header.Set("X-Machine-Id", machineID)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("handshake falló: status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var rc RemoteConfig
	if err := json.Unmarshal(body, &rc); err != nil {
		return nil, fmt.Errorf("parsear config: %w", err)
	}
	return &rc, nil
}
