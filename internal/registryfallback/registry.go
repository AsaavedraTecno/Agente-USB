package registryfallback

import (
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"usb-agent/internal/payload"
)

// Extract attempts to find toner levels from the Windows Registry
// specifically looking for Brother Status Monitor data.
func Extract(printerName string) ([]payload.Supply, error) {
	// PowerShell script to search HKCU\Software\Brother for Toner info
	// Buscamos propiedades que contengan Toner o Ink
	script := `
$paths = @("HKCU:\Software\Brother", "HKLM:\SOFTWARE\Brother")
$results = @()
foreach ($p in $paths) {
    if (Test-Path $p) {
        $items = Get-ChildItem -Path $p -Recurse -ErrorAction SilentlyContinue | Get-ItemProperty -ErrorAction SilentlyContinue
        foreach ($item in $items) {
            $props = $item.psobject.properties | Where-Object { $_.Name -match "Toner" -or $_.Name -match "Ink" -or $_.Name -match "Level" }
            foreach ($prop in $props) {
                if ($prop.Value -is [int] -or $prop.Value -is [string]) {
                    $results += "$($prop.Name)=$($prop.Value)"
                }
            }
        }
    }
}
$results | Select-Object -Unique
`

	psCmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	psCmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := psCmd.Output()
	if err != nil {
		return nil, err
	}

	raw := string(out)
	return parseRegistryOutput(raw), nil
}

func parseRegistryOutput(raw string) []payload.Supply {
	lines := strings.Split(raw, "\n")
	var supplies []payload.Supply

	// Buscar claves como "BlackTonerLevel=80" o "TonerStatus=100"
	re := regexp.MustCompile(`(?i)(black|cyan|magenta|yellow|toner).*?=(\d+)`)

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if match := re.FindStringSubmatch(line); match != nil {
			valStr := match[2]
			level, err := strconv.Atoi(valStr)
			// Status Monitor suele guardar porcentaje 0-100 o códigos, asumimos 0-100 si es <= 100
			if err == nil && level >= 0 && level <= 100 {
				color := strings.ToLower(match[1])
				if color == "toner" {
					color = "black"
				}

				// Evitar duplicados
				exists := false
				for _, s := range supplies {
					if s.Color == color {
						exists = true
						break
					}
				}

				if !exists {
					supplies = append(supplies, payload.Supply{
						Name:       "Toner (" + color + ")",
						Type:       "toner",
						Category:   "toner",
						Color:      color,
						Percentage: payload.Float64Ptr(float64(level)),
						Status:     getStatusForLevel(level),
					})
				}
			}
		}
	}

	return supplies
}

func getStatusForLevel(level int) string {
	if level <= 10 {
		return "cr\u00edtico"
	} else if level <= 25 {
		return "bajo"
	}
	return "ok"
}
