package usbraw

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

type BruteForceResult struct {
	TotalCommands int
	XMLCount      int
	BinaryCount   int
	ASCIICount    int
	Errors        int
	Results       []CommandResult
}

type CommandResult struct {
	Command    byte   `json:"command"`
	Format     string `json:"format"`
	Response   []byte `json:"-"`
	Length     int    `json:"length"`
	Type       string `json:"type"`
	XMLRoot    string `json:"xml_root,omitempty"`
	HasNumber  bool   `json:"has_number"`
	HasCounter bool   `json:"has_counter"`
	Preview    string `json:"preview"`
}

func BruteForceSamsungCommands(devicePath string) {
	log.Println("[BRUTE-FORCE] Starting Samsung command scan...")
	log.Printf("[BRUTE-FORCE] Device: %s\n", devicePath)

	var bfResult BruteForceResult
	knownCommands := map[byte]bool{
		0x01: true, 0x02: true, 0x03: true, 0x0b: true,
		0x0c: true, 0x0d: true, 0x0e: true, 0x0f: true,
		0x10: true, 0x11: true, 0x05: true, 0x49: true,
	}

	for i := 1; i <= 255; i++ {
		b := byte(i)
		if knownCommands[b] {
			log.Printf("[BRUTE-FORCE] CMD 0x%02X → Known command, skipping", b)
			continue
		}

		cmdA := []byte{0x0d, 0x00, b} // bRequest = 0x0D, wValue = (0x00 << 8) | b
		cmdB := []byte{b, 0x00, 0x00} // bRequest = b, wValue = 0

		resA := testCommand(devicePath, b, "A", cmdA)
		bfResult.Results = append(bfResult.Results, resA)
		logResult(resA)

		resB := testCommand(devicePath, b, "B", cmdB)
		bfResult.Results = append(bfResult.Results, resB)
		logResult(resB)

		updateStats(&bfResult, resA)
		updateStats(&bfResult, resB)
	}

	saveJSON("samsung_bruteforce_results.json", bfResult.Results)
	printSummary(bfResult)
}

func sendVendorGetCommand(devicePath string, inBuf []byte, outBufLen uint32) ([]byte, error) {
	pathW, err := syscall.UTF16PtrFromString(devicePath)
	if err != nil {
		return nil, err
	}

	h, err := syscall.CreateFile(
		pathW,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
		nil,
		syscall.OPEN_EXISTING,
		0,
		0,
	)
	if err != nil {
		return nil, err
	}
	defer syscall.CloseHandle(h)

	const IOCTL_USBPRINT_VENDOR_GET_COMMAND = 0x22003C
	outBuf := make([]byte, outBufLen)
	var bytesReturned uint32

	err = syscall.DeviceIoControl(
		h,
		IOCTL_USBPRINT_VENDOR_GET_COMMAND,
		&inBuf[0],
		uint32(len(inBuf)),
		&outBuf[0],
		uint32(len(outBuf)),
		&bytesReturned,
		nil,
	)
	if err != nil {
		return nil, err
	}

	return outBuf[:bytesReturned], nil
}

func testCommand(path string, cmd byte, format string, payload []byte) CommandResult {
	res := CommandResult{
		Command: cmd,
		Format:  format,
	}

	// Samsung typical XML responses are ~255 bytes or less, but we allow up to 1024
	fresh, err := sendVendorGetCommand(path, payload, 1024)
	if err != nil {
		res.Type = "error"
		res.Preview = err.Error()
		return res
	}

	res.Response = fresh
	res.Length = len(fresh)

	if res.Length == 0 {
		res.Type = "empty"
		return res
	}

	saveRaw(fmt.Sprintf("samsung_cmd_%02X_format_%s.raw", cmd, format), fresh)
	classifyResponse(&res)
	findNumbers(&res)
	return res
}

func classifyResponse(res *CommandResult) {
	data := res.Response

	// Intentar detectar XML
	strData := string(data)
	idxStart := strings.Index(strData, "<")
	if idxStart >= 0 {
		idxEnd := strings.Index(strData[idxStart:], ">")
		if idxEnd > 0 {
			res.Type = "xml"
			root := strData[idxStart+1 : idxStart+idxEnd]
			res.XMLRoot = strings.Fields(root)[0] // Tomar primer nombre sin attrs
			res.Preview = previewString(strData, 80)
			return
		}
	}

	// Detectar ASCII
	isAscii := true
	for _, b := range data {
		if b < 0x09 || b > 0x7E {
			isAscii = false
			break
		}
	}

	if isAscii {
		res.Type = "ascii"
		res.Preview = previewString(strData, 80)
		return
	}

	// Por defecto es binario
	res.Type = "binary"
	res.Preview = previewHex(data, 16)
}

