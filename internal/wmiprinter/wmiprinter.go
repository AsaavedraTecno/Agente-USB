// Package wmiprinter extrae datos básicos de impresora via Win32_Printer (WMI).
// No requiere SNMP ni PJL bidireccional. Siempre disponible si el driver está instalado.
// Útil como fallback para obtener modelo, estado y alertas básicas.
package wmiprinter

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"usb-agent/internal/payload"
)

// Result contiene datos extraídos de WMI.
type Result struct {
	Name                       string
	Brand                      string
	Status                     string
	Serial                     string
	PrinterState               uint32
	DetectedErrorState         uint32
	ExtendedDetectedErrorState uint32
	// PagesThisSession es el total de páginas impresas desde el último inicio del spooler.
	// Fuente: Win32_PerfRawData_Spooler_PrintQueue.TotalPagesPrinted
	PagesThisSession int64
	Alerts           []payload.Alert
}

// ExtendedDetectedErrorState values (Win32_Printer)
const (
	edesNoError         = 1
	edesLowPaper        = 2
	edesNoPaper         = 3
	edesLowToner        = 4
	edesNoToner         = 5
	edesOutOfMemory     = 6
	edesDoorOpen        = 7
	edesServerUnknown   = 8
	edesJammed          = 9
	edesOffline         = 10
	edesServiceReq      = 11
	edesOutputBinFull   = 12
	edesPaperProblem    = 13
	edesCannotPrintPage = 14
	edesUserInterv      = 15
	edesOutOfSvc        = 16
	edesOther           = 18
)

// Extract consulta Win32_Printer para el nombre de impresora dado.
func Extract(printerName string) (*Result, error) {
	script := fmt.Sprintf(`
$pName = '%s'
$p = Get-WmiObject Win32_Printer | Where-Object { $_.Name -eq $pName }
if ($p -eq $null) { Write-Output 'null'; exit }

$q = Get-WmiObject -Class Win32_PerfRawData_Spooler_PrintQueue -ErrorAction SilentlyContinue |
     Where-Object { $_.Name -eq $pName -or $_.Name -like "$pName (*)" } |
     Select-Object -First 1 -ExpandProperty TotalPagesPrinted
$pages = if ($q) { [int64]$q } else { 0 }

@{
  Name                       = [string]$p.Name
  Status                     = [string]$p.Status
  PNPDeviceID                = [string]$p.PNPDeviceID
  PrinterState               = [int]$p.PrinterState
  DetectedErrorState         = [int]$p.DetectedErrorState
  ExtendedDetectedErrorState = [int]$p.ExtendedDetectedErrorState
  PagesThisSession           = $pages
} | ConvertTo-Json -Compress`, strings.ReplaceAll(printerName, "'", "''"))

	psCmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	psCmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := psCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("WMI query: %w", err)
	}

	raw := strings.TrimSpace(string(out))
	if raw == "" || raw == "null" {
		return nil, fmt.Errorf("impresora %q no encontrada en WMI", printerName)
	}

	var wmiData struct {
		Name                       string `json:"Name"`
		Status                     string `json:"Status"`
		PNPDeviceID                string `json:"PNPDeviceID"`
		PrinterState               uint32 `json:"PrinterState"`
		DetectedErrorState         uint32 `json:"DetectedErrorState"`
		ExtendedDetectedErrorState uint32 `json:"ExtendedDetectedErrorState"`
		PagesThisSession           int64  `json:"PagesThisSession"`
	}

	if err := json.Unmarshal([]byte(raw), &wmiData); err != nil {
		return nil, fmt.Errorf("parsear respuesta WMI: %w", err)
	}

	res := &Result{
		Name:                       wmiData.Name,
		Brand:                      inferBrand(wmiData.Name),
		Status:                     wmiData.Status,
		PrinterState:               wmiData.PrinterState,
		DetectedErrorState:         wmiData.DetectedErrorState,
		ExtendedDetectedErrorState: wmiData.ExtendedDetectedErrorState,
	}

	// Extraer Serial desde PNPDeviceID (e.g. SWC\SAMSUNG_ML-371X_SER\7&1234567&0&01 -> 1234567)
	// O USB\VID_04E8&PID_331B\Z78RBJBD20000BL -> Z78RBJBD20000BL
	if wmiData.PNPDeviceID != "" {
		parts := strings.Split(wmiData.PNPDeviceID, "\\")
		if len(parts) > 0 {
			last := parts[len(parts)-1]
			// Limpiar si tiene &
			if idx := strings.LastIndex(last, "&"); idx != -1 {
				// A veces el serial es la parte antes de la última & o similar
				// Pero en USB puro es la última parte
				res.Serial = last
			} else {
				res.Serial = last
			}
		}
	}

	res.Alerts = decodeAlerts(wmiData.ExtendedDetectedErrorState)

	// Contador de páginas del spooler (desde último reinicio del servicio)
	res.PagesThisSession = wmiData.PagesThisSession

	return res, nil
}

// decodeAlerts convierte el campo ExtendedDetectedErrorState en alertas legibles.
func decodeAlerts(state uint32) []payload.Alert {
	var alerts []payload.Alert

	messages := map[uint32]struct {
		msg   string
		level string
	}{
		edesLowPaper:        {"Papel bajo", "warning"},
		edesNoPaper:         {"Sin papel", "critical"},
		edesLowToner:        {"Tóner bajo", "warning"},
		edesNoToner:         {"Sin tóner", "critical"},
		edesOutOfMemory:     {"Memoria insuficiente", "warning"},
		edesDoorOpen:        {"Puerta abierta", "critical"},
		edesJammed:          {"Atasco de papel", "critical"},
		edesOffline:         {"Impresora offline", "warning"},
		edesServiceReq:      {"Requiere servicio", "critical"},
		edesOutputBinFull:   {"Bandeja de salida llena", "warning"},
		edesPaperProblem:    {"Problema de papel", "warning"},
		edesCannotPrintPage: {"No puede imprimir página", "critical"},
		edesUserInterv:      {"Requiere intervención del usuario", "warning"},
	}

	if entry, ok := messages[state]; ok {
		alerts = append(alerts, payload.Alert{
			ID:         fmt.Sprintf("WMI_%d", state),
			Type:       "device",
			Severity:   entry.level,
			Message:    entry.msg,
			DetectedAt: time.Now().UTC().Format(time.RFC3339),
		})
	}

	return alerts
}

func inferBrand(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "samsung"):
		return "Samsung"
	case strings.Contains(n, "hp") || strings.Contains(n, "hewlett"):
		return "HP"
	case strings.Contains(n, "xerox"):
		return "Xerox"
	case strings.Contains(n, "canon"):
		return "Canon"
	case strings.Contains(n, "epson"):
		return "Epson"
	case strings.Contains(n, "brother"):
		return "Brother"
	case strings.Contains(n, "ricoh"):
		return "Ricoh"
	case strings.Contains(n, "lexmark"):
		return "Lexmark"
	case strings.Contains(n, "kyocera"):
		return "Kyocera"
	case strings.Contains(n, "konica"):
		return "Konica Minolta"
	default:
		return "Unknown"
	}
}
