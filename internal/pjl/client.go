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
	"usb-agent/internal/spooler"
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
	Model           string
	Brand           string
	Serial          string
	PCBMSerial      string // LAS_PCBM_SN — base de la MAC address de red
	PrinterHostname string // BRW/BRN + PCBM_SN (derivado)
	NetworkConn     string // LAS_NETWORK_CONNECTION: WLAN/LAN
	Status          string
	Online          bool
	PageCount       int64
	PrintPages      int64
	CopyPages       int64
	DuplexPages     int64
	DuplexSet       bool // true cuando LAS_PAGECOUNT_TOTAL_DX fue recibido (incluso si es 0)
	ScanPages       int64
	CoverageLast    float64
	CoverageAvg     float64 // LAS_COVERAGE_ACC — cobertura promedio histórica
	UptimeMinutes   int64   // LAS_TOTALTIME_POWER_ON en minutos
	PowerOnCount    int64   // LAS_POWER_ON_COUNT — número de encendidos
	EngineCycles    int64   // LAS_DEVROLLER_COUNT — contador mecánico del rodillo
	JamTotal        int64   // LAS_JAMCOUNT
	JamTray1        int64   // LAS_JAMCOUNTT1
	JamTray2        int64   // LAS_JAMCOUNTT2
	JamTrayMP       int64   // LAS_JAMCOUNTMP
	JamInside       int64   // LAS_JAMCOUNTINSIDE
	JamRear         int64   // LAS_JAMCOUNTREAR
	MAC             string
	IP              string
	Firmware        string
	HasVariables    bool
	Supplies        []payload.Supply
	Trays           []payload.Tray
	Alerts          []payload.Alert
	Confidence      string // pjl_full | pjl_basic
}

// Extract envía comandos PJL al nombre de impresora dado.
//
// El spooler de Windows compite por los bytes del endpoint bulk-IN USB: cuando la
// impresora responde a BRSUPPLY (~8-10s después del write), el spooler puede leer
// esos bytes antes que nosotros. Por eso bypass_spooler=true detiene el servicio
// ANTES de abrir el device, garantizando acceso exclusivo al canal bidireccional.
//
// Orden de intento:
//  1. USB raw con spooler detenido (30s timeout) — si bypass_spooler habilitado
//  2. USB raw con spooler activo (timeout normal) — fallback o si bypass falla/no admin
//  3. Job RAW via spooler + ReadPrinter — último recurso
func Extract(printerName, portName string, timeoutMs int, bypassSpooler bool) (*Result, error) {
	pjlCmd := buildPJLQuery()

	// Intento 1: detener spooler PRIMERO para acceso exclusivo al bulk-IN USB.
	// Sin spooler activo, la respuesta BRSUPPLY llega sin competencia.
	if bypassSpooler {
		log.Printf("[PJL-Raw] Deteniendo Spooler para acceso exclusivo al USB...")
		if stopErr := spooler.Stop(); stopErr == nil {
			log.Printf("[PJL-Raw] Spooler detenido. Esperando que libere el puerto...")
			time.Sleep(2 * time.Second) // dar tiempo a que se cierren los handles

			res, usbErr := tryUSBRaw(pjlCmd, 30000) // ventana de 30s para capturar BRSUPPLY

			log.Printf("[PJL-Raw] Restaurando Spooler...")
			spooler.Start()

			if usbErr == nil {
				return res, nil
			}
			log.Printf("[PJL-Raw] USB exclusivo falló: %v — continuando sin bypass", usbErr)
		} else {
			log.Printf("[PJL-Raw] No se pudo detener Spooler (¿se ejecuta como Admin?): %v", stopErr)
		}
	}

	// Intento 2: USB raw con spooler activo (datos parciales posibles por competencia)
	if res, err := tryUSBRaw(pjlCmd, timeoutMs); err == nil {
		return res, nil
	}

	// Intento 3: job RAW via spooler + ReadPrinter (último recurso)
	return extractViaSpool(printerName, timeoutMs)
}

