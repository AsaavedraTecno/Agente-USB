package hpprotocol

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strings"
	"syscall"
)

// Constantes para el Control Transfer (XML Streaming)
const (
	IOCTL_USBPRINT_VENDOR_GET_COMMAND = 0x22003C
	XmlChunkSize                      = 255
)

// ---------------------------------------------------------
// ESTRUCTURAS DE PARSEO XML
// ---------------------------------------------------------

type StatusMonitorInfo struct {
	XMLName        xml.Name       `xml:"StatusMonitorInfo"`
	Capabilities   Capabilities   `xml:"Capabilities"`
	PaperSizeTable PaperSizeTable `xml:"PaperSizeTable"`
	TonerOrder     TonerOrder     `xml:"TonerOrder"`
	GeneralMSG     GeneralMSG     `xml:"GeneralMSG"`
	Status         Status         `xml:"Status"`
}

type Capabilities struct {
	PSU string `xml:"PSU,attr"`
}

type PaperSizeTable struct {
	PaperSizes []PaperSize `xml:"PaperSize"`
}

type PaperSize struct {
	Name     string `xml:"name,attr"`
	SnmpCode string `xml:"snmpcode,attr"`
	UsbCode  string `xml:"usbcode,attr"`
	FeedDir  string `xml:"feeddir,attr"`
	XFeedDir string `xml:"xfeeddir,attr"`
}

type TonerOrder struct {
	Black TonerModel `xml:"Black"`
}

type TonerModel struct {
	String string `xml:"String,attr"`
	Model  string `xml:",chardata"` // ej: W1330A
}

// Para capturar cualquier mensaje/error activo
type GeneralMSG struct {
	Nodes []ActiveNode `xml:",any"`
}

type Status struct {
	Nodes []ActiveNode `xml:",any"`
}

type ActiveNode struct {
	XMLName           xml.Name
	String            string `xml:"STRING,attr"`
	GeneralMSG        string `xml:"GeneralMSG,attr"`
	TroubleshootingID string `xml:"TroubleshootingID,attr"`
	StandardCode      string `xml:"StandardCode,attr"`
	LowTonerStatus    string `xml:"LOWTONERSTATUS,attr"`
	Color             string `xml:"Color,attr"`
	Mask              string `xml:"Mask"`
	StatusBytes       string `xml:"Status"`
}

// ---------------------------------------------------------
// FUNCIONES DE EXTRACCIÓN Y PARSEO
// ---------------------------------------------------------

// FetchFullXML extrae el diccionario XML estático desde la impresora
func FetchFullXML(devicePath string) (string, error) {
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
		return "", fmt.Errorf("CreateFile error: %v", err)
	}
	defer syscall.CloseHandle(handle)

	var fullXML bytes.Buffer
	outBuf := make([]byte, XmlChunkSize)

	// El comando es bRequest=0x0D, y wValue(LSB) es el índice del chunk (1 a N).
	for i := 1; i <= 255; i++ {
		// 0x0D (bRequest), 0x00 (MSB), byte(i) (LSB) -> wValue = i
		inBuf := []byte{0x0D, 0x00, byte(i)}
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

		if err != nil || bytesReturned == 0 {
			break
		}

		// Limpiamos los bytes nulos que vienen al final del buffer
		chunk := string(bytes.TrimRight(outBuf[:bytesReturned], "\x00"))
		if chunk == "" {
			break
		}

		fullXML.WriteString(chunk)

		// Cortamos exactamente al encontrar el cierre, para no seguir pidiendo chunks y colgar la RAM
		if strings.Contains(chunk, "</StatusMonitorInfo>") {
			break
		}
	}

	result := fullXML.String()
	if result == "" {
		return "", fmt.Errorf("no se recibió XML desde la impresora")
	}

	return result, nil
}

type StateDefinition struct {
	Name         string
	String       string
	GeneralMSG   string
	StandardCode string
	Color        string
	Mask         [8]byte
	Expected     [8]byte
	HasMask      bool
}

