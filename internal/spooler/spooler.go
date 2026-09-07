package spooler

import (
	"fmt"
	"os/exec"
	"syscall"
)

// Stop detiene el Windows Print Spooler. Requiere administrador.
func Stop() error {
	cmd := exec.Command("net", "stop", "spooler")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("error deteniendo spooler: %w", err)
	}
	return nil
}

// Start inicia el Windows Print Spooler. Requiere administrador.
func Start() error {
	cmd := exec.Command("net", "start", "spooler")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("error iniciando spooler: %w", err)
	}
	return nil
}
