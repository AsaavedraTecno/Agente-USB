package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"usb-agent/internal/bidi"
	"usb-agent/internal/config"
	"usb-agent/internal/discovery"
	"usb-agent/internal/payload"
	"usb-agent/internal/pjl"
	"usb-agent/internal/queue"
	"usb-agent/internal/registryfallback"
	"usb-agent/internal/snmpusb"
	"usb-agent/internal/uploader"
	"usb-agent/internal/wmiprinter"
)

const version = "1.0.0"

func main() {
	log.SetFlags(log.Ltime)
	log.SetPrefix("[USB-Agent] ")

	fmt.Println("╔══════════════════════════════════════════════════════════╗")
	fmt.Printf("║   Agente Portable USB  v%s  │  %s/%s         ║\n", version, runtime.GOOS, runtime.GOARCH)
	fmt.Println("╚══════════════════════════════════════════════════════════╝")
	fmt.Println()

	cfg := config.Load("agent.yaml")
	hostname, _ := os.Hostname()
	p := payload.New(cfg.AgentID, hostname, version)
	startTime := time.Now()

	extracted := false

	// ── Paso 1: Detectar impresoras USB ────────────────────────────────────
	fmt.Println("▶ [1/4] Detectando impresoras USB conectadas...")
	printers := discovery.FindUSBPrinters()

	if len(printers) == 0 {
		fmt.Println("  ✗ No se encontraron impresoras USB.")
		fmt.Println("    Verifica que la impresora esté conectada e instalada en Windows.")
		os.Exit(1)
	}

	for i, pr := range printers {
		fmt.Printf("  [%d] %-40s  Puerto: %s\n", i+1, pr.Name, pr.PortName)
	}

	target := printers[0]
	fmt.Printf("\n  → Usando: %s (%s)\n\n", target.Name, target.PortName)

	// ── Paso 2: Extracción con fallback ────────────────────────────────────
	fmt.Println("▶ [2/4] Extrayendo datos...")



	// Método 1: SNMP sobre IP virtual USB (solo si el driver crea adaptador de red)
	fmt.Print("  Método 1 — SNMP sobre IP virtual USB ............. ")
	if ip, found := snmpusb.DetectVirtualIP(cfg.SNMPCommunity, cfg.TimeoutMs); found {
		fmt.Printf("IP: %s\n", ip)
		result, err := snmpusb.Extract(ip, cfg.SNMPCommunity, cfg.TimeoutMs)
		if err == nil {
			applySnmpResult(p, result, ip)
			p.Source.Confidence = "snmp_usb"
			extracted = true
			fmt.Println("  ✓ SNMP-USB completado")
		} else {
			fmt.Printf("  ✗ Error: %v\n", err)
		}
	} else {
		fmt.Println("no encontrada (driver no crea adaptador virtual)")
	}

	// Método 2: PJL directo via Win32 Spooler API
	if !extracted {
		fmt.Print("  Método 2 — PJL via Win32 Spooler API ............. ")
		result, err := pjl.Extract(target.Name, target.PortName, cfg.TimeoutMs, cfg.BypassSpooler)
		if err != nil {
			fmt.Printf("✗\n             %v\n", err)
		} else if result.Model == "" && result.PageCount == 0 && len(result.Supplies) == 0 && result.Status == "" {
			fmt.Println("sin datos útiles")
		} else {
			applyPJLResult(p, result)
			extracted = true
			fmt.Printf("✓  (%s)\n", result.Confidence)
		}
	}

	// Método 3: MSFT Bidi API (Puente C# In-Memory)
	// Intentar si PJL no fue completo (pjl_full) o si falló
	if p.Source.Confidence != "pjl_full" && p.Source.Confidence != "snmp_usb" {
		fmt.Print("  Método 3 — MSFT Bidi API (C# Hybrid) ............. ")
		bidiRes, bidiErr := bidi.Extract(target.Name)
		if bidiErr != nil {
			fmt.Printf("✗ %v\n", bidiErr)
		} else {
			applyBidiResult(p, bidiRes)
			extracted = true
			fmt.Println("✓")
		}
	}

	// Método 4: WMI (siempre disponible — datos básicos del driver de Windows)
	// Se ejecuta siempre para complementar con modelo y alertas si falta info
	fmt.Print("  Método 4 — WMI Win32_Printer ..................... ")
	wmiRes, wmiErr := wmiprinter.Extract(target.Name)
	if wmiErr != nil {
		fmt.Printf("✗ %v\n", wmiErr)
	} else {
		fmt.Printf("✓  (estado: %s)\n", wmiRes.Status)
		applyWMIResult(p, wmiRes, extracted)
		if !extracted {
			p.Source.Confidence = "wmi_basic"
			extracted = true
		}
	}

	// Método 5: Registry Fallback (Solo si aún no tenemos niveles de suministros)
	if len(p.Supplies) == 0 {
		fmt.Print("  Método 5 — Status Monitor Registry Fallback ...... ")
		regSupplies, regErr := registryfallback.Extract(target.Name)
		if regErr == nil && len(regSupplies) > 0 {
			p.Supplies = regSupplies
			// Si solo teníamos WMI básico, subimos el confidence a wmi_registry_hybrid
			if p.Source.Confidence == "wmi_basic" {
				p.Source.Confidence = "wmi_registry_hybrid"
			}
			fmt.Printf("✓ (%d suministros)\n", len(regSupplies))
		} else {
			if regErr != nil {
				fmt.Printf("✗ %v\n", regErr)
			} else {
				fmt.Println("✗ sin datos")
			}
		}
	}

	if !extracted {
		fmt.Println("\n  ⚠  Todos los métodos fallaron. El payload estará vacío.")
		p.Source.Confidence = "failed"
	}

	// ── Paso 3: Finalizar payload ──────────────────────────────────────────
	fmt.Println("\n▶ [3/4] Construyendo payload...")

	p.Metrics.Polling.LastPollAt = time.Now().UTC().Format(time.RFC3339)
	p.CollectedAt = p.Metrics.Polling.LastPollAt
	p.Metrics.Polling.PollDurationMs = time.Since(startTime).Milliseconds()
	// success_rate can be calculated based on whether any extracted data exists
	if extracted {
		p.Metrics.Polling.OidSuccessRate = 1.0
	} else {
		p.Metrics.Polling.OidSuccessRate = 0.0
	}
	if p.Printer.SerialNumber != "" {
		p.Printer.ID = p.Printer.SerialNumber
	} else if p.Printer.MACAddress != nil && *p.Printer.MACAddress != "" {
		p.Printer.ID = *p.Printer.MACAddress
	} else {
		p.Printer.ID = "UNKNOWN"
	}

	p.EventID = fmt.Sprintf("%s::%s::%d", p.Source.AgentID, p.Printer.ID, time.Now().Unix())

	// Counters: derive simplex = total - duplex
	p.Counters.Confidence = p.Source.Confidence
	if p.Counters.Absolute.Total != nil && p.Counters.LogicalMatrix.ByMode.Duplex != nil {
		simplex := *p.Counters.Absolute.Total - *p.Counters.LogicalMatrix.ByMode.Duplex
		p.Counters.LogicalMatrix.ByMode.Simplex = payload.Int64Ptr(simplex)
		// Also correct mono = total (this is a mono-only printer, color is always 0)
		p.Counters.Absolute.Mono = p.Counters.Absolute.Total
		p.Counters.Absolute.Color = payload.Int64Ptr(0)
	}

	// Printer hostname: usar el de la impresora (derivado de MAC), fallback a PC hostname
	if p.Printer.Hostname == "" {
		p.Printer.Hostname = p.Source.Hostname + "_usb_host"
	}

	jsonBytes, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		log.Fatalf("Error serializando payload: %v", err)
	}

	fmt.Println()
	fmt.Println(string(jsonBytes))

	// ── Paso 4: Enviar o encolar ───────────────────────────────────────────
	fmt.Println("\n▶ [4/4] Enviando datos al servidor...")

	if cfg.ServerURL == "" {
		fmt.Println("  [!] server_url vacío en agent.yaml — guardando en cola local.")
		saveToQueue(cfg, jsonBytes)
	} else {
		if err := uploader.Send(cfg.ServerURL, cfg.APIKey, jsonBytes); err != nil {
			fmt.Printf("  ✗ Error enviando: %v\n  → Guardando en cola local.\n", err)
			saveToQueue(cfg, jsonBytes)
		} else {
			fmt.Println("  ✓ Datos enviados correctamente.")
		}
	}

	retryQueue(cfg)

	fmt.Println()
	fmt.Printf("  Duración total: %v\n", time.Since(startTime).Round(time.Millisecond))
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Println("  Proceso completado.")
}

