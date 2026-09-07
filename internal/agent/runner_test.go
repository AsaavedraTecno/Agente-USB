package agent

import "testing"

// El bug real que esto prueba: hp_samsung.go mandaba p.Printer.Status =
// "normal" (vocabulario de salud del dispositivo de HP), y el backend solo
// reconoce online/offline/warning/error para online_status -- "normal"
// caía en el default y el panel mostraba "Desconocido" con la impresora
// perfectamente online.
func TestNormalizeStatus(t *testing.T) {
	casos := map[string]string{
		"":              "unknown",
		"online":        "online",
		"normal":        "online", // el caso real que rompía
		"Idle":          "online",
		"Printing":      "online",
		"Ready":         "online",
		"offline":       "offline",
		"Offline":       "offline",
		"warning":       "warning",
		"Warming Up":    "warning",
		"error":         "error",
		"critical":      "error", // hp_samsung.go usa "critical", no "error"
		"Paper Jam":     "error",
		"No Toner":      "error",
		"algo-random-x": "unknown", // no inventa online para lo que no reconoce
	}

	for in, want := range casos {
		if got := normalizeStatus(in); got != want {
			t.Errorf("normalizeStatus(%q) = %q, want %q", in, got, want)
		}
	}
}
