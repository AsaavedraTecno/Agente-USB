package uploader

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Sin X-Machine-Id, el backend no puede distinguir cuál notebook pregunta
// cuando varias comparten una misma Llave (el caso normal de USB) —
// resolveAgentStatusRow() les devuelve a todas la primera fila por id. Ver
// el comentario en FetchConfig.
func TestFetchConfig_EnviaMachineIdComoHeader(t *testing.T) {
	var headerRecibido string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headerRecibido = r.Header.Get("X-Machine-Id")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"active":true,"scan_interval":900}`))
	}))
	defer server.Close()

	_, err := FetchConfig(server.URL+"/telemetry", "llave-de-prueba", "machine-id-1234", false)
	if err != nil {
		t.Fatalf("FetchConfig devolvió error: %v", err)
	}

	if headerRecibido != "machine-id-1234" {
		t.Errorf("esperaba X-Machine-Id=%q, llegó %q", "machine-id-1234", headerRecibido)
	}
}

func TestFetchConfig_SinMachineIdNoMandaElHeader(t *testing.T) {
	headerPresente := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, headerPresente = r.Header["X-Machine-Id"]
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"active":true,"scan_interval":900}`))
	}))
	defer server.Close()

	_, err := FetchConfig(server.URL+"/telemetry", "llave-de-prueba", "", false)
	if err != nil {
		t.Fatalf("FetchConfig devolvió error: %v", err)
	}

	if headerPresente {
		t.Errorf("no debería mandar X-Machine-Id vacío como header")
	}
}
