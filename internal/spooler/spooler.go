package spooler

import (
	"fmt"
	"os/exec"
)

// Stop detiene el servicio de cola de impresión (Spooler) de Windows.
// Requiere privilegios de administrador.
func Stop() error {
	cmd := exec.Command("net", "stop", "spooler")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("error deteniendo spooler: %w", err)
	}
	return nil
}

// Start inicia el servicio de cola de impresión (Spooler) de Windows.
// Requiere privilegios de administrador.
func Start() error {
	cmd := exec.Command("net", "start", "spooler")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("error iniciando spooler: %w", err)
	}
	return nil
}
