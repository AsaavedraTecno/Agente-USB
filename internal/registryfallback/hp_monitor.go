package registryfallback

import (
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/yusufpapurcu/wmi"
	"golang.org/x/sys/windows/registry"
	"usb-agent/internal/payload"
)

type win32Printer struct {
	Name                  string
	ExtendedPrinterStatus uint16
	DetectedErrorState    uint16
	PrinterState          uint32
	StatusInfo            uint16
}

type msftPrinter struct {
	Name         string
	PrinterState uint32
}

// TryHPMonitorProxy investiga si hay datos dejados por el monitor de estado de HP.
// Retorna las listas de suministros y contadores si las encuentra.
func TryHPMonitorProxy(serial string) ([]payload.Supply, *payload.Counters, error) {
	log.Println("[HP-Proxy] Iniciando investigación de datos del Monitor HP...")
	var supplies []payload.Supply
	var counters *payload.Counters

	// 1. Escanear WMI
	scanWMI()

	// 2. Escanear Registro
	scanRegistry(serial)

	// 3. Escanear IPC/Procesos
	scanIPC()

	// 4. Escanear AppData (HP Smart / Status Monitor DBs)
	scanAppDataHP()

	// NOTA: Como este es un script de investigación, los datos no se extraen automáticamente
	// aún hacia supplies/counters hasta que se analicen los logs generados en el cliente.
	// Si encontramos el patrón exacto luego, se puede asignar aquí.

	return supplies, counters, nil
}

func scanWMI() {
	log.Println("[HP-Proxy] Escaneando WMI (Win32_Printer)...")
	var printers []win32Printer
	q := "SELECT Name, ExtendedPrinterStatus, DetectedErrorState, PrinterState, StatusInfo FROM Win32_Printer WHERE Name LIKE '%HP%408%'"
	err := wmi.Query(q, &printers)
	if err != nil {
		log.Printf("[HP-Proxy] Error WMI Win32_Printer: %v", err)
	} else {
		if len(printers) == 0 {
			log.Println("[HP-Proxy] No se encontraron impresoras HP 408 en Win32_Printer.")
		}
		for _, p := range printers {
			log.Printf("[HP-Proxy] WMI Win32_Printer -> Name: %s, ExtStatus: %d, ErrState: %d, State: %d, StatusInfo: %d", p.Name, p.ExtendedPrinterStatus, p.DetectedErrorState, p.PrinterState, p.StatusInfo)
		}
	}

	// PerfFormattedData_Spooler_PrintQueue
	type printQueue struct {
		Name              string
		TotalPagesPrinted uint32
	}
	var queues []printQueue
	q2 := "SELECT Name, TotalPagesPrinted FROM Win32_PerfFormattedData_Spooler_PrintQueue WHERE Name LIKE '%408%'"
	err = wmi.Query(q2, &queues)
	if err == nil {
		for _, q := range queues {
			log.Printf("[HP-Proxy] WMI PrintQueue -> Name: %s, TotalPagesPrinted: %d", q.Name, q.TotalPagesPrinted)
		}
	}
}

