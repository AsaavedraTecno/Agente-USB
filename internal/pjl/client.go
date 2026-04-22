// Package pjl implementa el cliente PJL (Printer Job Language) para Windows.
// Usa la Win32 Spooler API (OpenPrinter/WritePrinter/ReadPrinter) ya que los puertos
// USB001, USB002, etc. son puertos virtuales del spooler, no device objects accesibles
// directamente via \\.\USB001.
package pjl

import (
	"bytes"
	"fmt"
	"log"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"usb-agent/internal/payload"
	"usb-agent/internal/usbraw"
)

const (
	uel  = "\x1B%-12345X" // Universal Exit Language: activa modo PJL
	crlf = "\r\n"
)

// Win32 Spooler API
var (
	modWinspool          = syscall.NewLazyDLL("winspool.drv")
	procOpenPrinterW     = modWinspool.NewProc("OpenPrinterW")
	procClosePrinter     = modWinspool.NewProc("ClosePrinter")
	procStartDocPrinterW = modWinspool.NewProc("StartDocPrinterW")
	procEndDocPrinter    = modWinspool.NewProc("EndDocPrinter")
	procStartPagePrinter = modWinspool.NewProc("StartPagePrinter")
	procEndPagePrinter   = modWinspool.NewProc("EndPagePrinter")
	procWritePrinter     = modWinspool.NewProc("WritePrinter")
	procReadPrinter      = modWinspool.NewProc("ReadPrinter")
)

// docInfo1W mapea la estructura DOC_INFO_1W del Win32 API
type docInfo1W struct {
	pDocName    *uint16
	pOutputFile *uint16
	pDatatype   *uint16
}

// Result contiene los datos extraídos vía PJL.
type Result struct {
	Model      string
	Brand      string
	Serial     string
	Status     string
	Online     bool
	PageCount  int64
	Supplies   []payload.Supply
	Trays      []payload.Tray
	Alerts     []payload.Alert
	Confidence string // pjl_full | pjl_basic
}

// Extract envía comandos PJL al nombre de impresora dado.
// Prueba tres mecanismos en orden hasta obtener respuesta bidireccional:
// 1. Port monitor directo (USB001:)
// 2. Device path raw via SetupDi (comunicación directa al USB)
// 3. Job RAW via spooler + ReadPrinter (último recurso)
func Extract(printerName, portName string, timeoutMs int) (*Result, error) {
	pjlCmd := buildPJLQuery()

	// Intento 1: port monitor directo
	if res, err := tryPortMonitor(portName+":", timeoutMs); err == nil {
		return res, nil
	}

	// Intento 2: device path USB raw via SetupDi (el más confiable para lectura)
	if res, err := tryUSBRaw(pjlCmd, timeoutMs); err == nil {
		return res, nil
	}

	// Intento 3: job RAW via spooler (solo escritura; ReadPrinter como último recurso)
	return extractViaSpool(printerName, timeoutMs)
}

// tryUSBRaw enumera USB printer device paths via SetupDi y envía PJL directamente.
func tryUSBRaw(pjlCmd string, timeoutMs int) (*Result, error) {
	paths, err := usbraw.FindDevicePaths()
	if err != nil || len(paths) == 0 {
		return nil, fmt.Errorf("no se encontraron device paths USB: %v", err)
	}

	for _, path := range paths {
		data, err := usbraw.SendPJLAndRead(path, pjlCmd, timeoutMs)
		if err != nil {
			log.Printf("[PJL-Raw] %s: %v", path, err)
			continue
		}
		if len(data) > 0 {
			log.Printf("[PJL-Raw] Respuesta de %s: %d bytes", path, len(data))
			res := parsePJL(data)
			// Si el parser no extrajo nada útil, retornar el error para ver el dump
			if res.Model == "" && res.PageCount == 0 && len(res.Supplies) == 0 && res.Status == "" {
				log.Printf("[PJL-Raw] Respuesta no parseada como PJL — ver dump arriba para analizar formato")
				return nil, fmt.Errorf("respuesta de %d bytes no es PJL estándar", len(data))
			}
			return res, nil
		}
	}

	return nil, fmt.Errorf("USB raw: ningún device respondió datos PJL")
}

