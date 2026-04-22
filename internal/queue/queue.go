package queue

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// Save persiste el payload JSON en disco para reintento posterior.
func Save(dir string, data []byte) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("crear directorio de cola: %w", err)
	}

	name := filepath.Join(dir, fmt.Sprintf("%d.json", time.Now().UnixNano()))
	if err := os.WriteFile(name, data, 0644); err != nil {
		return fmt.Errorf("escribir archivo de cola: %w", err)
	}

	log.Printf("[Queue] Guardado en %s", name)
	return nil
}

// ListPending devuelve todos los archivos JSON pendientes en el directorio de cola.
func ListPending(dir string) ([]string, error) {
	return filepath.Glob(filepath.Join(dir, "*.json"))
}

// Remove elimina un archivo de cola exitosamente enviado.
func Remove(path string) error {
	return os.Remove(path)
}
