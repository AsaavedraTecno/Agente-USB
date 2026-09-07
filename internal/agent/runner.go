package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"usb-agent/internal/bidi"
	"usb-agent/internal/config"
	"usb-agent/internal/discovery"
	"usb-agent/internal/payload"
	"usb-agent/internal/pjl"
	"usb-agent/internal/profile"
	"usb-agent/internal/queue"
	"usb-agent/internal/registryfallback"
	"usb-agent/internal/snmpusb"
	"usb-agent/internal/state"
	"usb-agent/internal/uploader"
	"usb-agent/internal/wmiprinter"
)

const Version = "1.1.0"

// Run executes the full USB agent cycle. Each output line is sent to logFn.
// Special prefixes used for GUI parsing:
//   - "__JSON__:{...}"    — the final payload JSON
//   - "__STATUS__:success" / "__STATUS__:error:<msg>"
func Run(cfg *config.Config, logFn func(string)) error {
	lg := func(s string) { logFn(s) }
	lf := func(format string, args ...interface{}) { logFn(fmt.Sprintf(format, args...)) }

	lg("╔══════════════════════════════════════════════════════════╗")
	lf("║   Agente Portable USB  v%s  │  %s/%s              ║", Version, runtime.GOOS, runtime.GOARCH)
	lg("╚══════════════════════════════════════════════════════════╝")
	lg("")

	hostname, _ := os.Hostname()
	p := payload.New(cfg.AgentID, hostname, Version)
	p.Source.Label = cfg.ClientName
	startTime := time.Now()
	extracted := false

	// ── Paso 0: Handshake ─────────────────────────────────────────────────
	lg("▶ [0/4] Verificando conexión con el servidor...")
	if cfg.ServerURL != "" {
		if _, err := uploader.FetchConfig(cfg.ServerURL, cfg.APIKey, cfg.AgentID, cfg.SkipTLSVerify); err != nil {
			lf("  ✗ Handshake fallido: %v", err)
			lg("    El agente continuará en modo offline (encolando datos).")
		} else {
			lg("  ✓ Conexión exitosa. Agente autorizado.")
		}
	} else {
		lg("  ⚠  server_url no configurado — modo offline.")
	}

	// ── Paso 1: Detectar impresoras USB ───────────────────────────────────
	lg("")
	lg("▶ [1/4] Detectando impresoras USB conectadas...")
	printers := discovery.FindUSBPrinters()

	if len(printers) == 0 {
		lg("  ✗ No se encontraron impresoras USB.")
		lg("    Verifica que la impresora esté conectada e instalada en Windows.")
		logFn("__STATUS__:error:No se encontraron impresoras USB")
		return fmt.Errorf("no se encontraron impresoras USB")
	}

	for i, pr := range printers {
		lf("  [%d] %-40s  Puerto: %s", i+1, pr.Name, pr.PortName)
	}
	target := printers[0]
	lf("  → Usando: %s (%s)", target.Name, target.PortName)

	// ── Paso 2: Extracción con fallback ───────────────────────────────────
	lg("")
	lg("▶ [2/4] Extrayendo datos...")

	loggerObj := &profile.BasicLogger{LogFn: lg}
	profileMatched := false
	for _, prf := range profile.Registry {
		if prf.Match(target) {
			lg("  [Perfil] Compatibilidad encontrada: " + prf.Name())
			profileMatched = true
			success, err := prf.Extract(target, cfg, p, loggerObj)
			if err != nil {
				lf("  ✗ Error en perfil %s: %v", prf.Name(), err)
			}
			if success {
				extracted = true
			}
			break
		}
	}

	if !extracted {
		if profileMatched {
			lg("  [Perfil] Falló la extracción especializada. Intentando métodos genéricos como respaldo...")
		} else {
			lg("  [Perfil] No se encontró perfil especializado. Usando extracción genérica...")
		}

		// Método 1: SNMP sobre IP virtual USB
		fmt.Print("  Método 1 — SNMP sobre IP virtual USB ............. ")
		if ip, found := snmpusb.DetectVirtualIP(cfg.SNMPCommunity, cfg.TimeoutMs); found {
			lf("  Método 1 — SNMP sobre IP virtual USB ............. IP: %s", ip)
			result, err := snmpusb.Extract(ip, cfg.SNMPCommunity, cfg.TimeoutMs)
			if err == nil {
				applySnmpResult(p, result, ip)
				p.Source.Confidence = "snmp_usb"
				extracted = true
				lg("  ✓ SNMP-USB completado")
			} else {
				lf("  ✗ SNMP Error: %v", err)
			}
		} else {
			lg("  Método 1 — SNMP sobre IP virtual USB ............. no encontrada")
		}

		// Método 1.5: HP Status Monitor Proxy
		if !extracted && strings.Contains(strings.ToUpper(target.Name), "HP") {
			lg("  Método 1.5 — HP Status Monitor Proxy .............")
			hpSupplies, hpCounters, errHP := registryfallback.TryHPMonitorProxy("?")
			if errHP == nil && (len(hpSupplies) > 0 || hpCounters != nil) {
				if len(hpSupplies) > 0 {
					p.Supplies = hpSupplies
				}
				if hpCounters != nil && hpCounters.Absolute.Total != nil {
					p.Counters.Absolute.Total = hpCounters.Absolute.Total
				}
				p.Source.Confidence = "hp_monitor_proxy"
				// No marcamos 'extracted = true' para que PJL igual se ejecute y obtenga
				// el Modelo y Número de Serie real de la impresora.
				lg(fmt.Sprintf("  ✓ HP Proxy completado (%d suministros)", len(hpSupplies)))
			} else {
				lg("  ✗ HP Proxy — sin datos útiles")
			}
		}

		// Método 2: PJL directo
		if !extracted {
			lg("  Método 2 — PJL via Win32 Spooler API .............")
			result, err := pjl.Extract(target.Name, target.PortName, cfg.TimeoutMs, cfg.BypassSpooler)
			if err != nil {
				lf("  ✗ PJL Error: %v", err)
			} else if result.Model == "" && result.PageCount == 0 && len(result.Supplies) == 0 && result.Status == "" {
				lg("  ✗ PJL — sin datos útiles")
			} else {
				applyPJLResult(p, result)
				extracted = true
				lf("  ✓ PJL completado (%s)", result.Confidence)
			}
		}

		// Método 3: MSFT Bidi API
		if p.Source.Confidence != "pjl_full" && p.Source.Confidence != "snmp_usb" {
			lg("  Método 3 — MSFT Bidi API (C# Hybrid) .............")
			bidiRes, bidiErr := bidi.Extract(target.Name)
			if bidiErr != nil {
				lf("  ✗ Bidi Error: %v", bidiErr)
			} else {
				applyBidiResult(p, bidiRes)
				extracted = true
				lg("  ✓ Bidi completado")
			}
		}

		// Método 4: WMI
		lg("  Método 4 — WMI Win32_Printer .....................")
		wmiRes, wmiErr := wmiprinter.Extract(target.Name)
		if wmiErr != nil {
			lf("  ✗ WMI Error: %v", wmiErr)
		} else {
			lf("  ✓ WMI completado (estado: %s)", wmiRes.Status)
			applyWMIResult(p, wmiRes, extracted)
			if !extracted {
				p.Source.Confidence = "wmi_basic"
				extracted = true
			}
		}

		// Método 5: Registry Fallback
		if len(p.Supplies) == 0 {
			lg("  Método 5 — Status Monitor Registry Fallback ......")
			regSupplies, regErr := registryfallback.Extract(target.Name)
			if regErr == nil && len(regSupplies) > 0 {
				p.Supplies = regSupplies
				if p.Source.Confidence == "wmi_basic" {
					p.Source.Confidence = "wmi_registry_hybrid"
				}
				lf("  ✓ Registry (%d suministros)", len(regSupplies))
			} else {
				if regErr != nil {
					lf("  ✗ Registry Error: %v", regErr)
				} else {
					lg("  ✗ Registry — sin datos")
				}
			}
		}
		// Fin de extracción genérica
	}

	if !extracted {
		lg("")
		lg("  ⚠  Todos los métodos fallaron. El payload estará vacío.")
		p.Source.Confidence = "failed"
	}

	// ── Paso 3: Finalizar payload ──────────────────────────────────────────
	lg("")
	lg("▶ [3/4] Construyendo payload...")

	p.Metrics.Polling.LastPollAt = time.Now().UTC().Format(time.RFC3339)
	p.CollectedAt = p.Metrics.Polling.LastPollAt
	p.Metrics.Polling.PollDurationMs = time.Since(startTime).Milliseconds()
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
	p.Counters.Confidence = p.Source.Confidence

	if p.Counters.Absolute.Total != nil && p.Counters.LogicalMatrix.ByMode.Duplex != nil {
		simplex := *p.Counters.Absolute.Total - *p.Counters.LogicalMatrix.ByMode.Duplex
		p.Counters.LogicalMatrix.ByMode.Simplex = payload.Int64Ptr(simplex)
		p.Counters.Absolute.Mono = p.Counters.Absolute.Total
		p.Counters.Absolute.Color = payload.Int64Ptr(0)
	}

	if p.Printer.Hostname == "" {
		p.Printer.Hostname = p.Source.Hostname + "_usb_host"
	}

	// Cada perfil (hp_samsung.go, epson.go, WMI, SNMP-sobre-USB) escribe
	// p.Printer.Status con su propio vocabulario -- HP manda "normal"/
	// "critical", WMI manda el string crudo de Windows ("Idle", "Paper
	// Jam"...). El backend solo reconoce online/offline/warning/error
	// (columna printers.online_status, mapeada 1:1 desde este campo en
	// ProcessTelemetryJob.php); cualquier otro valor cae en "Desconocido"
	// en el panel aunque la impresora esté perfectamente online. Un solo
	// punto de normalización acá, no un fix por perfil -- AgenteSNMP ya
	// resuelve el mismo problema así (ver extractPrinterStatus en
	// pkg/telemetry/builder.go).
	p.Printer.Status = normalizeStatus(p.Printer.Status)

	// Delta de estado
	printerIDClean := strings.ReplaceAll(p.Printer.ID, ":", "")
	stateDir := cfg.ResolveDir(cfg.StateDir)
	prevState, errState := state.LoadState(stateDir, printerIDClean)
	if errState != nil {
		lf("  [State] Error cargando caché previo: %v", errState)
	} else if prevState != nil {
		delta, isReset := state.CalculateDelta(prevState, p)
		p.Counters.Delta = delta
		if isReset {
			p.Counters.ResetDetected = payload.BoolPtr(true)
		}
	} else {
		lf("  [State] Primera lectura para %s (no hay delta aún)", printerIDClean)
	}

	if extracted {
		if err := state.SaveState(stateDir, printerIDClean, p); err != nil {
			lf("  [State] Error guardando caché: %v", err)
		}
	}

	jsonBytes, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		logFn("__STATUS__:error:Error serializando payload")
		return fmt.Errorf("serializar payload: %w", err)
	}

	// Emitir JSON para el GUI (prefijo especial)
	logFn("__JSON__:" + string(jsonBytes))

	// ── Paso 4: Enviar o encolar ───────────────────────────────────────────
	lg("")
	lg("▶ [4/4] Enviando datos al servidor...")

	queueDir := cfg.ResolveDir(cfg.QueueDir)

	if cfg.ServerURL == "" {
		lg("  [!] server_url vacío — guardando en cola local.")
	} else {
		lg("  ✓ Agregando reporte a la cola para el procesador de lotes.")
	}
	saveToQueue(queueDir, jsonBytes, lg)

	retryQueue(cfg, queueDir, lg)

	lg("")
	lf("  Duración total: %v", time.Since(startTime).Round(time.Millisecond))
	lg("═══════════════════════════════════════════════════════════")
	lg("  Proceso completado.")
	logFn("__STATUS__:success")

	return nil
}

