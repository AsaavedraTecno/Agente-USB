package winsvc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"usb-agent/internal/agent"
	"usb-agent/internal/config"
	"usb-agent/internal/uploader"
)

// maxRateLimitBackoff topea la espera tras un 429 de /agent/config -- sube al
// doble en cada 429 consecutivo (arrancando en tickInterval) hasta acá, para
// no terminar esperando días si el rate limit no se libera solo.
const maxRateLimitBackoff = 4 * time.Hour

// controlInterval es cada cuánto se pregunta /api/agent/config (kill-switch +
// intervalo), independiente de cada cuánto se sondea la impresora de verdad.
// 30 min, no 60s como AgenteSNMP: /agent/config tiene throttle:120,1 (por IP,
// no por agent key) — a 60s, ~120 agentes detrás de la misma IP ya lo rompen,
// un escenario plausible para USB (muchas notebooks en un mismo edificio).
// A 30 min hacen falta ~3600 agentes por IP para romperlo. Ver
// notas/2026-09-04_config-nube-y-flujo-servicio.md.
const controlInterval = 30 * time.Minute

const (
	ServiceName    = "AgenteUSB"
	ServiceDisplay = "Agente USB Monitor"
	ServiceDesc    = "Monitoreo periódico de impresoras USB - TDMonitor"
	InstallDir     = `C:\ProgramData\AgenteUSB`
)

// Install copia el exe + config a ProgramData e instala el servicio de Windows.
// Requiere privilegios de administrador.
func Install() error {
	if err := os.MkdirAll(InstallDir, 0755); err != nil {
		return fmt.Errorf("crear directorio %s: %w", InstallDir, err)
	}

	exeSrc, err := os.Executable()
	if err != nil {
		return fmt.Errorf("obtener ruta exe: %w", err)
	}
	exeDst := filepath.Join(InstallDir, "agent-usb.exe")
	if err := copyFile(exeSrc, exeDst); err != nil {
		return fmt.Errorf("copiar exe: %w", err)
	}

	// Copiar agent.yaml si existe junto al exe fuente
	cfgSrc := filepath.Join(filepath.Dir(exeSrc), "agent.yaml")
	cfgDst := filepath.Join(InstallDir, "agent.yaml")
	if _, err := os.Stat(cfgSrc); err == nil {
		_ = copyFile(cfgSrc, cfgDst)
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("conectar SCM: %w", err)
	}
	defer m.Disconnect()

	// Eliminar si ya existe
	if s, err := m.OpenService(ServiceName); err == nil {
		_, _ = s.Control(svc.Stop)
		time.Sleep(time.Second)
		_ = s.Delete()
		s.Close()
	}

	s, err := m.CreateService(ServiceName, exeDst, mgr.Config{
		DisplayName:      ServiceDisplay,
		Description:      ServiceDesc,
		StartType:        mgr.StartAutomatic,
		ServiceStartName: "LocalSystem", // corre como SYSTEM, sin depender de ningún usuario
	}, "--service")
	if err != nil {
		return fmt.Errorf("crear servicio: %w", err)
	}
	defer s.Close()

	return s.Start()
}

// Uninstall detiene y elimina el servicio.
func Uninstall() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("conectar SCM: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(ServiceName)
	if err != nil {
		return fmt.Errorf("servicio no instalado: %w", err)
	}
	defer s.Close()

	_, _ = s.Control(svc.Stop)
	time.Sleep(2 * time.Second)
	return s.Delete()
}

// Status devuelve el estado actual del servicio.
func Status() string {
	m, err := mgr.Connect()
	if err != nil {
		return "error"
	}
	defer m.Disconnect()

	s, err := m.OpenService(ServiceName)
	if err != nil {
		return "no instalado"
	}
	defer s.Close()

	st, err := s.Query()
	if err != nil {
		return "error"
	}
	switch st.State {
	case svc.Running:
		return "ejecutando"
	case svc.Stopped:
		return "detenido"
	case svc.StartPending:
		return "iniciando..."
	case svc.StopPending:
		return "deteniendo..."
	default:
		return "desconocido"
	}
}

// Run ejecuta el bucle principal del servicio (llamado por el SCM de Windows).
func Run() error {
	return svc.Run(ServiceName, &usbService{})
}

// ── Implementación del servicio ───────────────────────────────────────────────

type usbService struct{}