// tryUSBRaw enumera USB printer device paths via SetupDi y envía PJL directamente.
// Estrategia de dos fases:
//  1. Lectura fresca: espera hasta ~10 s que la impresora responda al comando recién enviado.
//  2. Fallback al buffer drenado: la Brother HL-L5xxx tarda ~5-7 s en preparar su respuesta;
//     el driver USB retorna ERROR_NO_DATA inmediatamente, así que la respuesta llega después
//     de que la ventana de lectura se cierra y queda en el buffer hasta la siguiente ejecución.
//     Esos bytes drenados son una respuesta PJL válida del ciclo anterior y se usan como
//     fallback cuando la lectura fresca no obtiene datos a tiempo.
func tryUSBRaw(pjlCmd string, timeoutMs int) (*Result, error) {
	paths, err := usbraw.FindDevicePaths()
	if err != nil || len(paths) == 0 {
		return nil, fmt.Errorf("no se encontraron device paths USB: %v", err)
	}

	for _, path := range paths {
		isComplete := func(data []byte, isIdle bool) bool {
			if !isIdle {
				return false
			}
			res := parsePJL(data)
			if res.Model == "" {
				return false
			}

			// Esperar siempre hasta que el bloque VARIABLES termine (marcado con \x0c).
			// Como es el último comando que enviamos, si vemos \x0c después de él,
			// la impresora ha respondido a todo.
			idx := bytes.Index(data, []byte("@PJL INFO VARIABLES"))
			if idx != -1 {
				// Buscar \x0c después de la cabecera
				if bytes.IndexByte(data[idx:], '\x0c') != -1 {
					return len(res.Supplies) > 0 || res.PageCount > 0
				}
			}

			// Si la impresora no soporta nada de lo anterior, esperará al timeout (30s) por seguridad.
			return false
		}

		fresh, drained, err := usbraw.SendPJLAndRead(path, pjlCmd, timeoutMs, isComplete)
		if err != nil {
			log.Printf("[PJL-Raw] %s: %v", path, err)
			continue
		}

		resFresh := parsePJL(fresh)
		var combined []byte
		var source string
		if resFresh.Model != "" && (len(resFresh.Supplies) > 0 || resFresh.PageCount > 0) {
			// La respuesta fresca está completa. Descartamos el buffer previo porque podría
			// estar corrupto o pisado por el spooler.
			combined = fresh
			source = fmt.Sprintf("%d bytes frescos", len(fresh))
			if len(drained) > 0 {
				log.Printf("[PJL-Raw] Descartando %d bytes previos (fresca ya está completa)", len(drained))
			}
		} else {
			// Combinar drained + fresh como fallback (modo sin bypass spooler)
			combined = append(drained, fresh...)
			if len(combined) == 0 {
				continue
			}
			if len(drained) > 0 && len(fresh) > 0 {
				source = fmt.Sprintf("%d bytes previos + %d bytes frescos", len(drained), len(fresh))
			} else if len(drained) > 0 {
				source = fmt.Sprintf("%d bytes del buffer previo", len(drained))
			} else {
				source = fmt.Sprintf("%d bytes frescos", len(fresh))
			}
		}

		res := parsePJL(combined)
		if res.Model != "" || res.PageCount != 0 || len(res.Supplies) != 0 || res.Status != "" || res.Serial != "" {
			log.Printf("[PJL-Raw] %s → datos OK (%s)", path, source)
			return res, nil
		}
		log.Printf("[PJL-Raw] %s bytes combinados no son PJL estándar — ver dump", source)
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
	// Order matters: BRSUPPLY and PAGECOUNT come before the large VARIABLES block.
	// Brother HL-series returns "?" for standard SUPPLIES but responds to BRSUPPLY.
	// VARIABLES generates a huge response (~4KB+) that can overflow read buffers if placed first.
	return uel + "@PJL" + crlf +
		"@PJL INFO ID" + crlf +
		"@PJL INFO STATUS" + crlf +
		"@PJL INFO SUPPLIES" + crlf +
		"@PJL INFO BRSUPPLY" + crlf +
		"@PJL INFO PRODINFO" + crlf +
		"@PJL INFO NETWORK" + crlf +
		"@PJL INFO BRNETINFO" + crlf +
		"@PJL INQUIRE IPADDRESS" + crlf +
		"@PJL INQUIRE IPV4" + crlf +
		"@PJL INQUIRE MACADDRESS" + crlf +
		"@PJL DINQUIRE PAGECOUNT" + crlf +
		"@PJL INQUIRE PAGECOUNT" + crlf +
		"@PJL INFO CONFIG" + crlf +
		"@PJL INFO VARIABLES" + crlf +
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
	var brData map[string]string // accumulated LAS_ key-value pairs from BRSUPPLY

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
					// Brother: "HL-L5210DN series:84U-L0A:Ver.1.27" → solo el nombre
					if idx := strings.Index(model, ":"); idx > 0 {
						rest := model[idx+1:]
						if strings.Contains(strings.ToLower(rest), "ver.") {
							model = model[:idx]
						}
					}
					res.Model = strings.TrimSpace(model)
					res.Brand = inferBrand(res.Model)
				}
			}

		// ── INFO STATUS ──────────────────────────────────────────────────
		case "STATUS":
			switch key {
			case "CODE":
				// Códigos normales/informativos que no representan error:
				// 10000=Ready, 10001=Offline, 40000=Waiting/Warming up (Brother)
				normalCodes := map[string]bool{"10000": true, "10001": true, "40000": true}
				if val != "" && !normalCodes[val] {
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
				res.Trays = append(res.Trays, payload.Tray{
					Name:   normalizeTrayName(name),
					Status: "OK",
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

		// ── INFO BRSUPPLY (Brother proprietary) ───────────────────────────
		// Accumulate all LAS_ key-value pairs; processed after the loop.
		case "BRSUPPLY":
			if key != "" {
				if brData == nil {
					brData = make(map[string]string)
				}
				brData[strings.ToUpper(key)] = strings.Trim(val, `" `)
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

		// ── NETWORK INFO ──────────────────────────────────────────────────
		case "NETWORK", "BRNETINFO", "PRODINFO":
			if key == "IPADDRESS" || key == "IP_ADDRESS" || key == "LAS_NETWORK_IPADDRESS" || key == "IPV4" {
				res.IP = strings.Trim(val, `" `)
			} else if key == "MACADDRESS" || key == "MAC_ADDRESS" || key == "LAS_NETWORK_MACADDRESS" || key == "MAC" {
				res.MAC = strings.Trim(val, `" `)
			}

		case "INQUIRE_IPADDRESS", "INQUIRE_IPV4":
			v := val
			if v == "" {
				v = trimmed
			}
			v = strings.Trim(v, `" `)
			if v != "" && v != "?" && res.IP == "" {
				res.IP = v
			}

		case "INQUIRE_MACADDRESS":
			v := val
			if v == "" {
				v = trimmed
			}
			v = strings.Trim(v, `" `)
			if v != "" && v != "?" && res.MAC == "" {
				res.MAC = v
			}

		// ── INFO VARIABLES ────────────────────────────────────────────────
		// Lista todas las variables PJL del firmware. Extrae tóner, páginas, y bandejas
		// desde MEDIASOURCE (Samsung M332x/382x no usa IN TRAYS en CONFIG).
		case "VARIABLES":
			res.HasVariables = true
			// Línea indentada: es un valor enumerado de la variable anterior
			isIndented := strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "    ")
			if isIndented {
				if varSub == "mediasource" && len(res.Trays) < 8 {
					// Omitir opciones genéricas; registrar bandejas físicas
					switch strings.ToUpper(trimmed) {
					case "DEFAULT", "AUTO", "AUTOSELECT", "MANUAL", "MANUALFEED":
						// no es bandeja física
					case "MPF", "MPTRAY":
						res.Trays = append(res.Trays, payload.Tray{Name: "Manual Paper Tray", Status: "OK"})
					default:
						if strings.HasPrefix(strings.ToUpper(trimmed), "TRAY") {
							res.Trays = append(res.Trays, payload.Tray{Name: normalizeTrayName(trimmed), Status: "OK"})
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
			if upper == "HWADDRESS" || upper == "MACADDRESS" {
				valClean := strings.Trim(val, `" `)
				if valClean != "" && valClean != "?" && res.MAC == "" {
					res.MAC = valClean
				}
			}
			if upper == "IPADDRESS" || upper == "IPV4" {
				valClean := strings.Trim(val, `" `)
				if valClean != "" && valClean != "?" && res.IP == "" {
					res.IP = valClean
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
							Name:       key,
							Color:      "black",
							Type:       "toner",
							Percentage: payload.Float64Ptr(float64(pct)),
						})
						res.Confidence = "pjl_full"
					}
				}
			}
		}
	}

	flush()
	if brData != nil {
		applyBRSupplyData(res, brData)
	}
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

	status := "OK"
	if levelPct != nil {
		switch {
		case *levelPct <= 10:
			status = "Cr\u00edtico"
		case *levelPct <= 25:
			status = "Bajo"
		}
	}

	color := s.Color
	if color == "" {
		color = "black"
	}
	return payload.Supply{
		Name:       s.Name,
		Type:       s.Type,
		Color:      color,
		Percentage: payload.Float64Ptr(float64(*levelPct)),
		Status:     status,
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

// applyBRSupplyData extracts all useful fields from the Brother LAS_ key-value map.
// Brother HL-L5xxx returns TONER_REMAIN as a float (e.g. "94.00") and all counters
// and supply lifetimes via LAS_* keys in the BRSUPPLY section.
func applyBRSupplyData(res *Result, d map[string]string) {
	get := func(k string) string { return d[k] }

	// Identity
	if v := get("LAS_MODEL_NAME"); v != "" && res.Model == "" {
		res.Model = v
		res.Brand = inferBrand(v)
	}
	if v := strings.TrimSpace(get("LAS_MACHINE_SN")); v != "" && v != "?" && res.Serial == "" {
		res.Serial = v
	}

	// Total page count
	if v := get("LAS_PAGECOUNT_TOTAL"); v != "" && res.PageCount == 0 {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			res.PageCount = n
		}
	}
	if v := get("LAS_PAGECOUNT_PCPRINT"); v != "" && res.PrintPages == 0 {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			res.PrintPages = n
		}
	}
	if v := get("LAS_PAGECOUNT_OTHER"); v != "" && res.CopyPages == 0 {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			res.CopyPages = n
		}
	}
	if v := get("LAS_PAGECOUNT_TOTAL_DX"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			res.DuplexPages = n
			res.DuplexSet = true
		}
	} else if v := get("LAS_COUNTPAGE_DX"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			res.DuplexPages = n
			res.DuplexSet = true
		}
	}
	if v := get("LAS_SCANNER_PAGE_COUNT"); v != "" && res.ScanPages == 0 {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			res.ScanPages = n
		}
	}

	if v := get("LAS_COVERAGE_LAST"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			res.CoverageLast = f
		}
	}
	if v := get("LAS_RVERSION"); v != "" && res.Firmware == "" {
		res.Firmware = v
	} else if v := get("LAS_VERSION"); v != "" && res.Firmware == "" {
		res.Firmware = v
	}
	if v := get("LAS_TOTALTIME_POWER_ON"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			res.UptimeMinutes = n
		}
	}
	if v := strings.TrimSpace(get("LAS_PCBM_SN")); v != "" && v != "?" && res.PCBMSerial == "" {
		res.PCBMSerial = v
		// Derivar MAC: "3BE1000312B8" → "3b:e1:00:03:12:b8"
		if len(v) == 12 && res.MAC == "" {
			b := strings.ToLower(v)
			res.MAC = b[0:2] + ":" + b[2:4] + ":" + b[4:6] + ":" + b[6:8] + ":" + b[8:10] + ":" + b[10:12]
		}
	}
	// Derivar hostname de impresora: BRW (WiFi) o BRN (LAN) + PCBM_SN en mayúsculas
	if nc := get("LAS_NETWORK_CONNECTION"); nc != "" {
		res.NetworkConn = nc
	}
	if res.PCBMSerial != "" && res.PrinterHostname == "" {
		prefix := "BRN"
		if strings.EqualFold(res.NetworkConn, "WLAN") {
			prefix = "BRW"
		}
		res.PrinterHostname = prefix + strings.ToUpper(res.PCBMSerial)
	}

	if v := get("LAS_COVERAGE_ACC"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			res.CoverageAvg = f
		}
	}
	if v := get("LAS_POWER_ON_COUNT"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			res.PowerOnCount = n
		}
	}
	if v := get("LAS_DEVROLLER_COUNT"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			res.EngineCycles = n
		}
	}

	// Jam counters — predicción de mantenimiento por bandeja
	parseJam := func(key string) int64 {
		if v := get(key); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				return n
			}
		}
		return 0
	}
	res.JamTotal = parseJam("LAS_JAMCOUNT")
	res.JamTray1 = parseJam("LAS_JAMCOUNTT1")
	res.JamTray2 = parseJam("LAS_JAMCOUNTT2")
	res.JamTrayMP = parseJam("LAS_JAMCOUNTMP")
	res.JamInside = parseJam("LAS_JAMCOUNTINSIDE")
	res.JamRear = parseJam("LAS_JAMCOUNTREAR")

	// Toner level — Brother reports as float percentage e.g. "94.00"
	if v := get("LAS_TONER_REMAIN"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			pct := int(f + 0.5)
			if pct > 100 {
				pct = 100
			}

			var genuine *bool
			if gk := get("LAS_TONER_GENUINE_K"); gk != "" {
				gen := gk == "1"
				genuine = &gen
			}
			var changeCount *int
			if cc := get("LAS_TONER_CHANGE_COUNT"); cc != "" {
				if c, err := strconv.Atoi(cc); err == nil {
					changeCount = &c
				}
			}
			var CartridgeType *string
			if pt := get("LAS_TONER_CHANGE_TYPE1"); pt != "" {
				CartridgeType = &pt
			}

			var sn *string
			if sv := get("LAS_TONER_SN"); sv != "" && sv != "?" {
				sn = &sv
			}

			rawL := pct
			rawM := 100
			res.Supplies = append(res.Supplies, payload.Supply{
				ID:            fmt.Sprintf("toner_black_.1.%d", len(res.Supplies)+1),
				Name:          "Toner Black",
				Type:          "toner",
				Color:         "black",
				Description:   "Black Toner Cartridge",
				Percentage:    payload.Float64Ptr(float64(pct)),
				Status:        brSupplyStatus(pct),
				IsMeasurable:  true,
				RawLevel:      &rawL,
				RawMax:        &rawM,
				Genuine:       genuine,
				ChangeCount:   changeCount,
				CartridgeType: CartridgeType,
				SerialNumber:  sn,
			})
			res.Confidence = "pjl_brother_custom"
		}
	}

	// Drum unit — remaining pages via LAS_NEXTCARE_DRUM / LAS_DRUM_LIFE_PERIOD
	if remain, life := get("LAS_NEXTCARE_DRUM"), get("LAS_DRUM_LIFE_PERIOD"); remain != "" && life != "" {
		if r, err1 := strconv.ParseInt(remain, 10, 64); err1 == nil {
			if l, err2 := strconv.ParseInt(life, 10, 64); err2 == nil && l > 0 {
				pct := int((r * 100) / l)
				if pct > 100 {
					pct = 100
				}
				if pct < 0 {
					pct = 0
				}
				rawL := int(r)
				rawM := int(l)
				res.Supplies = append(res.Supplies, payload.Supply{
					ID:           fmt.Sprintf("drum_.1.%d", len(res.Supplies)+1),
					Name:         "Drum Black",
					Description:  "Black Drum Unit",
					Type:         "drum",
					Color:        "black",
					Percentage:   payload.Float64Ptr(float64(pct)),
					Status:       brSupplyStatus(pct),
					IsMeasurable: true,
					RawLevel:     &rawL,
					RawMax:       &rawM,
				})
			}
		}
	}

	// Fuser and paper-feed kits — remain / life_period pairs
	kits := []struct{ remain, life, name, typ string }{
		{"LAS_FUSER_REMAIN", "LAS_FUSER_LIFE_PERIOD", "Fuser Kit", "fuser"},
		{"LAS_SCANNER_REMAIN", "LAS_SCANNER_LIFE_PERIOD", "Scanner Kit", "maintenance"},
		{"LAS_PFKIT1_REMAIN", "LAS_PFKIT1_LIFE_PERIOD", "Paper Feed Kit (Tray 1)", "maintenance"},
		{"LAS_PFKITMP_REMAIN", "LAS_PFKITMP_LIFE_PERIOD", "Paper Feed Kit (MP Tray)", "maintenance"},
		{"LAS_PFKIT2_REMAIN", "LAS_PFKIT2_LIFE_PERIOD", "Paper Feed Kit (Tray 2)", "maintenance"},
		{"LAS_PFKIT3_REMAIN", "LAS_PFKIT3_LIFE_PERIOD", "Paper Feed Kit (Tray 3)", "maintenance"},
		{"LAS_PFKIT4_REMAIN", "LAS_PFKIT4_LIFE_PERIOD", "Paper Feed Kit (Tray 4)", "maintenance"},
	}
	for _, k := range kits {
		rv, lv := get(k.remain), get(k.life)
		if rv == "" || lv == "" {
			continue
		}
		r, err1 := strconv.ParseInt(rv, 10, 64)
		l, err2 := strconv.ParseInt(lv, 10, 64)
		if err1 != nil || err2 != nil || l <= 0 {
			continue
		}
		pct := int((r * 100) / l)
		if pct > 100 {
			pct = 100
		}
		if pct < 0 {
			pct = 0
		}
		rawL := int(r)
		rawM := int(l)
		res.Supplies = append(res.Supplies, payload.Supply{
			ID:           fmt.Sprintf("%s_.1.%d", k.typ, len(res.Supplies)+1),
			Name:         k.name,
			Description:  k.name,
			Type:         k.typ,
			Color:        "n/a",
			Percentage:   payload.Float64Ptr(float64(pct)),
			Status:       brSupplyStatus(pct),
			IsMeasurable: true,
			RawLevel:     &rawL,
			RawMax:       &rawM,
		})
	}
}

func brSupplyStatus(pct int) string {
	switch {
	case pct <= 10:
		return "Cr\u00edtico"
	case pct <= 25:
		return "Bajo"
	default:
		return "OK"
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