func tryPortMonitor(portWithColon string, timeoutMs int) (*Result, error) {
	handle, err := winOpenPrinter(portWithColon)
	if err != nil {
		return nil, fmt.Errorf("port monitor %s: %w", portWithColon, err)
	}
	defer winClosePrinter(handle)

	log.Printf("[PJL] Puerto monitor abierto: %s", portWithColon)

	pjlCmd := buildPJLQuery()
	cmdBytes := []byte(pjlCmd)
	var written uint32
	r, _, e := procWritePrinter.Call(
		uintptr(handle),
		uintptr(unsafe.Pointer(&cmdBytes[0])),
		uintptr(len(cmdBytes)),
		uintptr(unsafe.Pointer(&written)),
	)
	if r == 0 {
		return nil, fmt.Errorf("WritePrinter (port): %w", e)
	}
	log.Printf("[PJL] %d bytes escritos al puerto", written)

	time.Sleep(2 * time.Second)

	buf := make([]byte, 65536)
	var readBytes uint32
	procReadPrinter.Call(
		uintptr(handle),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
		uintptr(unsafe.Pointer(&readBytes)),
	)

	if readBytes == 0 {
		return nil, fmt.Errorf("ReadPrinter (port): no devolvió datos")
	}

	log.Printf("[PJL] Respuesta recibida: %d bytes", readBytes)
	return parsePJL(buf[:readBytes]), nil
}

// extractViaSpool envía PJL como job RAW e intenta leer la respuesta bidireccional.
func extractViaSpool(printerName string, timeoutMs int) (*Result, error) {
	handle, err := winOpenPrinter(printerName)
	if err != nil {
		return nil, fmt.Errorf("OpenPrinter(%q): %w", printerName, err)
	}
	defer winClosePrinter(handle)

	log.Printf("[PJL] Spooler abierto para: %s", printerName)

	docNameW, _ := syscall.UTF16PtrFromString("PJL-Query")
	datatypeW, _ := syscall.UTF16PtrFromString("RAW")
	di := docInfo1W{
		pDocName:  docNameW,
		pDatatype: datatypeW,
	}

	jobID, _, e := procStartDocPrinterW.Call(
		uintptr(handle), 1, uintptr(unsafe.Pointer(&di)),
	)
	if jobID == 0 {
		return nil, fmt.Errorf("StartDocPrinter: %w", e)
	}
	defer procEndDocPrinter.Call(uintptr(handle))

	r, _, e := procStartPagePrinter.Call(uintptr(handle))
	if r == 0 {
		return nil, fmt.Errorf("StartPagePrinter: %w", e)
	}
	defer procEndPagePrinter.Call(uintptr(handle))

	pjlCmd := buildPJLQuery()
	cmdBytes := []byte(pjlCmd)
	var written uint32
	r, _, e = procWritePrinter.Call(
		uintptr(handle),
		uintptr(unsafe.Pointer(&cmdBytes[0])),
		uintptr(len(cmdBytes)),
		uintptr(unsafe.Pointer(&written)),
	)
	if r == 0 {
		return nil, fmt.Errorf("WritePrinter: %w", e)
	}
	log.Printf("[PJL] %d bytes enviados via spooler (job #%d)", written, jobID)

	// Esperar respuesta
	wait := time.Duration(timeoutMs) * time.Millisecond
	if wait < time.Second {
		wait = time.Second
	}
	time.Sleep(wait)

	buf := make([]byte, 65536)
	var readBytes uint32
	procReadPrinter.Call(
		uintptr(handle),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
		uintptr(unsafe.Pointer(&readBytes)),
	)

	if readBytes == 0 {
		return nil, fmt.Errorf("ReadPrinter (spool): la impresora no respondió datos PJL " +
			"(el driver no soporta lectura bidireccional vía spooler)")
	}

	log.Printf("[PJL] Respuesta recibida: %d bytes", readBytes)
	return parsePJL(buf[:readBytes]), nil
}

func buildPJLQuery() string {
	// Samsung ML-375x: DINQUIRE PAGECOUNT e INFO SUPPLIES devuelven "?" (sin chip CRUM).
	// INFO VARIABLES descubre qué variables soporta el firmware de esta unidad.
	// INQUIRE PAGECOUNT (sin D) es la variante síncrona; algunos firmwares lo responden.
	return uel + "@PJL" + crlf +
		"@PJL INFO ID" + crlf +
		"@PJL INFO STATUS" + crlf +
		"@PJL INFO SUPPLIES" + crlf +
		"@PJL INFO CONFIG" + crlf +
		"@PJL INFO VARIABLES" + crlf +
		"@PJL DINQUIRE PAGECOUNT" + crlf +
		"@PJL INQUIRE PAGECOUNT" + crlf +
		uel
}

