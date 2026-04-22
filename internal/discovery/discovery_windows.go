package discovery

import (
	"encoding/json"
	"log"
	"os/exec"
	"strings"
)

// USBPrinter representa una impresora detectada en el sistema vía puerto USB.
type USBPrinter struct {
	Name     string
	PortName string // ej: USB001, USB002
	Driver   string
}

// FindUSBPrinters usa WMI (vía PowerShell) para enumerar impresoras con puerto USB activo.
func FindUSBPrinters() []USBPrinter {
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
		`Get-WmiObject Win32_Printer | `+
			`Where-Object { $_.PortName -like 'USB*' -and $_.WorkOffline -eq $false } | `+
			`Select-Object Name, PortName, DriverName | `+
			`ConvertTo-Json -Compress`)

	out, err := cmd.Output()
	if err != nil {
		log.Printf("[Discovery] Error ejecutando WMI: %v", err)
		return nil
	}

	raw := strings.TrimSpace(string(out))
	if raw == "" || raw == "null" {
		return nil
	}

	return parseWMIOutput(raw)
}

func parseWMIOutput(raw string) []USBPrinter {
	type wmiPrinter struct {
		Name       string `json:"Name"`
		PortName   string `json:"PortName"`
		DriverName string `json:"DriverName"`
	}

	var printers []USBPrinter

	if strings.HasPrefix(raw, "[") {
		var tmp []wmiPrinter
		if err := json.Unmarshal([]byte(raw), &tmp); err == nil {
			for _, p := range tmp {
				printers = append(printers, USBPrinter{
					Name:     p.Name,
					PortName: p.PortName,
					Driver:   p.DriverName,
				})
			}
		}
	} else {
		var tmp wmiPrinter
		if err := json.Unmarshal([]byte(raw), &tmp); err == nil {
			printers = append(printers, USBPrinter{
				Name:     tmp.Name,
				PortName: tmp.PortName,
				Driver:   tmp.DriverName,
			})
		}
	}

	return printers
}
