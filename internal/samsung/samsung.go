package samsung

import (
	"encoding/binary"
	"fmt"
	"log"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"
)

type SamsungData struct {
	Serial      string
	TonerModels []string
	Status      string
	Alerts      []Alert
	SuppliesBin []SupplyBin
}

type Alert struct {
	Type         string
	Message      string
	StandardCode string
}

type SupplyBin struct {
	Type  uint32
	Value uint32
}

// PrepareSamsungUSB detiene el spooler para acceso exclusivo.
func PrepareSamsungUSB() error {
	cmd := exec.Command("net", "stop", "spooler")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.Run()
	time.Sleep(2 * time.Second)
	return nil
}

// RestoreSpooler reinicia el servicio spooler.
func RestoreSpooler() {
	cmd := exec.Command("net", "start", "spooler")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.Run()
}

// SendSamsungCommand envía el pseudo-setup packet a través del Control Pipe (Endpoint 0)
func SendSamsungCommand(devicePath string, cmd byte, formatA bool) ([]byte, error) {
	pathW, err := syscall.UTF16PtrFromString(devicePath)
	if err != nil {
		return nil, err
	}

	// Abrimos SIN Overlapped. Los Control Transfers son instantáneos y DeviceIoControl se encarga
	handle, err := syscall.CreateFile(
		pathW,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		0, // Acceso exclusivo
		nil,
		syscall.OPEN_EXISTING,
		0, // SIN OVERLAPPED
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("CreateFile error: %w", err)
	}
	defer syscall.CloseHandle(handle)

	var inBuf []byte
	if formatA {
		// bRequest = 0x0D, wValue MSB = 0x00, wValue LSB = cmd
		inBuf = []byte{0x0d, 0x00, cmd}
	} else {
		// bRequest = cmd, wValue MSB = 0x00, wValue LSB = 0x00 (o 0x01 para 0x49)
		lsb := byte(0x00)
		if cmd == 0x49 {
			lsb = 0x01
		}
		inBuf = []byte{cmd, 0x00, lsb}
	}

	const IOCTL_USBPRINT_VENDOR_GET_COMMAND = 0x22003C
	outBuf := make([]byte, 255) // Wireshark dice wLength = 0x00FF (255 bytes exactos)
	var bytesReturned uint32

	err = syscall.DeviceIoControl(
		handle,
		IOCTL_USBPRINT_VENDOR_GET_COMMAND,
		&inBuf[0],
		uint32(len(inBuf)),
		&outBuf[0],
		uint32(len(outBuf)),
		&bytesReturned,
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("DeviceIoControl error: %w", err)
	}

	return outBuf[:bytesReturned], nil
}

// ParseSamsungResponse parsea la respuesta RAW.
func ParseSamsungResponse(raw []byte) (*SamsungData, error) {
	data := &SamsungData{}

	payloadBuf := raw
	// El header USB (IEEE1284 string) a veces se devuelve al principio (28 bytes o más).
	if len(raw) > 28 && raw[0] == 0x1c {
		payloadBuf = raw[28:]
	}

	if len(payloadBuf) == 0 {
		return data, nil
	}

	if payloadBuf[0] == '<' || (len(payloadBuf) > 5 && string(payloadBuf[:5]) == "<?xml") {
		parseSamsungXML(string(payloadBuf), data)
	} else if isASCII(payloadBuf) {
		data.Serial = strings.TrimRight(string(payloadBuf), "\x00")
	} else {
		parseSamsungBinary(payloadBuf, data)
	}

	return data, nil
}

func parseSamsungXML(xmlStr string, data *SamsungData) {
	if strings.Contains(xmlStr, "<TonerID>") {
		re := regexp.MustCompile(`>(W\d{4}[AX])<`)
		matches := re.FindAllStringSubmatch(xmlStr, -1)
		for _, m := range matches {
			data.TonerModels = append(data.TonerModels, m[1])
		}
	}

	alerts := []struct {
		tag  string
		name string
	}{
		{"TonerEmpty", "toner_empty"},
		{"TonerLow", "toner_low"},
		{"TonerExhausted", "toner_exhausted"},
		{"TonerWorn", "toner_worn"},
		{"ImagingUnitWorn", "imaging_unit_worn"},
		{"FuserHighError", "fuser_error"},
		{"MarkerSupplyLow", "supply_low"},
		{"MarkerFailure", "supply_failure"},
		{"MarkerSupplyEmpty", "supply_empty"},
		{"MediaLow", "media_low"},
		{"MediaEmpty", "media_empty"},
		{"DoorOpen", "door_open"},
		{"Offline", "offline"},
		{"PowerSave", "power_save"},
		{"Waiting", "waiting"},
		{"Printing", "printing"},
	}

	for _, a := range alerts {
		if strings.Contains(xmlStr, "<"+a.tag) {
			re := regexp.MustCompile(a.tag + `[^>]*STRING="([^"]*)"`)
			matches := re.FindStringSubmatch(xmlStr)
			alert := Alert{Type: a.name}
			if len(matches) > 1 {
				alert.Message = matches[1]
			}
			reCode := regexp.MustCompile(`StandardCode="([^"]*)"`)
			codeMatches := reCode.FindStringSubmatch(xmlStr)
			if len(codeMatches) > 1 {
				alert.StandardCode = codeMatches[1]
			}
			data.Alerts = append(data.Alerts, alert)
		}
	}

	if strings.Contains(xmlStr, "<Offline") {
		data.Status = "offline"
	} else if strings.Contains(xmlStr, "<PowerSave") {
		data.Status = "sleeping"
	} else if strings.Contains(xmlStr, "<Waiting") {
		data.Status = "warming_up"
	} else if strings.Contains(xmlStr, "<Printing") {
		data.Status = "printing"
	} else if strings.Contains(xmlStr, "<None") {
		data.Status = "ready"
	}
}