// ── Win32 helpers ──────────────────────────────────────────────────────────

func winOpenPrinter(name string) (syscall.Handle, error) {
	nameW, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	var handle syscall.Handle
	r, _, e := procOpenPrinterW.Call(
		uintptr(unsafe.Pointer(nameW)),
		uintptr(unsafe.Pointer(&handle)),
		0,
	)
	if r == 0 {
		return 0, e
	}
	return handle, nil
}

func winClosePrinter(handle syscall.Handle) {
	procClosePrinter.Call(uintptr(handle))
}

// ── Parser PJL ─────────────────────────────────────────────────────────────

// parsePJL analiza la respuesta cruda PJL.
// Maneja los bytes especiales de Samsung: \x00 = separador de subsección, \x0C = fin de bloque.
func parsePJL(data []byte) *Result {
	// Samsung usa \x00 como separador de subsección en CONFIG; lo reemplazamos con \n
	clean := bytes.ReplaceAll(data, []byte{0x00}, []byte("\n"))
	// \x0C (form feed) = fin de bloque PJL → tratarlo como separador también
	clean = bytes.ReplaceAll(clean, []byte{0x0c}, []byte("\n"))

	lines := strings.Split(string(clean), "\n")
	res := &Result{
		Confidence: "pjl_basic",
		Online:     true,
		Supplies:   []payload.Supply{},
		Trays:      []payload.Tray{},
		Alerts:     []payload.Alert{},
	}

	var section string
	var configSub string // subsección dentro de CONFIG (trays, papers, etc.)
	var varSub string    // subsección dentro de VARIABLES (e.g. mediasource)
	var cur *supplyInProgress

	flush := func() {
		if cur != nil {
			res.Supplies = append(res.Supplies, cur.build())
			cur = nil
		}
	}

	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		// Preservar tabs para detección de indentación, luego strip para key/val
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "\x1B") {
			continue
		}

		// Detectar encabezado de sección PJL
		if strings.HasPrefix(trimmed, "@PJL") {
			parts := strings.Fields(trimmed)
			if len(parts) >= 3 && parts[1] == "INFO" {
				flush()
				section = parts[2]
				configSub = ""
				continue
			}
			if len(parts) >= 3 && (parts[1] == "DINQUIRE" || parts[1] == "INQUIRE") {
				flush()
				section = parts[1] + "_" + parts[2]
				configSub = ""
				continue
			}
		}

		// Samsung M332x/382x/402x: emite contenido CONFIG directamente después del INFO ID,
		// sin encabezado @PJL INFO CONFIG. Detectar por palabras clave típicas de CONFIG.
		if section == "ID" || section == "" {
			upper := strings.ToUpper(trimmed)
			if strings.HasPrefix(upper, "USTATUS") ||
				strings.HasPrefix(upper, "MEMORY=") ||
				strings.HasPrefix(upper, "SERIAL NUMBER") ||
				strings.HasPrefix(upper, "IN TRAYS") ||
				strings.HasPrefix(upper, "DISPLAY LINES") ||
				strings.HasPrefix(upper, "DISPLAY CHARACTER") {
				flush()
				section = "CONFIG"
				configSub = ""
			}
		}

		key, val := splitKV(trimmed)

		switch section {

		// ── INFO ID ──────────────────────────────────────────────────────
		case "ID":
			// El modelo PJL es siempre una cadena entre comillas. Solo capturar la primera.
			if key == "" && res.Model == "" && strings.HasPrefix(trimmed, `"`) {
				model := strings.Trim(trimmed, `"`)
				if model != "" && model != "?" {
					res.Model = model
					res.Brand = inferBrand(model)
				}
			}

		// ── INFO STATUS ──────────────────────────────────────────────────
		case "STATUS":
			switch key {
			case "CODE":
				if val != "" && val != "10001" && val != "10000" {
					res.Alerts = append(res.Alerts, payload.Alert{
						Code:    val,
						Message: "PJL status code: " + val,
						Level:   "warning",
					})
				}
			case "DISPLAY":
				res.Status = strings.Trim(val, `"`)
			case "ONLINE":
				res.Online = strings.EqualFold(val, "TRUE")
			case "FMU":
				// Samsung: Fusing Maintenance Unit status (GOOD/WARNING/REPLACE)
				if !strings.EqualFold(val, "GOOD") && val != "" {
					res.Alerts = append(res.Alerts, payload.Alert{
						Code:    "FMU_" + strings.ToUpper(val),
						Message: "Unidad de fusión: " + val,
						Level:   "warning",
					})
				}
			}

		// ── INFO SUPPLIES ─────────────────────────────────────────────────
		case "SUPPLIES":
			// Formato estructurado (HP/Xerox/Ricoh estándar)
			switch key {
			case "LABEL":
				flush()
				cur = &supplyInProgress{Name: strings.Trim(val, `"`)}
				res.Confidence = "pjl_full"
			case "COLORANT":
				if cur != nil {
					cur.Color = strings.ToLower(val)
				}
			case "TYPE":
				if cur != nil {
					cur.Type = normalizeSupplyType(val)
				}
			case "CAPACITY":
				if cur != nil {
					cur.MaxCapacity, _ = strconv.ParseInt(val, 10, 64)
				}
			case "LEVEL":
				if cur != nil {
					cur.Level, _ = strconv.ParseInt(val, 10, 64)
				}
			default:
				// Formato simple: TONER=75 o BLACK=75
				if strings.Contains(key, "TONER") || strings.Contains(key, "BLACK") {
					if lvl, err := strconv.ParseInt(val, 10, 64); err == nil && lvl >= 0 {
						flush()
						cur = &supplyInProgress{Name: "Black Toner", Color: "black", Type: "toner", Level: lvl}
						flush()
						res.Confidence = "pjl_full"
					}
				}
			}

		// ── INFO CONFIG ───────────────────────────────────────────────────
		case "CONFIG":
			// Detectar inicio de subsección
			upper := strings.ToUpper(trimmed)
			if strings.HasPrefix(upper, "IN TRAYS") {
				configSub = "trays"
				continue
			}
			if strings.HasPrefix(upper, "PAPERS") || strings.HasPrefix(upper, "LANGUAGES") ||
				strings.HasPrefix(upper, "USTATUS") || strings.HasPrefix(upper, "MEMORY") ||
				strings.HasPrefix(upper, "DISPLAY") {
				configSub = ""
			}

			// Tab indica elemento de la subsección activa
			isIndented := strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "    ")

			if isIndented && configSub == "trays" {
				name := trimmed
				id := len(res.Trays) + 1
				res.Trays = append(res.Trays, payload.Tray{
					ID:     id,
					Name:   normalizeTrayName(name),
					Status: "ok",
				})
				continue
			}

			// Claves con valor (SERIAL NUMBER, MEMORY, etc.)
			if strings.Contains(strings.ToUpper(key), "SERIAL") {
				s := strings.Trim(val, `"`)
				if s != "" && s != "?" {
					res.Serial = s
				}
			}

		// ── DINQUIRE/INQUIRE PAGECOUNT ────────────────────────────────────
		case "DINQUIRE_PAGECOUNT", "DINQUIRE_TOTALPAGECOUNT",
			"INQUIRE_PAGECOUNT", "INQUIRE_TOTALPAGECOUNT":
			v := val
			if v == "" {
				v = trimmed
			}
			v = strings.Trim(v, `" `)
			if v != "" && v != "?" && res.PageCount == 0 {
				if n, err := strconv.ParseInt(v, 10, 64); err == nil {
					res.PageCount = n
				}
			}

		// ── INFO VARIABLES ────────────────────────────────────────────────
		// Lista todas las variables PJL del firmware. Extrae tóner, páginas, y bandejas
		// desde MEDIASOURCE (Samsung M332x/382x no usa IN TRAYS en CONFIG).
		case "VARIABLES":
			// Línea indentada: es un valor enumerado de la variable anterior
			isIndented := strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "    ")
			if isIndented {
				if varSub == "mediasource" && len(res.Trays) < 8 {
					// Omitir opciones genéricas; registrar bandejas físicas
					switch strings.ToUpper(trimmed) {
					case "DEFAULT", "AUTO", "AUTOSELECT", "MANUAL", "MANUALFEED":
						// no es bandeja física
					case "MPF", "MPTRAY":
						id := len(res.Trays) + 1
						res.Trays = append(res.Trays, payload.Tray{ID: id, Name: "Manual Paper Tray", Status: "ok"})
					default:
						if strings.HasPrefix(strings.ToUpper(trimmed), "TRAY") {
							id := len(res.Trays) + 1
							res.Trays = append(res.Trays, payload.Tray{ID: id, Name: normalizeTrayName(trimmed), Status: "ok"})
						}
					}
				}
				continue
			}
			// Línea sin indentación = nueva variable
			varSub = ""
			if key == "" {
				continue
			}
			upper := strings.ToUpper(key)
			// Detectar inicio de MEDIASOURCE para recoger bandejas
			if strings.HasPrefix(upper, "MEDIASOURCE") && len(res.Trays) == 0 {
				varSub = "mediasource"
			}
			// Extraer valor si está inline: PAGECOUNT=12345 [1 RANGE ...]
			if strings.Contains(upper, "PAGECOUNT") || strings.Contains(upper, "TOTALPAGE") {
				// El valor puede estar antes del bracket: KEY=VALUE [range]
				valClean := val
				if idx := strings.Index(valClean, "["); idx != -1 {
					valClean = strings.TrimSpace(valClean[:idx])
				}
				valClean = strings.Trim(valClean, `" `)
				if valClean != "" && valClean != "?" && res.PageCount == 0 {
					if n, err := strconv.ParseInt(valClean, 10, 64); err == nil {
						res.PageCount = n
					}
				}
			}
			if strings.Contains(upper, "TONER") || strings.Contains(upper, "SUPPLY") {
				valClean := val
				if idx := strings.Index(valClean, "["); idx != -1 {
					valClean = strings.TrimSpace(valClean[:idx])
				}
				valClean = strings.Trim(valClean, `" `)
				if lvl, err := strconv.ParseInt(valClean, 10, 64); err == nil && lvl >= 0 && lvl <= 100 {
					// Solo agregar si no tenemos ya datos de SUPPLIES
					if len(res.Supplies) == 0 {
						pct := int(lvl)
						res.Supplies = append(res.Supplies, payload.Supply{
							Name:  key,
							Color: "black",
							Type:  "toner",
							Level: &pct,
						})
						res.Confidence = "pjl_full"
					}
				}
			}
		}
	}

	flush()
	return res
}