// normalizeStatus traduce el vocabulario de estado de cualquier perfil (HP
// EWS: normal/warning/critical; WMI: el string crudo de Windows, ej. "Idle",
// "Paper Jam"; SNMP-sobre-USB: ya viene normalizado) al mismo vocabulario de
// conectividad que espera el backend: online/offline/warning/error/unknown.
// Un desconocido no es "unknown" de mentira -- si no matchea nada, es mejor
// no inventar online, así que también cae en unknown.
func normalizeStatus(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))

	switch s {
	case "":
		return "unknown"
	case "online", "normal", "idle", "printing", "ready", "ok", "processing":
		return "online"
	case "offline":
		return "offline"
	case "warning":
		return "warning"
	case "error", "critical":
		return "error"
	}

	switch {
	case strings.Contains(s, "jam"), strings.Contains(s, "critical"), strings.Contains(s, "no toner"), strings.Contains(s, "door open"):
		return "error"
	case strings.Contains(s, "offline"), strings.Contains(s, "disconnect"):
		return "offline"
	case strings.Contains(s, "error"):
		return "error"
	case strings.Contains(s, "warn"), strings.Contains(s, "warm"), strings.Contains(s, "low"):
		return "warning"
	}

	return "unknown"
}

// ── Aplicadores de resultado ──────────────────────────────────────────────────

