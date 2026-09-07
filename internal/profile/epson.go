package profile

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"usb-agent/internal/config"
	"usb-agent/internal/discovery"
	"usb-agent/internal/payload"
	"usb-agent/internal/samsung"
	"usb-agent/internal/usbraw"
	"usb-agent/internal/wmiprinter"
)

type EpsonProfile struct{}

func init() {
	Register(&EpsonProfile{})
}

func (e *EpsonProfile) Name() string {
	return "Epson USB RAW Bypass"
}

func (e *EpsonProfile) Match(printer discovery.USBPrinter) bool {
	upperName := strings.ToUpper(printer.Name)
	return strings.Contains(upperName, "EPSON")
}

func (e *EpsonProfile) Extract(printer discovery.USBPrinter, cfg *config.Config, p *payload.Payload, lg Logger) (bool, error) {
	lg.Logf("  Iniciando extracción con perfil: %s", e.Name())

	paths, err := usbraw.FindDevicePaths()
	if err != nil || len(paths) == 0 {
		return false, fmt.Errorf("no se encontraron rutas de dispositivo USB: %w", err)
	}

	// Buscar el path de Epson (VID 04B8)
	var targetPath string
	for _, path := range paths {
		if strings.Contains(strings.ToLower(path), "vid_04b8") {
			targetPath = path
			break
		}
	}

	if targetPath == "" {
		return false, fmt.Errorf("no se encontró una impresora Epson (VID_04B8) conectada en las rutas USB")
	}

	lg.Logf("  → Puerto RAW USB de Epson encontrado: %s", targetPath)

	// 1. Intentar obtener el Device ID estándar IEEE-1284 mediante IOCTL
	devId, err := samsung.Get1284DeviceID(targetPath)
	if err == nil && devId != "" {
		lg.Logf("  ✓ IEEE-1284 Device ID obtenido")
		snRe := regexp.MustCompile(`(?i)(SN|SERN):\s*([A-Za-z0-9]+)`)
		if match := snRe.FindStringSubmatch(devId); len(match) > 2 {
			p.Printer.SerialNumber = match[2]
			lg.Logf("  → Serial detectado por IEEE-1284: %s", p.Printer.SerialNumber)
		}
	} else {
		lg.Logf("  ✗ Error obteniendo Device ID por IOCTL: %v", err)
	}

	inkFound := false

	// 2. EXTRACCIÓN 100% PORTABLE (SIN DRIVERS): Enviar comandos EJL y BDC directamente al puerto USB.
	// Esto reemplaza el PJL genérico que falló antes, usando el protocolo real de Epson.
	lg.Log("  → Intentando comunicación directa USB (Driverless EJL/BDC + PJL)...")
	
	// 2.1 Intentar extraer el Page Count vía PJL primero, de forma aislada
	// Agregamos más comandos PJL para ver qué otra información suelta la impresora
	pjlCmd := "\x1B%-12345X@PJL\r\n" +
		"@PJL INFO PAGECOUNT\r\n" +
		"@PJL INFO VARIABLES\r\n" +
		"@PJL INFO CONFIG\r\n" +
		"@PJL INFO STATUS\r\n" +
		"\x1B%-12345X\r\n"
		
	freshPjl, drainedPjl, _ := usbraw.SendPJLAndRead(targetPath, pjlCmd, 2500, nil)
	respPjl := freshPjl
	if len(freshPjl) == 0 && len(drainedPjl) > 0 {
		respPjl = drainedPjl
	}
	
	if len(respPjl) > 0 {
		// Epson suele responder al PJL INFO PAGECOUNT directamente con el número en la siguiente línea
		pageCountRe := regexp.MustCompile(`(?i)(?:PAGECOUNT|TOTALPAGES)[^\d]*(\d+)`)
		if match := pageCountRe.FindStringSubmatch(string(respPjl)); len(match) > 1 {
			if count, err := strconv.ParseInt(match[1], 10, 64); err == nil {
				p.Counters.Absolute.Total = payload.Int64Ptr(count)
				lg.Logf("  ✓ Contador absoluto detectado por USB PJL: %d", count)
			}
		} else {
			pageCountRe2 := regexp.MustCompile(`(?i)PAGES\s*:\s*([0-9]+)`)
			if match2 := pageCountRe2.FindStringSubmatch(string(respPjl)); len(match2) > 1 {
				if count, err := strconv.ParseInt(match2[1], 10, 64); err == nil {
					p.Counters.Absolute.Total = payload.Int64Ptr(count)
					lg.Logf("  ✓ Contador absoluto detectado por USB PJL: %d", count)
				}
			}
		}
		// Extraer el Firmware Version (FIRMWARE DATECODE)
		fwRe := regexp.MustCompile(`(?i)FIRMWARE\s*DATECODE\s*=?\s*"?([A-Z0-9]+)"?`)
		if match := fwRe.FindStringSubmatch(string(respPjl)); len(match) > 1 {
			p.Printer.Firmware = payload.StrPtr(match[1])
			lg.Logf("  ✓ Firmware extraído del USB: %s", match[1])
		}
	}

	// 2.2 Enviar la petición EJL/BDC propietaria
	ejlCmd := "\x1B\x01@EJL \r\n@BDC ST2\r\n"
	fresh, drained, errRaw := usbraw.SendPJLAndRead(targetPath, ejlCmd, 3000, nil)
	respData := fresh
	if len(fresh) == 0 && len(drained) > 0 {
		respData = drained
	}

	if errRaw == nil && len(respData) > 20 {
		lg.Logf("  ✓ ¡Respuesta RAW EJL recibida desde Epson! (%d bytes)", len(respData))
		
		// Intentamos decodificar el mismo bloque binario, pero venido directo del cable
		inkFound = parseEpsonBinaryStatus(respData, p, lg)
		if inkFound {
			lg.Log("  ✓ ¡Extracción driverless exitosa (Hardware Directo)!")
		} else {
			usbraw.DumpHex("RAW USB Dump", respData)
		}
	} else {
		lg.Logf("  ✗ Sin respuesta RAW (posible bloqueo del driver o timeout).")
	}

	// 3. Consultar WMI para contadores de páginas como fallback
	if p.Counters.Absolute.Total == nil {
		lg.Log("  → Consultando a Windows (WMI) para buscar contadores...")
		wmiRes, wmiErr := wmiprinter.Extract(printer.Name)
		if wmiErr == nil {
			if wmiRes.PagesThisSession > 0 {
				p.Counters.Absolute.Total = payload.Int64Ptr(wmiRes.PagesThisSession)
			}
			if wmiRes.Status != "" && wmiRes.Status != "Unknown" {
				p.Printer.Status = strings.ToLower(wmiRes.Status)
			}
		}
	}

	// 4. Limpieza del Modelo
	cleanModel := printer.Name
	cleanRe := regexp.MustCompile(`(?i)\s*\(Copiar\s*\d+\)`)
	cleanModel = cleanRe.ReplaceAllString(cleanModel, "")
	
	p.Printer.Brand = "EPSON"
	p.Printer.BrandConfidence = 1.0
	p.Printer.Model = cleanModel
	
	if inkFound {
		p.Source.Confidence = "epson_usb_raw"
	} else {
		p.Source.Confidence = "epson_wmi_hybrid"
	}

	lg.Logf("  ✓ Perfil %s finalizado", e.Name())
	return true, nil
}