func parseSamsungBinary(payloadBuf []byte, data *SamsungData) {
	if len(payloadBuf) < 4 {
		return
	}

	count := binary.LittleEndian.Uint32(payloadBuf[0:4])
	if count > 10 {
		return
	}

	offset := 4
	for i := 0; i < int(count) && offset+8 <= len(payloadBuf); i++ {
		supply := SupplyBin{
			Type:  binary.LittleEndian.Uint32(payloadBuf[offset : offset+4]),
			Value: binary.LittleEndian.Uint32(payloadBuf[offset+4 : offset+8]),
		}
		data.SuppliesBin = append(data.SuppliesBin, supply)
		offset += 8
	}
}

func isASCII(data []byte) bool {
	for _, b := range data {
		if b != 0x00 && (b < 0x20 || b > 0x7e) {
			return false
		}
	}
	return true
}

// TrySamsungUSB ejecuta el escaneo aislado del protocolo propietario
func TrySamsungUSB(devicePath string) (*SamsungData, error) {
	PrepareSamsungUSB()
	defer RestoreSpooler()

	data := &SamsungData{}

	commands := []struct {
		cmd     byte
		formatA bool
		name    string
	}{
		{0x49, false, "serial"},
		{0x05, false, "supplies"},
		{0x0b, true, "toner_models"},
		{0x0c, true, "status"},
		{0x0f, true, "alerts"},
		{0x11, true, "media"},
		{0x16, true, "attention"},
		{0x18, true, "marker_ews"},
		{0x19, true, "network"},
	}

	for _, c := range commands {
		resp, err := SendSamsungCommand(devicePath, c.cmd, c.formatA)
		if err != nil {
			log.Printf("[SAMSUNG] Cmd 0x%02X failed: %v", c.cmd, err)
			continue
		}

		parsed, err := ParseSamsungResponse(resp)
		if err != nil {
			continue
		}

		if parsed.Serial != "" {
			data.Serial = parsed.Serial
		}
		data.TonerModels = append(data.TonerModels, parsed.TonerModels...)
		if parsed.Status != "" {
			data.Status = parsed.Status
		}
		data.Alerts = append(data.Alerts, parsed.Alerts...)
		data.SuppliesBin = append(data.SuppliesBin, parsed.SuppliesBin...)
	}

	return data, nil
}

// Get1284DeviceID obtiene la cadena de identificación IEEE-1284 del dispositivo (IOCTL_USBPRINT_GET_1284_ID)
func Get1284DeviceID(devicePath string) (string, error) {
	pathW, err := syscall.UTF16PtrFromString(devicePath)
	if err != nil {
		return "", err
	}

	handle, err := syscall.CreateFile(
		pathW,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		0,
		nil,
		syscall.OPEN_EXISTING,
		0,
		0,
	)
	if err != nil {
		return "", fmt.Errorf("CreateFile error: %w", err)
	}
	defer syscall.CloseHandle(handle)

	const IOCTL_USBPRINT_GET_1284_ID = 0x220038
	outBuf := make([]byte, 1024)
	var bytesReturned uint32

	err = syscall.DeviceIoControl(
		handle,
		IOCTL_USBPRINT_GET_1284_ID,
		nil,
		0,
		&outBuf[0],
		uint32(len(outBuf)),
		&bytesReturned,
		nil,
	)
	if err != nil {
		return "", fmt.Errorf("DeviceIoControl error: %w", err)
	}

	// El formato IEEE-1284 devuelve un string que puede tener una longitud de 2 bytes al inicio.
	if bytesReturned > 2 {
		return string(outBuf[2:bytesReturned]), nil
	}
	return string(outBuf[:bytesReturned]), nil
}