// ── Aplicadores de resultado ───────────────────────────────────────────────

func applySnmpResult(p *payload.Payload, r *snmpusb.Result, ip string) {
	p.Printer.Brand = r.Brand
	p.Printer.Model = r.Model
	p.Printer.SerialNumber = r.Serial
	p.Printer.IP = payload.StrPtr(ip)
	if r.MAC != "" {
		p.Printer.MACAddress = payload.StrPtr(r.MAC)
	}
	if r.TotalPages > 0 {
		p.Counters.Absolute.Total = payload.Int64Ptr(r.TotalPages)
	}
	p.Supplies = r.Supplies
	// Map Alerts to string
	for _, a := range r.Alerts {
		p.DeviceAlerts = append(p.DeviceAlerts, a.Message)
	}
}

func applyPJLResult(p *payload.Payload, r *pjl.Result) {
	p.Printer.Brand = r.Brand
	p.Printer.Model = r.Model
	p.Printer.SerialNumber = r.Serial
	// La impresora reporta su marca directamente vía PJL → confianza total
	p.Printer.BrandConfidence = 1.0
	if r.PrinterHostname != "" {
		p.Printer.Hostname = r.PrinterHostname
	}
	if len(r.Trays) > 0 {
		p.Printer.Trays = r.Trays
	}
	if r.PageCount > 0 {
		p.Counters.Absolute.Total = payload.Int64Ptr(r.PageCount)
		p.Counters.Absolute.Mono = payload.Int64Ptr(r.PageCount)
	}
	if r.PrintPages > 0 {
		p.Counters.LogicalMatrix.ByFunction.Print = payload.Int64Ptr(r.PrintPages)
	}
	if r.CopyPages > 0 {
		p.Counters.LogicalMatrix.ByFunction.Copy = payload.Int64Ptr(r.CopyPages)
	}
	// Duplex: usar DuplexSet para saber si el dato fue recibido (puede ser 0)
	if r.DuplexSet {
		p.Counters.LogicalMatrix.ByMode.Duplex = payload.Int64Ptr(r.DuplexPages)
	} else if r.DuplexPages > 0 {
		p.Counters.LogicalMatrix.ByMode.Duplex = payload.Int64Ptr(r.DuplexPages)
	}
	if r.ScanPages > 0 {
		p.Counters.HardwareUsage.TotalScans = payload.Int64Ptr(r.ScanPages)
	}
	if r.CoverageLast > 0 {
		p.Counters.CoverageLast = payload.Float64Ptr(r.CoverageLast)
	}
	if r.MAC != "" && p.Printer.MACAddress == nil {
		p.Printer.MACAddress = payload.StrPtr(r.MAC)
	}
	if r.IP != "" && p.Printer.IP == nil {
		p.Printer.IP = payload.StrPtr(r.IP)
	}
	if r.Online {
		p.Printer.Status = "online"
	} else if r.Status != "" {
		p.Printer.Status = strings.ToLower(r.Status)
	}
	if r.Firmware != "" && p.Printer.Firmware == nil {
		p.Printer.Firmware = payload.StrPtr(r.Firmware)
	}
	if r.UptimeMinutes > 0 {
		uptimeSec := r.UptimeMinutes * 60
		p.Metrics.UptimeSeconds = payload.Int64Ptr(uptimeSec)
	}
	if r.PowerOnCount > 0 {
		p.Metrics.PowerOnCount = payload.Int64Ptr(r.PowerOnCount)
	}
	if r.EngineCycles > 0 {
		p.Counters.HardwareUsage.EngineCycles = payload.Int64Ptr(r.EngineCycles)
	}
	// Métricas Brother propietarias: se escriben siempre (incluso como 0)
	// cuando se recibió BRSUPPLY — mismo patrón que los jam counters.
	if r.Confidence == "pjl_brother_custom" {
		p.Metrics.UptimeSeconds = payload.Int64Ptr(r.UptimeMinutes * 60)
		p.Metrics.PowerOnCount  = payload.Int64Ptr(r.PowerOnCount)
		p.Counters.HardwareUsage.JamTotal  = payload.Int64Ptr(r.JamTotal)
		p.Counters.HardwareUsage.JamTray1  = payload.Int64Ptr(r.JamTray1)
		p.Counters.HardwareUsage.JamTray2  = payload.Int64Ptr(r.JamTray2)
		p.Counters.HardwareUsage.JamTrayMP = payload.Int64Ptr(r.JamTrayMP)
		p.Counters.HardwareUsage.JamInside = payload.Int64Ptr(r.JamInside)
		p.Counters.HardwareUsage.JamRear   = payload.Int64Ptr(r.JamRear)
	}
	if r.CoverageAvg > 0 {
		p.Counters.CoverageAvg = payload.Float64Ptr(r.CoverageAvg)
	}
	p.Supplies = r.Supplies
	for _, a := range r.Alerts {
		p.DeviceAlerts = append(p.DeviceAlerts, a.Message)
	}
	p.Source.Confidence = r.Confidence
}