func applySnmpResult(p *payload.Payload, r *snmpusb.Result, ip string) {
	p.Printer.Brand = r.Brand
	p.Printer.Model = r.Model
	p.Printer.SerialNumber = r.Serial
	p.Printer.IP = payload.StrPtr(ip)
	if r.MAC != "" {
		p.Printer.MACAddress = payload.StrPtr(r.MAC)
	}
	if r.Display != "" {
		p.Printer.Display = payload.StrPtr(r.Display)
	}
	if r.TotalPages > 0 {
		p.Counters.Absolute.Total = payload.Int64Ptr(r.TotalPages)
	}
	p.Supplies = r.Supplies
	for _, a := range r.Alerts {
		p.Alerts = append(p.Alerts, payload.Alert{
			ID:         "alert_snmp",
			Type:       "device",
			Severity:   "warning",
			Message:    a.Message,
			DetectedAt: time.Now().UTC().Format(time.RFC3339),
		})
	}
}

func applyPJLResult(p *payload.Payload, r *pjl.Result) {
	p.Printer.Brand = r.Brand
	p.Printer.Model = r.Model
	p.Printer.SerialNumber = r.Serial
	p.Printer.BrandConfidence = 1.0
	if r.PrinterHostname != "" {
		p.Printer.Hostname = r.PrinterHostname
	}
	if len(r.Trays) > 0 {
		p.Printer.Trays = r.Trays
	}
	if r.PageCount > 0 {
		p.Counters.Absolute.Total = payload.Int64Ptr(r.PageCount)
		if r.MonoPages > 0 || r.ColorPages > 0 {
			if r.MonoPages > 0 {
				p.Counters.Absolute.Mono = payload.Int64Ptr(r.MonoPages)
			}
			if r.ColorPages > 0 {
				p.Counters.Absolute.Color = payload.Int64Ptr(r.ColorPages)
			}
		} else {
			p.Counters.Absolute.Mono = payload.Int64Ptr(r.PageCount)
		}
	}
	if r.PrintPages > 0 {
		p.Counters.LogicalMatrix.ByFunction.Print = payload.Int64Ptr(r.PrintPages)
	}
	if r.PrintMono > 0 {
		p.Counters.LogicalMatrix.ByFunction.PrintMono = payload.Int64Ptr(r.PrintMono)
	}
	if r.PrintColor > 0 {
		p.Counters.LogicalMatrix.ByFunction.PrintColor = payload.Int64Ptr(r.PrintColor)
	}
	if r.CopyPages > 0 {
		p.Counters.LogicalMatrix.ByFunction.Copy = payload.Int64Ptr(r.CopyPages)
	}
	if r.CopyMono > 0 {
		p.Counters.LogicalMatrix.ByFunction.CopyMono = payload.Int64Ptr(r.CopyMono)
	}
	if r.CopyColor > 0 {
		p.Counters.LogicalMatrix.ByFunction.CopyColor = payload.Int64Ptr(r.CopyColor)
	}
	if r.ReportsPages > 0 {
		p.Counters.LogicalMatrix.ByFunction.Reports = payload.Int64Ptr(r.ReportsPages)
	}
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
	if r.Display != "" {
		p.Printer.Display = payload.StrPtr(r.Display)
	}
	if r.Firmware != "" && p.Printer.Firmware == nil {
		p.Printer.Firmware = payload.StrPtr(r.Firmware)
	}
	if r.UptimeMinutes > 0 {
		p.Metrics.UptimeSeconds = payload.Int64Ptr(r.UptimeMinutes * 60)
	}
	if r.PowerOnCount > 0 {
		p.Metrics.PowerOnCount = payload.Int64Ptr(r.PowerOnCount)
	}
	if r.EngineCycles > 0 {
		p.Counters.HardwareUsage.EngineCycles = payload.Int64Ptr(r.EngineCycles)
	}
	if r.Confidence == "pjl_brother_custom" {
		p.Metrics.UptimeSeconds = payload.Int64Ptr(r.UptimeMinutes * 60)
		p.Metrics.PowerOnCount = payload.Int64Ptr(r.PowerOnCount)
		p.Counters.HardwareUsage.JamTotal = payload.Int64Ptr(r.JamTotal)
		p.Counters.HardwareUsage.JamTray1 = payload.Int64Ptr(r.JamTray1)
		p.Counters.HardwareUsage.JamTray2 = payload.Int64Ptr(r.JamTray2)
		p.Counters.HardwareUsage.JamTrayMP = payload.Int64Ptr(r.JamTrayMP)
		p.Counters.HardwareUsage.JamInside = payload.Int64Ptr(r.JamInside)
		p.Counters.HardwareUsage.JamRear = payload.Int64Ptr(r.JamRear)
	}
	if r.CoverageAvg > 0 {
		p.Counters.CoverageAvg = payload.Float64Ptr(r.CoverageAvg)
	}
	if len(r.Supplies) > 0 {
		p.Supplies = r.Supplies
	}
	for _, a := range r.Alerts {
		p.Alerts = append(p.Alerts, payload.Alert{
			ID:         "alert_pjl",
			Type:       "device",
			Severity:   "warning",
			Message:    a.Message,
			DetectedAt: time.Now().UTC().Format(time.RFC3339),
		})
	}
	// Solo sobreescribir confidence si PJL trajo datos reales o si era fallido
	if r.Confidence != "pjl_basic" || p.Source.Confidence == "" || p.Source.Confidence == "failed" {
		p.Source.Confidence = r.Confidence
	}
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
		p.Alerts = append(p.Alerts, payload.Alert{
			ID:         "alert_wmi",
			Type:       "device",
			Severity:   "warning",
			Message:    a.Message,
			DetectedAt: time.Now().UTC().Format(time.RFC3339),
		})
	}
	if p.Printer.Status == "" && r.Status != "" && r.Status != "Unknown" {
		p.Printer.Status = strings.ToLower(r.Status)
	}
}

