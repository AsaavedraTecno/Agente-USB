package hpprotocol

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"usb-agent/internal/usbraw"
)

// EWSSupply representa un consumible leído desde la página "Supplies Status" del EWS.
type EWSSupply struct {
	Name         string
	PartNumber   string
	Percent      int
	PagesPrinted int
	InstallDate  string // YYYY-MM-DD
	Serial       string
}

// EWSTray representa una bandeja de papel leída de la tabla "Media" del home page.
type EWSTray struct {
	Name      string
	PaperSize string
	Status    string // "empty", "ok", etc. (estado real reportado por la impresora)
	Capacity  int    // hojas
}

// EWSStatus es el resultado de leer el Embedded Web Server de HP a través del pipe USB.
type EWSStatus struct {
	Model             string
	ModelNumber       string // SKU, ej. 3PZ35A
	Serial            string
	TotalPages        int64
	EngineCycles      int64 // ciclos del motor/fusor (Configuration Page)
	FirmwareRevision  string
	FirmwareDatecode  string // YYYY-MM-DD
	Supplies          []EWSSupply
	Trays             []EWSTray
	Location          string
	AssetNumber       string
	CompanyName       string
	ContactPerson     string
	StatusIcon        string // "ok", "warning", "error", etc.
	StatusMessage     string // ej. "Tray 2 empty: Plain, Letter"
	HasDuplex         bool
	OutputBinCapacity int
}

var (
	ewsCartridgeHeaderRe = regexp.MustCompile(`id="(\w+Cartridge\d+)-Header">([^<]+)</h2>`)
	ewsTotalPagesRe      = regexp.MustCompile(`ImpressionsByMediaSizeTable\.Print\.TotalTotal"[^>]*>([\d,]+)<`)
	ewsSerialRe          = regexp.MustCompile(`DeviceSerialNumber"[^>]*>([^<]+)<`)
	ewsProductNameRe     = regexp.MustCompile(`DeviceInformation\.ProductName"[^>]*>([^<]+)<`)
	ewsLocationRe        = regexp.MustCompile(`id="DeviceLocation">([^<]*)<`)
	ewsAssetRe           = regexp.MustCompile(`id="AssetNumber">([^<]*)<`)
	ewsCompanyRe         = regexp.MustCompile(`id="CompanyName">([^<]*)<`)
	ewsContactRe         = regexp.MustCompile(`id="ContactPerson">([^<]*)<`)
	ewsEngineCyclesRe    = regexp.MustCompile(`id="EngineCycles">([\d,]+)<`)
	ewsModelNumberRe     = regexp.MustCompile(`id="ModelNumber">([^<]+)<`)
	ewsFirmwareRevRe     = regexp.MustCompile(`id="FirmwareRevision">([^<]+)<`)
	ewsFirmwareDateRe    = regexp.MustCompile(`id="FirmwareDatecode">(\d{4})(\d{2})(\d{2})<`)
	ewsTrayBinNameRe     = regexp.MustCompile(`id="TrayBinName_(\d+)">([^<]+)<`)
	ewsMachineStatusRe   = regexp.MustCompile(`(?s)class="icon\s+(\w+)"></span>.*?id="MachineStatus">\s*([^<]+?)\s*</span>`)
	ewsOutputBinRe       = regexp.MustCompile(`id="OutputBin_1">(\d+)\s*Sheets<`)
)

// httpGetOverUSB envía un GET HTTP/1.1 crudo por el pipe USB bulk y devuelve el cuerpo
// de la respuesta. Algunos modelos HP más nuevos (heredados de Samsung pero con firmware
// "onehp") exponen su Embedded Web Server completo por el mismo endpoint USB que antes
// se usaba para PJL/comandos binarios — responden con HTTP real (Server: HP_Compact_Server
// o Virata-EmWeb) en vez del protocolo propietario.
func httpGetOverUSB(devicePath, path string, timeoutMs int) (string, error) {
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: printer\r\nConnection: close\r\n\r\n", path)
	fresh, drained, err := usbraw.SendRawAndRead(devicePath, []byte(req), timeoutMs, nil)
	resp := fresh
	if len(resp) == 0 {
		resp = drained
	}
	if len(resp) == 0 {
		if err != nil {
			return "", err
		}
		return "", fmt.Errorf("sin respuesta")
	}

	respStr := string(resp)
	sepIdx := strings.Index(respStr, "\r\n\r\n")
	if sepIdx == -1 {
		return "", fmt.Errorf("respuesta HTTP incompleta (%d bytes)", len(resp))
	}
	if !strings.Contains(respStr[:sepIdx], " 200 ") {
		return "", fmt.Errorf("HTTP no-200: %s", strings.SplitN(respStr, "\r\n", 2)[0])
	}
	return respStr[sepIdx+4:], nil
}