func (s *usbService) Execute(_ []string, r <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}

	cfg := config.LoadPortable()
	dataInterval := time.Duration(cfg.IntervalMinutes) * time.Minute
	if dataInterval < time.Minute {
		dataInterval = 30 * time.Minute
	}

	// El ticker corre siempre al intervalo "fino" (controlInterval) y nunca se
	// resetea — lo único que cambia es ticksPerCycle, que decide cada cuántos
	// ticks toca hacer el trabajo pesado (sondear la impresora). Así, chequear
	// active/scan_interval es barato y frecuente sin tocar la impresora más
	// seguido de lo configurado. Ver notas/2026-09-04_config-nube-y-flujo-servicio.md.
	tickInterval := controlInterval
	if dataInterval < tickInterval {
		tickInterval = dataInterval // interval_minutes corto: no tiene sentido chequear más seguido que eso
	}
	ticksPerCycle := int(dataInterval / tickInterval)
	if ticksPerCycle < 1 {
		ticksPerCycle = 1
	}
	tick := 0

	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}

	// Correr inmediatamente al iniciar, igual que hoy
	go runCycle(cfg)

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	// Backoff exponencial ante 429 de /agent/config -- ver
	// notas/2026-09-04_config-nube-y-flujo-servicio.md, "Backoff en 429".
	var rateLimitBackoff time.Duration
	var rateLimitUntil time.Time

	for {
		select {
		case <-ticker.C:
			cfg = config.LoadPortable() // Recargar config local en cada tick

			active := true // fail-open: si no se puede consultar la nube, seguir monitoreando
			if now := time.Now(); now.Before(rateLimitUntil) {
				logControl("en backoff por rate limit — próximo intento %s", rateLimitUntil.Format("15:04:05"))
			} else if rc, err := uploader.FetchConfig(cfg.ServerURL, cfg.APIKey, cfg.AgentID, cfg.SkipTLSVerify); err != nil {
				if errors.Is(err, uploader.ErrRateLimited) {
					if rateLimitBackoff == 0 {
						rateLimitBackoff = tickInterval
					}
					rateLimitBackoff *= 2
					if rateLimitBackoff > maxRateLimitBackoff {
						rateLimitBackoff = maxRateLimitBackoff
					}
					rateLimitUntil = now.Add(rateLimitBackoff)
					logControl("/agent/config respondió 429 — esperando %v antes de reintentar", rateLimitBackoff)
				} else {
					logControl("consulta a /agent/config falló (%v) — se sigue con la config local", err)
				}
			} else {
				rateLimitBackoff = 0
				rateLimitUntil = time.Time{}
				active = rc.Active
				if rc.ScanInterval > 0 {
					newDataInterval := time.Duration(rc.ScanInterval) * time.Second
					if newDataInterval != dataInterval {
						dataInterval = newDataInterval
						newTicksPerCycle := int(dataInterval / tickInterval)
						if newTicksPerCycle < 1 {
							newTicksPerCycle = 1
						}
						ticksPerCycle = newTicksPerCycle
					}
				}
			}

			tick++
			if tick >= ticksPerCycle {
				tick = 0
				if !active {
					logControl("agente pausado desde la nube (active=false) — se saltea este ciclo")
				} else {
					go runCycle(cfg)
				}
			}
		case c := <-r:
			switch c.Cmd {
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				return false, 0
			}
		}
	}
}

// logControl deja un rastro del plano de control en agent.log, con el mismo
// formato/archivo que runCycle — para que la pestaña Logs de la GUI lo vea.
func logControl(format string, args ...interface{}) {
	LogEvent("control", format, args...)
}

// LogEvent escribe una línea en el mismo agent.log que usa runCycle, con un
// tag para distinguir el origen (ej. "control", "gui"). Exportado para que la
// GUI pueda dejar rastro de acciones manuales (ej. "Probar Conexión") en el
// mismo lugar que ya mira la pestaña Logs — una sola fuente de verdad.
func LogEvent(tag, format string, args ...interface{}) {
	f, err := openLogAppend()
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "[%s] [%s] %s\n", time.Now().Format("15:04:05"), tag, fmt.Sprintf(format, args...))
}

// maxLogSize es el tamaño a partir del cual agent.log rota -- sin esto crece
// para siempre (O_APPEND desde que arranca el servicio, sin límite). No hace
// falta una librería (lumberjack, que sí usa AgenteSNMP): un solo backup
// alcanza para un log de texto simple que nadie necesita conservar más de
// una vuelta.
const maxLogSize = 10 * 1024 * 1024 // 10 MB

// openLogAppend rota agent.log a agent.log.old si superó maxLogSize y
// devuelve el archivo abierto para append. Única entrada compartida por
// LogEvent y RunCycle -- así la rotación no depende de tocar cada caller.
func openLogAppend() (*os.File, error) {
	logPath := filepath.Join(config.ExeDir(), "agent.log")

	if info, err := os.Stat(logPath); err == nil && info.Size() >= maxLogSize {
		backup := logPath + ".old"
		_ = os.Remove(backup)
		_ = os.Rename(logPath, backup)
	}

	return os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
}

func runCycle(cfg *config.Config) {
	_ = RunCycle(cfg)
}

// RunCycle ejecuta un ciclo de sondeo+envío igual que el ticker del servicio,
// pero de forma síncrona y devolviendo el error — para que la GUI pueda
// ofrecer "Extraer Datos Ahora" (botón manual, igual que "Probar Conexión"
// pero para el sondeo real) sin esperar al próximo tick.
func RunCycle(cfg *config.Config) error {
	f, err := openLogAppend()
	if err != nil {
		return err
	}
	defer f.Close()

	ts := time.Now().Format("2006-01-02 15:04:05")
	fmt.Fprintf(f, "\n[%s] ═══ Inicio de ciclo ═══\n", ts)

	runErr := agent.Run(cfg, func(line string) {
		if !strings.HasPrefix(line, "__") {
			fmt.Fprintf(f, "[%s] %s\n", time.Now().Format("15:04:05"), line)
		}
	})

	fmt.Fprintf(f, "[%s] ═══ Fin de ciclo ═══\n", time.Now().Format("15:04:05"))
	return runErr
}

// ── Utilidades ────────────────────────────────────────────────────────────────

// RunElevated re-lanza el exe actual con privilegios de administrador (UAC).
func RunElevated(args string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}

	verb, _ := syscall.UTF16PtrFromString("runas")
	file, _ := syscall.UTF16PtrFromString(exe)
	params, _ := syscall.UTF16PtrFromString(args)
	dir, _ := syscall.UTF16PtrFromString(filepath.Dir(exe))

	shell32 := syscall.NewLazyDLL("shell32.dll")
	shellExecuteW := shell32.NewProc("ShellExecuteW")

	ret, _, _ := shellExecuteW.Call(
		0,
		uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(file)),
		uintptr(unsafe.Pointer(params)),
		uintptr(unsafe.Pointer(dir)),
		1, // SW_SHOWNORMAL
	)
	if ret <= 32 {
		return fmt.Errorf("ShellExecute falló (código %d)", ret)
	}
	return nil
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0755)
}