// normalizeTrayName convierte nombres internos PJL a nombres legibles.
func normalizeTrayName(raw string) string {
	switch strings.ToUpper(raw) {
	case "INTRAY1", "TRAY1":
		return "Tray 1"
	case "INTRAY2", "TRAY2":
		return "Tray 2"
	case "INTRAY3", "TRAY3":
		return "Tray 3"
	case "INMPTRAY", "MPTRAY", "MULTIPURPOSE":
		return "Manual Paper Tray"
	default:
		return raw
	}
}

type supplyInProgress struct {
	Name        string
	Color       string
	Type        string
	Level       int64
	MaxCapacity int64
}

func (s *supplyInProgress) build() payload.Supply {
	var levelPct *int
	if s.MaxCapacity > 0 && s.Level >= 0 {
		pct := int((s.Level * 100) / s.MaxCapacity)
		levelPct = &pct
	} else if s.Level >= 0 && s.Level <= 100 {
		pct := int(s.Level)
		levelPct = &pct
	}

	status := "ok"
	if levelPct != nil {
		switch {
		case *levelPct <= 10:
			status = "critical"
		case *levelPct <= 25:
			status = "warning"
		}
	}

	color := s.Color
	if color == "" {
		color = "black"
	}
	return payload.Supply{
		Name:   s.Name,
		Type:   s.Type,
		Color:  color,
		Level:  levelPct,
		Status: status,
	}
}