func applyBidiResult(p *payload.Payload, r *bidi.Result) {
	if (p.Printer.SerialNumber == "" || p.Printer.SerialNumber == "?") && r.Serial != "" {
		p.Printer.SerialNumber = r.Serial
	}
	if p.Counters.Absolute.Total == nil && r.PageCount > 0 {
		p.Counters.Absolute.Total = payload.Int64Ptr(r.PageCount)
	}
	if len(r.Supplies) > 0 {
		p.Supplies = r.Supplies
	}
	p.Source.Confidence = "bidi_hybrid"
}

// applyWMIResult complementa el payload con datos WMI.
// Si primary=false, llena campos vacíos; si primary=true, solo agrega alertas que falten.
func applyWMIResult(p *payload.Payload, r *wmiprinter.Result, primary bool) {
	if !primary {
		p.Printer.Brand = r.Brand
		p.Printer.Model = r.Name
	}
	if (p.Printer.SerialNumber == "" || p.Printer.SerialNumber == "?") && r.Serial != "" {
		p.Printer.SerialNumber = r.Serial
	}
	if p.Counters.Absolute.Total == nil && r.PagesThisSession > 50 {
		p.Counters.Absolute.Total = payload.Int64Ptr(r.PagesThisSession)
	}
	for _, a := range r.Alerts {
		p.DeviceAlerts = append(p.DeviceAlerts, a.Message)
	}
	if p.Printer.Status == "" && r.Status != "" && r.Status != "Unknown" {
		p.Printer.Status = strings.ToLower(r.Status)
	}
}

// ── Cola local ────────────────────────────────────────────────────────────

func saveToQueue(cfg *config.Config, data []byte) {
	if err := queue.Save(cfg.QueueDir, data); err != nil {
		log.Printf("Error guardando en cola: %v", err)
	} else {
		fmt.Printf("  ✓ Guardado en %s\n", cfg.QueueDir)
	}
}

func retryQueue(cfg *config.Config) {
	if cfg.ServerURL == "" {
		return
	}
	files, err := queue.ListPending(cfg.QueueDir)
	if err != nil || len(files) == 0 {
		return
	}
	fmt.Printf("\n  [Queue] %d archivo(s) pendiente(s) — reintentando...\n", len(files))
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if err := uploader.Send(cfg.ServerURL, cfg.APIKey, data); err != nil {
			fmt.Printf("  ✗ %s: %v\n", filepath.Base(f), err)
		} else {
			fmt.Printf("  ✓ %s enviado\n", filepath.Base(f))
			queue.Remove(f)
		}
	}
}