// ── Cola local ────────────────────────────────────────────────────────────────

func saveToQueue(queueDir string, data []byte, lg func(string)) {
	if err := queue.Save(queueDir, data); err != nil {
		lg(fmt.Sprintf("  ✗ Error guardando en cola: %v", err))
	} else {
		lg(fmt.Sprintf("  ✓ Guardado en cola: %s", queueDir))
	}
}

func retryQueue(cfg *config.Config, queueDir string, lg func(string)) {
	if cfg.ServerURL == "" {
		return
	}
	queue.Purge(queueDir, 72*time.Hour)
	files, err := queue.ListPending(queueDir)
	if err != nil || len(files) == 0 {
		return
	}
	lg(fmt.Sprintf("  [Queue] %d archivo(s) pendiente(s) — procesando en lotes...", len(files)))

	batchSize := 50
	for i := 0; i < len(files); i += batchSize {
		end := i + batchSize
		if end > len(files) {
			end = len(files)
		}

		batchFiles := files[i:end]
		var batchData [][]byte

		for _, f := range batchFiles {
			data, err := os.ReadFile(f)
			if err == nil {
				batchData = append(batchData, data)
			}
		}

		deleteFiles, err := uploader.SendBatch(cfg.ServerURL, cfg.APIKey, cfg.SkipTLSVerify, batchData)

		if deleteFiles {
			// Borramos los archivos (ya sea por éxito 2xx o por data corrupta 4xx)
			for _, f := range batchFiles {
				queue.Remove(f)
			}
			if err != nil {
				lg(fmt.Sprintf("  ✗ Lote %d-%d descartado por error 4xx: %v", i+1, end, err))
			} else {
				lg(fmt.Sprintf("  ✓ Lote %d-%d enviado y limpiado", i+1, end))
			}
		} else {
			// Backoff: dejamos los archivos y abortamos el procesamiento de esta iteración
			lg(fmt.Sprintf("  ✗ Lote %d-%d falló (se reintentará luego): %v", i+1, end, err))
			break
		}
	}
}