// ParseStateDefinitions lee el XML estático (con regex para tolerar errores del firmware)
// y devuelve un mapa de definiciones
func ParseStateDefinitions(xmlRaw string) []StateDefinition {
	var defs []StateDefinition

	lines := strings.Split(xmlRaw, "<")
	var currentDef *StateDefinition

	for _, line := range lines {
		// Detectamos un nuevo nodo que tiene STRING=
		if strings.Contains(line, "STRING=\"") {
			// Es el inicio de un estado (ej: Ready, Printing, TonerWarn...)
			name := strings.TrimSpace(strings.Split(line, " ")[0])
			if name == "" {
				name = "Unknown"
			}

			currentDef = &StateDefinition{Name: name}
			currentDef.String = extractAttr(line, "STRING=\"")
			currentDef.GeneralMSG = extractAttr(line, "GeneralMSG=\"")
			currentDef.StandardCode = extractAttr(line, "StandardCode=\"")
			currentDef.Color = extractAttr(line, "Color=\"")

			// Máscara por defecto si no viene en el XML (solo comparamos el primer byte o asumimos 00 en todo)
			// Según tu captura, si no hay <Mask>, por defecto todos son [00] (coincidencia exacta)
			currentDef.Mask = [8]byte{0, 0, 0, 0, 0, 0, 0, 0}
			currentDef.HasMask = false
		} else if strings.HasPrefix(line, "Mask>") && currentDef != nil {
			// Parsear Mask (sin buscar "</" porque hicimos split por "<")
			maskStr := strings.TrimSpace(line[5:])
			maskBytes := parseStatusBytes(maskStr)
			if len(maskBytes) == 8 {
				copy(currentDef.Mask[:], maskBytes)
				currentDef.HasMask = true
			}
		} else if strings.HasPrefix(line, "Status>") && currentDef != nil {
			// Parsear Status
			statusStr := strings.TrimSpace(line[7:])
			statusBytes := parseStatusBytes(statusStr)
			if len(statusBytes) == 8 {
				copy(currentDef.Expected[:], statusBytes)
				defs = append(defs, *currentDef)
			}
			currentDef = nil // Cerramos este nodo
		}
	}
	return defs
}

func extractAttr(line, prefix string) string {
	idx := strings.Index(line, prefix)
	if idx == -1 {
		return ""
	}
	start := idx + len(prefix)
	end := strings.Index(line[start:], "\"")
	if end == -1 {
		return ""
	}
	return line[start : start+end]
}

// parseStatusBytes convierte el formato "[82][05][B7][00]..." a []byte
func parseStatusBytes(statusStr string) []byte {
	var out []byte
	clean := strings.ReplaceAll(statusStr, "][", ",")
	clean = strings.ReplaceAll(clean, "[", "")
	clean = strings.ReplaceAll(clean, "]", "")
	parts := strings.Split(clean, ",")

	for _, p := range parts {
		var b byte
		fmt.Sscanf(p, "%02X", &b)
		out = append(out, b)
	}
	return out
}

// ParseStatusXML decodifica el string XML en estructuras de Go
func ParseStatusXML(xmlData string) (*StatusMonitorInfo, error) {
	// A veces la impresora intercala basura o corta mal el encoding. Limpiamos:
	startIdx := strings.Index(xmlData, "<?xml")
	if startIdx == -1 {
		startIdx = strings.Index(xmlData, "<StatusMonitorInfo")
	}

	if startIdx != -1 {
		xmlData = xmlData[startIdx:]
	}

	endIdx := strings.Index(xmlData, "</StatusMonitorInfo>")
	if endIdx != -1 {
		xmlData = xmlData[:endIdx+len("</StatusMonitorInfo>")]
	}

	var info StatusMonitorInfo
	err := xml.Unmarshal([]byte(xmlData), &info)
	if err != nil {
		return nil, fmt.Errorf("error decodificando XML: %v", err)
	}

	return &info, nil
}

// GetActiveErrors escanea los nodos del XML buscando alertas reales (PaperJams, Errores de Fuser, etc)
func (info *StatusMonitorInfo) GetActiveErrors() []string {
	var errors []string

	for _, node := range info.GeneralMSG.Nodes {
		if node.XMLName.Local != "None" && node.XMLName.Local != "Ready" {
			errors = append(errors, fmt.Sprintf("[%s] %s", node.StandardCode, node.String))
		}
	}

	for _, node := range info.Status.Nodes {
		if node.GeneralMSG != "Ready" && node.GeneralMSG != "PowerSave" && node.GeneralMSG != "Printing" && node.GeneralMSG != "Waiting" {
			// Ignoramos nodos vacíos
			if node.String != "" {
				errors = append(errors, fmt.Sprintf("[%s] %s", node.StandardCode, node.String))
			}
		}
	}

	return errors
}
