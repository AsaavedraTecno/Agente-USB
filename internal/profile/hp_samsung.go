package profile

import (
	"fmt"
	"strings"
	"time"

	"usb-agent/internal/config"
	"usb-agent/internal/discovery"
	"usb-agent/internal/hpprotocol"
	"usb-agent/internal/payload"
	"usb-agent/internal/samsung"
	"usb-agent/internal/usbraw"
)

type HPSamsungProfile struct{}

func init() {
	Register(&HPSamsungProfile{})
}

func (h *HPSamsungProfile) Name() string {
	return "HP/Samsung RAW Protocol"
}

func (h *HPSamsungProfile) Match(printer discovery.USBPrinter) bool {
	upperName := strings.ToUpper(printer.Name)
	if strings.Contains(upperName, "HP ") && strings.Contains(upperName, "LASER") {
		return true
	}
	return false
}

func (h *HPSamsungProfile) Extract(printer discovery.USBPrinter, cfg *config.Config, p *payload.Payload, lg Logger) (bool, error) {
	lg.Logf("  Iniciando extracción con perfil: %s", h.Name())

	paths, err := usbraw.FindDevicePaths()
	if err != nil || len(paths) == 0 {
		return false, fmt.Errorf("no se encontraron rutas de dispositivo USB: %w", err)
	}

	var targetPath string
	for _, path := range paths {
		if strings.Contains(strings.ToLower(path), "vid_03f0") {
			targetPath = path
			break
		}
	}

	if targetPath == "" {
		return false, fmt.Errorf("no se encontró una impresora HP conectada en las rutas USB")
	}

	samsung.PrepareSamsungUSB()
	defer samsung.RestoreSpooler()

	serial := "Desconocido"
	respSerial, err := samsung.SendSamsungCommand(targetPath, 0x49, false)
	if err == nil && len(respSerial) > 0 {
		serial = strings.TrimRight(string(respSerial), "\x00")
	}

	if serial == "MPS-MODE" || serial == "Desconocido" || serial == "" {
		parts := strings.Split(targetPath, "#")
		if len(parts) >= 3 {
			serial = strings.ToUpper(parts[2])
		}
	}

	var fwVersion *string
	devId, err := samsung.Get1284DeviceID(targetPath)
	if err == nil && devId != "" {
		parts := strings.Split(devId, ";")
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(part, "MCV:") {
				val := strings.TrimSpace(strings.TrimPrefix(part, "MCV:"))
				if val != "" {
					fwVersion = payload.StrPtr(val)
				}
				break
			}
		}
	}

	xmlRaw, err := hpprotocol.FetchFullXML(targetPath)
	var defs []hpprotocol.StateDefinition
	if err != nil {
		lg.Logf("  ⚠ Sin XML de estado (%v) — continuando con datos crudos (sin alertas ni modelo de tóner)", err)
	} else {
		defs = hpprotocol.ParseStateDefinitions(xmlRaw)
	}

	tonerModel := "Desconocido"
	if idx := strings.Index(xmlRaw, "<Black"); idx != -1 {
		endIdx := strings.Index(xmlRaw[idx:], "</Black>")
		if endIdx != -1 {
			inner := xmlRaw[idx : idx+endIdx]
			closingBracket := strings.Index(inner, ">")
			if closingBracket != -1 {
				tonerModel = inner[closingBracket+1:]
			}
		}
	}

	statusRaw, err := samsung.SendSamsungCommand(targetPath, 0x02, false)
	if err != nil || len(statusRaw) < 8 {
		lg.Logf("  ⚠ Protocolo binario Samsung no respondió (%v) — probando EWS sobre USB (HTTP)...", err)
		return extractViaEWS(targetPath, printer, p, lg)
	}

	totalImpressions := samsung.GetTotalImpressions(targetPath)
	confidence := "hp_samsung_raw"
	if totalImpressions == -1 {
		confidence = "hp_samsung_raw_estimate"
		totalImpressions = int(statusRaw[5]) * 64
	}

	engineCycles := 0
	respCounters, err := samsung.SendSamsungCommand(targetPath, 0x0D, false)
	if err == nil && len(respCounters) >= 8 {
		engineCycles = (int(respCounters[4]) << 8) | int(respCounters[5])
	}

	panelText := samsung.GetPanelDisplay(targetPath)

	p.Printer.Brand = "HP"
	p.Printer.BrandConfidence = 1.0
	p.Printer.Model = printer.Name
	p.Printer.SerialNumber = serial
	p.Printer.Firmware = fwVersion
	if panelText != "" {
		p.Printer.Display = payload.StrPtr(panelText)
	}

	p.Source.Confidence = confidence

	if totalImpressions > 0 {
		p.Counters.Absolute.Total = payload.Int64Ptr(int64(totalImpressions))
		p.Counters.Absolute.Mono = p.Counters.Absolute.Total
		p.Counters.Absolute.Color = payload.Int64Ptr(0)
		p.Counters.Absolute.EquivalentTotal = p.Counters.Absolute.Total
		p.Counters.Absolute.EquivalentMono = p.Counters.Absolute.Total
		p.Counters.Absolute.EquivalentColor = payload.Int64Ptr(0)
	}
	if engineCycles > 0 {
		p.Counters.HardwareUsage.EngineCycles = payload.Int64Ptr(int64(engineCycles))
		p.Counters.HardwareUsage.EngineCyclesMono = p.Counters.HardwareUsage.EngineCycles
		p.Counters.HardwareUsage.EngineCyclesColor = payload.Int64Ptr(0)
	}

	now := time.Now().UTC()
	var activeErrors []hpprotocol.StateDefinition
	var activeWarnings []hpprotocol.StateDefinition

	for _, def := range defs {
		matched := true
		for i := 0; i < 8; i++ {
			if def.Mask[i] == 0x00 && statusRaw[i] != def.Expected[i] {
				matched = false
				break
			}
		}
		if matched {
			if def.GeneralMSG != "Ready" && def.GeneralMSG != "PowerSave" && def.GeneralMSG != "Printing" && def.GeneralMSG != "Waiting" {
				if def.Expected[0] == 0x84 {
					activeErrors = append(activeErrors, def)
				} else {
					activeWarnings = append(activeWarnings, def)
				}

				alertType := "other"
				if strings.Contains(def.GeneralMSG, "Jam") || strings.Contains(def.GeneralMSG, "Media") {
					alertType = "paper"
				} else if strings.Contains(def.GeneralMSG, "Supply") || strings.Contains(def.GeneralMSG, "Marker") {
					alertType = "supply"
				} else if strings.Contains(def.GeneralMSG, "Attention") || strings.Contains(def.GeneralMSG, "Error") {
					alertType = "status"
				}

				severity := "warning"
				if def.Expected[0] == 0x84 {
					severity = "critical"
				}

				msg := def.String
				if def.StandardCode != "" {
					msg = fmt.Sprintf("[%s] %s", def.StandardCode, def.String)
				}

				p.Alerts = append(p.Alerts, payload.Alert{
					ID:         fmt.Sprintf("%s-%d", def.Name, now.Unix()),
					Type:       alertType,
					Severity:   severity,
					Message:    msg,
					DetectedAt: now.Format(time.RFC3339),
				})
			}
		}
	}

	if len(activeErrors) > 0 {
		p.Printer.Status = "critical"
	} else if len(activeWarnings) > 0 {
		p.Printer.Status = "warning"
	} else {
		p.Printer.Status = "normal"
	}

	tonerPct := int(statusRaw[4])
	if tonerPct > 100 {
		tonerPct = 0
	}

	tonerStatus := "ok"
	for _, a := range p.Alerts {
		if strings.Contains(strings.ToLower(a.Message), "toner") {
			if a.Severity == "critical" {
				tonerStatus = "empty"
			} else {
				tonerStatus = "low"
			}
		}
	}

	p.Supplies = append(p.Supplies, payload.Supply{
		ID:           "slot_1_1",
		Type:         "toner",
		Color:        "black",
		Category:     "toner",
		Name:         "Toner Black",
		Description:  "Standard Black Toner",
		Model:        payload.StrPtr(tonerModel),
		IsOriginal:   payload.BoolPtr(true),
		Percentage:   payload.Float64Ptr(float64(tonerPct)),
		Status:       tonerStatus,
		IsMeasurable: true,
		SnmpTypeID:   payload.IntPtr(21),
		RawLevel:     payload.IntPtr(tonerPct),
		RawMax:       payload.IntPtr(100),
	})

	drumMax := 30000
	drumPages := totalImpressions
	if drumPages > drumMax {
		drumPages = drumMax
	}
	drumPct := int(float64(drumMax-drumPages) / float64(drumMax) * 100.0)

	p.Supplies = append(p.Supplies, payload.Supply{
		ID:           "slot_1_2",
		Type:         "drum",
		Color:        "black",
		Category:     "drum",
		Name:         "Imaging Unit",
		Description:  "Drum Unit",
		Model:        payload.StrPtr("W1332A"),
		IsOriginal:   payload.BoolPtr(true),
		Percentage:   payload.Float64Ptr(float64(drumPct)),
		Status:       "ok",
		IsMeasurable: true,
		SnmpTypeID:   payload.IntPtr(9),
		RawLevel:     payload.IntPtr(drumMax - drumPages),
		RawMax:       payload.IntPtr(drumMax),
	})

	// El protocolo binario Samsung no tiene un comando para "páginas impresas
	// con ESTE cartucho" (solo el total de vida de la impresora, ya leído
	// arriba) -- ese dato solo lo expone el Embedded Web Server de HP. Sin
	// esto, pages_with_supply queda NULL para siempre en cualquier impresora
	// donde el protocolo binario responde bien (el camino normal), y
	// "Uso/Ciclo Total" en el panel se ve vacío pese a que el agente sí tiene
	// forma de conseguir el dato -- solo que no por esta vía. Best-effort:
	// si EWS no responde, se ignora, ya tenemos todo lo demás del binario.
	enrichPagesWithSupplyViaEWS(targetPath, p, lg)

	lg.Logf("  ✓ Perfil %s ejecutado correctamente", h.Name())
	return true, nil
}