// parseEpsonBinaryStatus decodifica el arreglo de bytes de Epson
func parseEpsonBinaryStatus(val []byte, p *payload.Payload, lg Logger) bool {
	// Verificamos firma de Epson "@BDC ST2"
	if len(val) > 50 {
		
		// 1. Extraer Número de Serie (10 caracteres alfanuméricos)
		snRe := regexp.MustCompile(`([A-Z0-9]{10})`)
		matches := snRe.FindAllString(string(val), -1)
		for _, m := range matches {
			// Evitar falsos positivos como 0000000000 o FF
			if strings.ContainsAny(m, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") && strings.ContainsAny(m, "0123456789") {
				p.Printer.SerialNumber = m
				lg.Logf("  ✓ Serial Number extraído del USB: %s", m)
				break
			}
		}

		// A veces la respuesta tiene basura antes. Buscamos el @BDC
		startIndex := -1
		for i := 0; i < len(val)-4; i++ {
			if val[i] == '@' && val[i+1] == 'B' && val[i+2] == 'D' && val[i+3] == 'C' {
				startIndex = i
				break
			}
		}
		
		if startIndex != -1 {
			lg.Log("  ✓ Firma de Epson @BDC ST2 encontrada en el payload.")
			
			var black, cyan, magenta, yellow, maintenance float64
			
			// Buscamos a partir del inicio del @BDC
			for i := startIndex + 12; i < len(val)-1; i++ {
				colorIndex := val[i]
				level := float64(val[i+1])
				
				if level >= 0 && level <= 100 {
					if colorIndex == 0x01 && val[i-1] == 0x0D && black == 0 {
						black = level
					} else if colorIndex == 0x00 && val[i-1] == 0x01 && cyan == 0 {
						cyan = level
					} else if colorIndex == 0x03 && val[i-1] == 0x05 && yellow == 0 {
						yellow = level
					} else if colorIndex == 0x02 && val[i-1] == 0x04 && magenta == 0 {
						magenta = level
					}
				}
				
				// Heurística para la Caja de Mantenimiento (Maintenance Box)
				// Generalmente aparece después de la cadena "unknown$" precedida por un par de bytes fijos
				if i > 8 && val[i-8] == 'u' && val[i-7] == 'n' && val[i-6] == 'k' && val[i-5] == 'n' && val[i-4] == 'o' && val[i-3] == 'w' && val[i-2] == 'n' && val[i-1] == '$' {
					// El porcentaje suele estar 4 o 5 bytes después de unknown$
					if i+4 < len(val) {
						mLevel := float64(val[i+4])
						if mLevel > 0 && mLevel <= 100 && maintenance == 0 {
							maintenance = mLevel
						}
					}
				}
			}
			
			// Fallback de offset estático (relativo a startIndex)
			if black == 0 && len(val) > startIndex+0x24 {
				black = float64(val[startIndex+0x15])
				cyan = float64(val[startIndex+0x1E])
				yellow = float64(val[startIndex+0x21])
				magenta = float64(val[startIndex+0x24])
			}

			if black > 0 || cyan > 0 {
				p.Supplies = append(p.Supplies, payload.Supply{ID: "slot_1_ink", Name: "Negro", Model: payload.StrPtr("R04X"), Type: "ink", Color: "black", Percentage: &black, Status: "ok", RawLevel: payload.IntPtr(int(black)), RawMax: payload.IntPtr(100)})
				p.Supplies = append(p.Supplies, payload.Supply{ID: "slot_2_ink", Name: "Cian", Model: payload.StrPtr("R04L"), Type: "ink", Color: "cyan", Percentage: &cyan, Status: "ok", RawLevel: payload.IntPtr(int(cyan)), RawMax: payload.IntPtr(100)})
				p.Supplies = append(p.Supplies, payload.Supply{ID: "slot_3_ink", Name: "Magenta", Model: payload.StrPtr("R04L"), Type: "ink", Color: "magenta", Percentage: &magenta, Status: "ok", RawLevel: payload.IntPtr(int(magenta)), RawMax: payload.IntPtr(100)})
				p.Supplies = append(p.Supplies, payload.Supply{ID: "slot_4_ink", Name: "Amarillo", Model: payload.StrPtr("R04L"), Type: "ink", Color: "yellow", Percentage: &yellow, Status: "ok", RawLevel: payload.IntPtr(int(yellow)), RawMax: payload.IntPtr(100)})
				
				if maintenance > 0 {
					p.Supplies = append(p.Supplies, payload.Supply{ID: "slot_5_waste_toner", Name: "Caja de Mantenimiento", Model: payload.StrPtr("T6716"), Type: "waste_toner", Color: "other", Percentage: &maintenance, Status: "ok", RawLevel: payload.IntPtr(int(maintenance)), RawMax: payload.IntPtr(100)})
					lg.Logf("  → Niveles decodificados: N=%.0f%%, C=%.0f%%, M=%.0f%%, A=%.0f%%, Caja=%.0f%%", black, cyan, magenta, yellow, maintenance)
				} else {
					lg.Logf("  → Niveles decodificados: Negro=%.0f%%, Cian=%.0f%%, Magenta=%.0f%%, Amarillo=%.0f%%", black, cyan, magenta, yellow)
				}
				
				return true
			}
		}
	}
	return false
}