func scanRegistry(serial string) {
	log.Println("[HP-Proxy] Escaneando Registry (HKLM\\SOFTWARE\\HP y Hewlett-Packard)...")
	bases := []string{`SOFTWARE\HP`, `SOFTWARE\Hewlett-Packard`}
	for _, base := range bases {
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, base, registry.ENUMERATE_SUB_KEYS|registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		walkRegistry(k, `HKLM\`+base, serial, 0)
		k.Close()
	}
}

func walkRegistry(k registry.Key, path, serial string, depth int) {
	// Limitar profundidad para evitar ciclos o búsquedas eternas
	if depth > 5 {
		return
	}

	names, err := k.ReadValueNames(0)
	if err == nil {
		for _, name := range names {
			lowerName := strings.ToLower(name)
			// Buscamos valores que nos den información útil
			if strings.Contains(lowerName, "toner") || strings.Contains(lowerName, "level") ||
				strings.Contains(lowerName, "page") || strings.Contains(lowerName, "count") ||
				strings.Contains(lowerName, "status") || strings.Contains(lowerName, "supply") {

				valStr, _, errStr := k.GetStringValue(name)
				if errStr == nil {
					log.Printf("[HP-Proxy] Reg Value %s\\%s = %s", path, name, valStr)
				} else {
					valInt, _, errInt := k.GetIntegerValue(name)
					if errInt == nil {
						log.Printf("[HP-Proxy] Reg Value %s\\%s = %d", path, name, valInt)
					}
				}
			} else if serial != "" && serial != "?" {
				// Buscar si el serial está en el nombre o valor
				valStr, _, _ := k.GetStringValue(name)
				if strings.Contains(lowerName, strings.ToLower(serial)) || strings.Contains(strings.ToLower(valStr), strings.ToLower(serial)) {
					log.Printf("[HP-Proxy] Reg Serial Match! %s\\%s = %s", path, name, valStr)
				}
			}
		}
	}

	subkeys, err := k.ReadSubKeyNames(0)
	if err == nil {
		for _, sub := range subkeys {
			subK, err := registry.OpenKey(k, sub, registry.ENUMERATE_SUB_KEYS|registry.QUERY_VALUE)
			if err == nil {
				walkRegistry(subK, path+`\`+sub, serial, depth+1)
				subK.Close()
			}
		}
	}
}

func scanIPC() {
	log.Println("[HP-Proxy] Escaneando procesos y Named Pipes...")

	// Escanear pipes
	// Listar archivos en \\.\pipe\ es posible con filepath.Glob
	matches, _ := filepath.Glob(`\\.\pipe\*`)
	foundHP := false
	for _, pipe := range matches {
		if strings.Contains(strings.ToLower(pipe), "hp") {
			log.Printf("[HP-Proxy] Named Pipe encontrado: %s", pipe)
			foundHP = true
		}
	}
	if !foundHP {
		log.Println("[HP-Proxy] No se encontraron Named Pipes relacionados con HP.")
	}

	// Archivos temporales de HP
	tempDir := os.TempDir()
	hpTemp := filepath.Join(tempDir, "HP")
	if info, err := os.Stat(hpTemp); err == nil && info.IsDir() {
		log.Printf("[HP-Proxy] Directorio temporal HP encontrado en: %s", hpTemp)
	}

	// ProgramData HP
	programData := os.Getenv("PROGRAMDATA")
	if programData != "" {
		hpData := filepath.Join(programData, "HP")
		if info, err := os.Stat(hpData); err == nil && info.IsDir() {
			log.Printf("[HP-Proxy] Directorio ProgramData HP encontrado en: %s", hpData)
		}
	}
}

func scanAppDataHP() {
	log.Println("[HP-Proxy] Escaneando LocalAppData por bases de datos de HP...")
	localApp := os.Getenv("LOCALAPPDATA")
	if localApp == "" {
		return
	}

	// Posibles directorios de utilidades HP
	targets := []string{
		filepath.Join(localApp, "HP"),
		filepath.Join(localApp, "Hewlett-Packard"),
		filepath.Join(localApp, "Packages"), // HP Smart UWP (AD2F1837.HPPrinterControl...)
	}

	for _, t := range targets {
		if _, err := os.Stat(t); os.IsNotExist(err) {
			continue
		}

		err := filepath.Walk(t, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if !info.IsDir() {
				ext := strings.ToLower(filepath.Ext(path))
				// Buscar archivos de base de datos o configuraciones
				if ext == ".db" || ext == ".sqlite" || ext == ".json" || ext == ".xml" {
					lowerPath := strings.ToLower(path)
					if strings.Contains(lowerPath, "hp") || strings.Contains(lowerPath, "smart") || strings.Contains(lowerPath, "printer") {
						// Leer los primeros bytes o solo reportar si es muy grande
						if info.Size() < 5*1024*1024 { // < 5MB
							content, err := os.ReadFile(path)
							if err == nil {
								// Búsqueda heurística de "toner", "level", "page"
								str := strings.ToLower(string(content))
								if strings.Contains(str, "toner") || strings.Contains(str, "pagecount") {
									log.Printf("[HP-Proxy] ALERTA: Posible DB con telemetría encontrada en: %s (Size: %d)", path, info.Size())
								}
							}
						} else {
							log.Printf("[HP-Proxy] DB muy grande encontrada (posible caché HP): %s", path)
						}
					}
				}
			}
			return nil
		})
		if err != nil {
			log.Printf("[HP-Proxy] Error recorriendo %s: %v", t, err)
		}
	}
}