// enrichPagesWithSupplyViaEWS completa PagesWithSupply en los tóners ya
// extraídos por el protocolo binario, consultando el EWS solo por ese dato
// puntual. Timeout corto (2s): es un complemento, no puede volver lenta la
// extracción normal si el EWS no responde rápido.
func enrichPagesWithSupplyViaEWS(targetPath string, p *payload.Payload, lg Logger) {
	status, err := hpprotocol.FetchEWSStatus(targetPath, 2000)
	if err != nil {
		lg.Logf("  [i] No se pudo completar páginas-por-cartucho vía EWS (%v) — Uso/Ciclo Total quedará vacío para esta lectura", err)
		return
	}

	for i := range p.Supplies {
		if p.Supplies[i].Type != "toner" || p.Supplies[i].Color == "" {
			continue
		}
		for _, s := range status.Supplies {
			if s.PagesPrinted > 0 && strings.Contains(strings.ToLower(s.Name), p.Supplies[i].Color) {
				p.Supplies[i].PagesWithSupply = payload.IntPtr(s.PagesPrinted)
				break
			}
		}
	}
}

// extractViaEWS es el respaldo para modelos HP más nuevos (herencia Samsung pero firmware
// "onehp") cuyo pipe USB ya no habla el protocolo binario propietario, sino que expone su
// Embedded Web Server completo por HTTP sobre el mismo endpoint (visto en la LaserJet E40040).
func extractViaEWS(targetPath string, printer discovery.USBPrinter, p *payload.Payload, lg Logger) (bool, error) {
	status, err := hpprotocol.FetchEWSStatus(targetPath, 5000)
	if err != nil {
		return false, fmt.Errorf("EWS sobre USB también falló: %w", err)
	}

	p.Printer.Brand = "HP"
	p.Printer.BrandConfidence = 1.0
	p.Printer.Model = printer.Name
	if status.ModelNumber != "" {
		p.Printer.Model = fmt.Sprintf("%s (%s)", printer.Name, status.ModelNumber)
	}
	if status.Serial != "" {
		p.Printer.SerialNumber = status.Serial
	}
	if status.TotalPages > 0 {
		p.Counters.Absolute.Total = payload.Int64Ptr(status.TotalPages)
		p.Counters.Absolute.Mono = p.Counters.Absolute.Total
		p.Counters.Absolute.Color = payload.Int64Ptr(0)
	}
	if status.Location != "" {
		p.Printer.Location = payload.StrPtr(status.Location)
	}
	if status.AssetNumber != "" {
		p.Printer.AssetNumber = payload.StrPtr(status.AssetNumber)
	}
	if status.CompanyName != "" {
		p.Printer.CompanyName = payload.StrPtr(status.CompanyName)
	}
	if status.ContactPerson != "" {
		p.Printer.ContactPerson = payload.StrPtr(status.ContactPerson)
	}
	if status.EngineCycles > 0 {
		p.Counters.HardwareUsage.EngineCycles = payload.Int64Ptr(status.EngineCycles)
	}
	if status.FirmwareRevision != "" {
		fw := status.FirmwareRevision
		if status.FirmwareDatecode != "" {
			fw = fmt.Sprintf("%s (%s)", fw, status.FirmwareDatecode)
		}
		p.Printer.Firmware = payload.StrPtr(fw)
	}
	for _, t := range status.Trays {
		tray := payload.Tray{
			Name:      t.Name,
			Status:    t.Status,
			PaperSize: t.PaperSize,
		}
		if tray.Status == "" {
			tray.Status = "ok"
		}
		if t.Capacity > 0 {
			tray.Capacity = payload.IntPtr(t.Capacity)
		}
		p.Printer.Trays = append(p.Printer.Trays, tray)
	}
	if status.HasDuplex {
		p.Printer.Trays = append(p.Printer.Trays, payload.Tray{Name: "Duplex Unit", Status: "ok"})
	}
	if status.OutputBinCapacity > 0 {
		p.Printer.Trays = append(p.Printer.Trays, payload.Tray{
			Name:     "Output Bin",
			Status:   "ok",
			Capacity: payload.IntPtr(status.OutputBinCapacity),
		})
	}

	p.Printer.Status = "normal"
	if status.StatusMessage != "" && !strings.EqualFold(status.StatusIcon, "ok") {
		severity := "warning"
		if strings.EqualFold(status.StatusIcon, "error") || strings.EqualFold(status.StatusIcon, "critical") {
			severity = "critical"
			p.Printer.Status = "critical"
		} else {
			p.Printer.Status = "warning"
		}
		alertType := "other"
		lowerMsg := strings.ToLower(status.StatusMessage)
		switch {
		case strings.Contains(lowerMsg, "tray") || strings.Contains(lowerMsg, "paper") || strings.Contains(lowerMsg, "jam"):
			alertType = "paper"
		case strings.Contains(lowerMsg, "toner") || strings.Contains(lowerMsg, "cartridge") || strings.Contains(lowerMsg, "supply"):
			alertType = "supply"
		case strings.Contains(lowerMsg, "door"):
			alertType = "status"
		}
		p.Alerts = append(p.Alerts, payload.Alert{
			ID:         "alert_ews_status",
			Type:       alertType,
			Severity:   severity,
			Message:    status.StatusMessage,
			DetectedAt: time.Now().UTC().Format(time.RFC3339),
		})
	}

	for i, s := range status.Supplies {
		color := "black"
		switch {
		case strings.Contains(strings.ToLower(s.Name), "cyan"):
			color = "cyan"
		case strings.Contains(strings.ToLower(s.Name), "magenta"):
			color = "magenta"
		case strings.Contains(strings.ToLower(s.Name), "yellow"):
			color = "yellow"
		}
		tonerStatus := "ok"
		if s.Percent <= 5 {
			tonerStatus = "empty"
		} else if s.Percent <= 15 {
			tonerStatus = "low"
		}
		supply := payload.Supply{
			ID:           fmt.Sprintf("slot_1_%d", i+1),
			Type:         "toner",
			Color:        color,
			Category:     "toner",
			Name:         s.Name,
			Description:  s.Name,
			Model:        payload.StrPtr(s.PartNumber),
			IsOriginal:   payload.BoolPtr(true),
			Percentage:   payload.Float64Ptr(float64(s.Percent)),
			Status:       tonerStatus,
			IsMeasurable: true,
			SnmpTypeID:   payload.IntPtr(21),
			RawLevel:     payload.IntPtr(s.Percent),
			RawMax:       payload.IntPtr(100),
		}
		if s.PagesPrinted > 0 {
			supply.PagesWithSupply = payload.IntPtr(s.PagesPrinted)
		}
		if s.InstallDate != "" {
			supply.InstallDate = payload.StrPtr(s.InstallDate)
		}
		if s.Serial != "" {
			supply.SerialNumber = payload.StrPtr(s.Serial)
		}
		p.Supplies = append(p.Supplies, supply)
	}

	p.Source.Confidence = "hp_ews_usb"
	lg.Log("  ✓ Datos obtenidos vía EWS sobre USB (HTTP)")
	return true, nil
}