func findNumbers(res *CommandResult) {
	data := res.Response

	if res.Type == "xml" || res.Type == "ascii" {
		strData := string(data)
		// Buscar números entre > y < o entre comillas
		numRegex := regexp.MustCompile(`>(\d+)<|="(\d+)"`)
		matches := numRegex.FindAllStringSubmatch(strData, -1)
		for _, m := range matches {
			numStr := m[1]
			if numStr == "" {
				numStr = m[2]
			}
			val, err := strconv.Atoi(numStr)
			if err == nil {
				if val >= 0 && val <= 100 {
					res.HasNumber = true
				}
				if val > 1000 {
					res.HasCounter = true
				}
			}
		}
	} else if res.Type == "binary" {
		// Heurística simple: buscar números 0-100 como bytes
		for _, b := range data {
			if b > 0 && b <= 100 {
				res.HasNumber = true
			}
			// Imposible detectar >1000 fiablemente sin saber endianness, omitido.
		}
	}
}

func previewString(s string, max int) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

func previewHex(data []byte, max int) string {
	if len(data) > max {
		data = data[:max]
	}
	var hex []string
	for _, b := range data {
		hex = append(hex, fmt.Sprintf("%02X", b))
	}
	s := strings.Join(hex, " ")
	if len(data) == max {
		s += "..."
	}
	return s
}

func logResult(res CommandResult) {
	var numStr string
	if res.HasNumber {
		numStr += " [POSSIBLE %]"
	}
	if res.HasCounter {
		numStr += " [POSSIBLE COUNTER]"
	}

	if res.Type == "xml" {
		log.Printf("[BRUTE-FORCE] CMD 0x%02X Format %s → XML <%s> [%d bytes]%s", res.Command, res.Format, res.XMLRoot, res.Length, numStr)
	} else if res.Type == "empty" {
		// Loggear empty es ruidoso, omitido para no llenar la pantalla
	} else {
		log.Printf("[BRUTE-FORCE] CMD 0x%02X Format %s → %s [%d bytes] - %s%s", res.Command, res.Format, strings.ToUpper(res.Type), res.Length, res.Preview, numStr)
	}
}

func updateStats(bf *BruteForceResult, res CommandResult) {
	bf.TotalCommands++
	switch res.Type {
	case "xml":
		bf.XMLCount++
	case "binary":
		bf.BinaryCount++
	case "ascii":
		bf.ASCIICount++
	case "error":
		bf.Errors++
	}
}

func saveJSON(filename string, data interface{}) {
	b, err := json.MarshalIndent(data, "", "  ")
	if err == nil {
		os.WriteFile(filename, b, 0644)
	}
}

func saveRaw(filename string, data []byte) {
	os.WriteFile(filename, data, 0644)
}

func printSummary(bf BruteForceResult) {
	log.Println("\n[BRUTE-FORCE] SUMMARY:")

	var xmlCmds []string
	var binCmds []string
	var pctCmds []string
	var cntCmds []string
	var keyCmds []string

	keywords := []string{"toner", "supply", "level", "percent", "count", "page"}

	for _, r := range bf.Results {
		if r.Type == "xml" {
			xmlCmds = append(xmlCmds, fmt.Sprintf("0x%02X(%s)", r.Command, r.Format))
		}
		if r.Type == "binary" {
			binCmds = append(binCmds, fmt.Sprintf("0x%02X(%s)", r.Command, r.Format))
		}
		if r.HasNumber {
			pctCmds = append(pctCmds, fmt.Sprintf("0x%02X(%s)", r.Command, r.Format))
		}
		if r.HasCounter {
			cntCmds = append(cntCmds, fmt.Sprintf("0x%02X(%s)", r.Command, r.Format))
		}

		strData := strings.ToLower(string(r.Response))
		for _, kw := range keywords {
			if strings.Contains(strData, kw) {
				keyCmds = append(keyCmds, fmt.Sprintf("0x%02X(%s)", r.Command, r.Format))
				break
			}
		}
	}

	log.Printf("  XML commands: %s", strings.Join(xmlCmds, ", "))
	log.Printf("  Binary commands: %s", strings.Join(binCmds, ", "))
	log.Printf("  Possible percentages: %s", strings.Join(pctCmds, ", "))
	log.Printf("  Possible counters: %s", strings.Join(cntCmds, ", "))
	log.Printf("  Commands with keywords: %s", strings.Join(keyCmds, ", "))
	log.Printf("  Saved to: samsung_bruteforce_results.json and .raw files")
}