func splitKV(line string) (string, string) {
	parts := strings.SplitN(line, "=", 2)
	if len(parts) == 2 {
		return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	}
	return "", ""
}

func normalizeSupplyType(t string) string {
	switch strings.ToUpper(t) {
	case "TONERCONTAINER", "TONER", "TONERINK":
		return "toner"
	case "DRUMKIT", "DRUM":
		return "drum"
	case "FUSERKIT", "FUSER":
		return "fuser"
	default:
		return strings.ToLower(t)
	}
}

func inferBrand(model string) string {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "samsung"):
		return "Samsung"
	case strings.Contains(m, "hewlett") || strings.Contains(m, " hp"):
		return "HP"
	case strings.Contains(m, "xerox"):
		return "Xerox"
	case strings.Contains(m, "canon"):
		return "Canon"
	case strings.Contains(m, "epson"):
		return "Epson"
	case strings.Contains(m, "brother"):
		return "Brother"
	case strings.Contains(m, "ricoh"):
		return "Ricoh"
	case strings.Contains(m, "lexmark"):
		return "Lexmark"
	case strings.Contains(m, "kyocera"):
		return "Kyocera"
	case strings.Contains(m, "konica"):
		return "Konica Minolta"
	case strings.Contains(m, "oki"):
		return "OKI"
	default:
		return "Unknown"
	}
}
