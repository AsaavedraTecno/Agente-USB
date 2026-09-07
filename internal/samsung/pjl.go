package samsung

import (
	"fmt"
	"strings"
	"syscall"
	"time"
)

// SendPJLCommand envía un comando PJL raw a la impresora y lee su respuesta.
func SendPJLCommand(devicePath string, cmd string) (string, error) {
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

	pjlStr := fmt.Sprintf("\x1B%%-12345X@PJL %s\r\n\x1B%%-12345X", cmd)

	var bytesWritten uint32
	err = syscall.WriteFile(handle, []byte(pjlStr), &bytesWritten, nil)
	if err != nil {
		return "", fmt.Errorf("WriteFile error: %v", err)
	}

	// Pequeña pausa para que la impresora genere la respuesta
	time.Sleep(300 * time.Millisecond)

	outBuf := make([]byte, 4096)
	var fullResp strings.Builder

	// Leer con timeout para evitar bloqueos
	done := make(chan struct{})

	go func() {
		for i := 0; i < 3; i++ {
			var br uint32
			err := syscall.ReadFile(handle, outBuf, &br, nil)
			if err == nil && br > 0 {
				chunk := string(outBuf[:br])
				fullResp.WriteString(chunk)
				if strings.Contains(chunk, "\x0C") {
					break
				}
			} else {
				break
			}
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		// Timeout reached, just return whatever we have so far
	}

	return fullResp.String(), nil
}

// GetTotalImpressions envía el comando de PAGECOUNT para obtener contadores reales.
func GetTotalImpressions(devicePath string) int {
	resp, err := SendPJLCommand(devicePath, "INFO PAGECOUNT")
	if err != nil {
		return -1
	}

	// Buscar PAGECOUNT=XXXX
	lines := strings.Split(resp, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "PAGECOUNT=") {
			var count int
			fmt.Sscanf(line, "PAGECOUNT=%d", &count)
			return count
		} else if strings.HasPrefix(line, "TOTAL=") {
			// Por si la impresora responde con el formato extendido
			var count int
			fmt.Sscanf(line, "TOTAL=%d", &count)
			return count
		}
	}

	return -1
}

// GetPanelDisplay envía INFO STATUS para leer la pantalla LCD actual.
func GetPanelDisplay(devicePath string) string {
	resp, err := SendPJLCommand(devicePath, "INFO STATUS")
	if err != nil {
		return ""
	}

	lines := strings.Split(resp, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "DISPLAY=") {
			return line[8:]
		}
	}
	return ""
}