// FetchEWSStatus reconstruye tóner, serie, páginas y modelo hablando HTTP directo con
// el Embedded Web Server de HP a través del pipe USB. Es el respaldo cuando el protocolo
// binario heredado de Samsung (SendSamsungCommand) no responde, como ocurre en modelos
// más nuevos (ej. LaserJet E40040) donde ese endpoint en realidad es un puente HTTP/IPP-USB.
func FetchEWSStatus(devicePath string, timeoutMs int) (*EWSStatus, error) {
	// Home: trae el mensaje de estado general en vivo (ej. "Tray 2 empty", "Door open") que no
	// aparece en ninguna otra página — es opcional, sin él seguimos con el resto de los datos.
	home, _ := httpGetOverUSB(devicePath, "/", timeoutMs)
	supplies, err := httpGetOverUSB(devicePath, "/hp/device/InternalPages/Index?id=SuppliesStatus", timeoutMs)
	if err != nil {
		return nil, fmt.Errorf("EWS supplies status: %w", err)
	}
	usage, err := httpGetOverUSB(devicePath, "/hp/device/InternalPages/Index?id=UsagePage", timeoutMs)
	if err != nil {
		return nil, fmt.Errorf("EWS usage page: %w", err)
	}
	// Ubicación/Activo/Empresa/Contacto son campos que el admin configura a mano en el EWS —
	// esta página puede fallar (auth, modelo distinto) sin que eso invalide lo ya obtenido arriba.
	deviceInfo, _ := httpGetOverUSB(devicePath, "/hp/device/DeviceInformation/View", timeoutMs)
	// Engine Cycles = ciclos del motor de impresión, lo que en la etiqueta de la impresora
	// aparece como "Fusor" — no es un consumible con % sino un contador de vida acumulada.
	configPage, _ := httpGetOverUSB(devicePath, "/hp/device/InternalPages/Index?id=ConfigurationPage", timeoutMs)

	status := &EWSStatus{}

	for _, m := range ewsCartridgeHeaderRe.FindAllStringSubmatch(supplies, -1) {
		prefix, name := regexp.QuoteMeta(m[1]), m[2]
		if name == "" {
			continue
		}
		s := EWSSupply{Name: name}
		if pm := regexp.MustCompile(prefix + `-InstalledPartNumber">([^<]+)<`).FindStringSubmatch(supplies); len(pm) > 1 {
			s.PartNumber = pm[1]
		}
		if gm := regexp.MustCompile(prefix + `-Header_Level">(\d+)%`).FindStringSubmatch(supplies); len(gm) > 1 {
			s.Percent, _ = strconv.Atoi(gm[1])
		}
		if pp := regexp.MustCompile(prefix + `-PagesPrintedWithSupply">([\d,]+)<`).FindStringSubmatch(supplies); len(pp) > 1 {
			s.PagesPrinted, _ = strconv.Atoi(strings.ReplaceAll(pp[1], ",", ""))
		}
		if sn := regexp.MustCompile(prefix + `-SerialNumber">([^<]+)<`).FindStringSubmatch(supplies); len(sn) > 1 {
			s.Serial = sn[1]
		}
		if id := regexp.MustCompile(prefix + `-FirstInstallDate">(\d{4})(\d{2})(\d{2})<`).FindStringSubmatch(supplies); len(id) > 3 {
			s.InstallDate = fmt.Sprintf("%s-%s-%s", id[1], id[2], id[3])
		}
		status.Supplies = append(status.Supplies, s)
	}

	if m := ewsSerialRe.FindStringSubmatch(usage); len(m) > 1 {
		status.Serial = strings.TrimSpace(m[1])
	}
	if m := ewsProductNameRe.FindStringSubmatch(usage); len(m) > 1 {
		status.Model = strings.TrimSpace(m[1])
	}
	if m := ewsTotalPagesRe.FindStringSubmatch(usage); len(m) > 1 {
		if n, err := strconv.ParseInt(strings.ReplaceAll(m[1], ",", ""), 10, 64); err == nil {
			status.TotalPages = n
		}
	}

	if m := ewsLocationRe.FindStringSubmatch(deviceInfo); len(m) > 1 {
		status.Location = strings.TrimSpace(m[1])
	}
	if m := ewsAssetRe.FindStringSubmatch(deviceInfo); len(m) > 1 {
		status.AssetNumber = strings.TrimSpace(m[1])
	}
	if m := ewsCompanyRe.FindStringSubmatch(deviceInfo); len(m) > 1 {
		status.CompanyName = strings.TrimSpace(m[1])
	}
	if m := ewsContactRe.FindStringSubmatch(deviceInfo); len(m) > 1 {
		status.ContactPerson = strings.TrimSpace(m[1])
	}

	if m := ewsEngineCyclesRe.FindStringSubmatch(configPage); len(m) > 1 {
		if n, err := strconv.ParseInt(strings.ReplaceAll(m[1], ",", ""), 10, 64); err == nil {
			status.EngineCycles = n
		}
	}
	if m := ewsModelNumberRe.FindStringSubmatch(configPage); len(m) > 1 {
		status.ModelNumber = m[1]
	}
	if m := ewsFirmwareRevRe.FindStringSubmatch(configPage); len(m) > 1 {
		status.FirmwareRevision = m[1]
	}
	if m := ewsFirmwareDateRe.FindStringSubmatch(configPage); len(m) > 3 {
		status.FirmwareDatecode = fmt.Sprintf("%s-%s-%s", m[1], m[2], m[3])
	}
	// Bandejas: la tabla "Media" del home page trae estado real (Empty/OK) y capacidad en
	// hojas, algo que la Configuration Page no tiene (solo tamaño/tipo configurado).
	for _, m := range ewsTrayBinNameRe.FindAllStringSubmatch(home, -1) {
		idx, name := m[1], m[2]
		size := regexp.MustCompile(`id="TrayBinSize_` + idx + `">([^<]+)<`).FindStringSubmatch(home)
		if len(size) > 1 && (size[1] == "N&#47;A" || size[1] == "N/A") {
			continue // bandeja de salida (Standard bin) — ya se captura aparte como Output Bin
		}
		t := EWSTray{Name: name}
		if len(size) > 1 {
			t.PaperSize = size[1]
		}
		if st := regexp.MustCompile(`(?s)id="TrayBinStatus_` + idx + `"[^>]*>([^<]+)<`).FindStringSubmatch(home); len(st) > 1 {
			t.Status = strings.ToLower(strings.TrimSpace(st[1]))
		}
		if cap := regexp.MustCompile(`id="TrayBinCapacity_` + idx + `">(\d+)\s*sheets<`).FindStringSubmatch(home); len(cap) > 1 {
			t.Capacity, _ = strconv.Atoi(cap[1])
		}
		status.Trays = append(status.Trays, t)
	}
	status.HasDuplex = strings.Contains(configPage, `id="DuplexUnit">Duplex Unit<`)
	if m := ewsOutputBinRe.FindStringSubmatch(configPage); len(m) > 1 {
		status.OutputBinCapacity, _ = strconv.Atoi(m[1])
	}

	if m := ewsMachineStatusRe.FindStringSubmatch(home); len(m) > 2 {
		status.StatusIcon = m[1]
		status.StatusMessage = strings.TrimSpace(m[2])
	}

	if status.Serial == "" && len(status.Supplies) == 0 && status.TotalPages == 0 {
		return nil, fmt.Errorf("EWS no devolvió datos reconocibles")
	}

	return status, nil
}
