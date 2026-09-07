package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"usb-agent/internal/agent"
	"usb-agent/internal/config"
	"usb-agent/internal/gui"
	"usb-agent/internal/usbraw"
	"usb-agent/internal/winsvc"
)

func main() {
	arg := ""
	if len(os.Args) > 1 {
		arg = os.Args[1]
	}

	switch arg {
	case "--service":
		// Llamado por el SCM de Windows — ejecuta el bucle del servicio
		if err := winsvc.Run(); err != nil {
			log.Fatalf("[service] %v", err)
		}

	case "--install":
		// Instalar servicio (requiere admin, llamado via RunElevated desde la GUI)
		fmt.Println("Instalando servicio...")
		if err := winsvc.Install(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Servicio instalado y ejecutándose.")

	case "--uninstall":
		// Desinstalar servicio (requiere admin)
		fmt.Println("Desinstalando servicio...")
		if err := winsvc.Uninstall(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Servicio desinstalado.")

	case "--headless":
		// Ejecución manual sin GUI (para scripts o pruebas)
		runHeadless()

	case "--bruteforce":
		// Modo debug: iterar comandos Samsung por USB para descubrir nuevos endpoints
		log.SetFlags(log.Ltime)
		log.SetPrefix("[USB-Agent] ")
		fmt.Println("[BRUTE-FORCE] Buscando impresoras USB...")
		paths, err := usbraw.FindDevicePaths()
		if err != nil || len(paths) == 0 {
			log.Fatalf("[-] No se encontraron impresoras USB: %v\n", err)
		}
		usbraw.BruteForceSamsungCommands(paths[0])

	case "--httpprobe":
		// Modo debug: la E40040 respondió HTTP 400 (Virata-EmWeb) al PJL crudo.
		// Probamos si el pipe USB en realidad es un canal IPP-USB/EWS y responde a HTTP real.
		log.SetFlags(log.Ltime)
		log.SetPrefix("[USB-Agent] ")
		paths, err := usbraw.FindDevicePaths()
		if err != nil || len(paths) == 0 {
			log.Fatalf("[-] No se encontraron impresoras USB: %v\n", err)
		}
		exec.Command("net", "stop", "spooler").Run()
		time.Sleep(2 * time.Second)
		defer exec.Command("net", "start", "spooler").Run()

		reqs := []string{
			"GET / HTTP/1.1\r\nHost: printer\r\nConnection: close\r\n\r\n",
			"GET /hp/device/InternalPages/Index?id=SuppliesStatus HTTP/1.1\r\nHost: printer\r\nConnection: close\r\n\r\n",
			"GET /hp/device/InternalPages/Index?id=ConfigurationPage HTTP/1.1\r\nHost: printer\r\nConnection: close\r\n\r\n",
			"GET /hp/device/InternalPages/Index?id=UsagePage HTTP/1.1\r\nHost: printer\r\nConnection: close\r\n\r\n",
			"GET /hp/device/DeviceInformation/View HTTP/1.1\r\nHost: printer\r\nConnection: close\r\n\r\n",
		}
		for _, path := range paths {
			fmt.Printf("\n=== Device: %s ===\n", path)
			for _, req := range reqs {
				fresh, drained, errReq := usbraw.SendRawAndRead(path, []byte(req), 4000, nil)
				resp := fresh
				if len(resp) == 0 {
					resp = drained
				}
				fmt.Printf("--- Request: %q\n", strings.SplitN(req, "\r\n", 2)[0])
				if errReq != nil {
					fmt.Printf("    error: %v\n", errReq)
					continue
				}
				fmt.Printf("    %d bytes:\n%s\n", len(resp), string(resp))
			}
		}

	default:
		// Modo normal: ventana gráfica.
		// "--tray": la usa el acceso directo de inicio automático (ver
		// setup_script.iss, {commonstartup}) — arranca solo el ícono de
		// bandeja, sin ventana, para no interrumpir el inicio de sesión.
		startHidden := false
		for _, a := range os.Args[1:] {
			if a == "--tray" {
				startHidden = true
				break
			}
		}
		gui.RunWindow(startHidden)
	}
}

func runHeadless() {
	log.SetFlags(log.Ltime)
	log.SetPrefix("[USB-Agent] ")

	cfg := config.LoadPortable()
	cfg.AgentID = config.GetOrCreateAgentID(cfg)

	fmt.Printf("[USB-Agent] Modo headless — AgentID: %s\n", cfg.AgentID)

	if err := agent.Run(cfg, func(line string) { fmt.Println(line) }); err != nil {
		log.Fatalf("Error: %v", err)
	}
}
