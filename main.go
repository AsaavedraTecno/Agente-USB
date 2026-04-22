package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"usb-agent/internal/bidi"
	"usb-agent/internal/config"
	"usb-agent/internal/discovery"
	"usb-agent/internal/payload"
	"usb-agent/internal/pjl"
	"usb-agent/internal/queue"
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

	extracted := false

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
		result, err := pjl.Extract(target.Name, target.PortName, cfg.TimeoutMs)
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

	if !extracted {
		fmt.Println("\n  ⚠  Todos los métodos fallaron. El payload estará vacío.")
		p.Source.Confidence = "failed"
	}

	// ── Paso 3: Finalizar payload ──────────────────────────────────────────
	fmt.Println("\n▶ [3/4] Construyendo payload...")

	endTime := time.Now()
	p.Metrics.PollCompletedAt = endTime.UTC().Format(time.RFC3339)
	p.Metrics.PollDurationMs = endTime.Sub(startTime).Milliseconds()
	if extracted {
		p.Metrics.SuccessRate = 1.0
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
	p.Printer.Serial = r.Serial
	p.Printer.IP = payload.StrPtr(ip)
	if r.MAC != "" {
		p.Printer.MAC = payload.StrPtr(r.MAC)
	}
	if r.TotalPages > 0 {
		p.Counters.TotalPages = payload.Int64Ptr(r.TotalPages)
	}
	p.Supplies = r.Supplies
	p.DeviceAlerts = r.Alerts
}

func applyPJLResult(p *payload.Payload, r *pjl.Result) {
	p.Printer.Brand = r.Brand
	p.Printer.Model = r.Model
	p.Printer.Serial = r.Serial
	if len(r.Trays) > 0 {
		p.Printer.Trays = r.Trays
	}
	if r.PageCount > 0 {
		p.Counters.TotalPages = payload.Int64Ptr(r.PageCount)
	}
	p.Supplies = r.Supplies
	p.DeviceAlerts = r.Alerts
	p.Source.Confidence = r.Confidence
}

func applyBidiResult(p *payload.Payload, r *bidi.Result) {
	if (p.Printer.Serial == "" || p.Printer.Serial == "?") && r.Serial != "" {
		p.Printer.Serial = r.Serial
	}
	if p.Counters.TotalPages == nil && r.PageCount > 0 {
		p.Counters.TotalPages = payload.Int64Ptr(r.PageCount)
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
	// Si el serial está vacío o tiene el signo '?' residual de Samsung, usar el de WMI
	if (p.Printer.Serial == "" || p.Printer.Serial == "?") && r.Serial != "" {
		p.Printer.Serial = r.Serial
	}
	// Si no hay contador de páginas de PJL/SNMP, usar el del spooler (desde último reinicio)
	if p.Counters.TotalPages == nil && r.PagesThisSession > 0 {
		p.Counters.TotalPages = payload.Int64Ptr(r.PagesThisSession)
	}
	// Siempre agregar alertas WMI que no estén ya reportadas
	for _, a := range r.Alerts {
		p.DeviceAlerts = append(p.DeviceAlerts, a)
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
