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

// Purge borra los archivos de cola más viejos que maxAge -- sin esto, un
// agente que pierde contacto con el servidor por mucho tiempo (notebook de
// baja, servidor caído) llena el disco de a poco, porque Save() nunca deja
// de escribir. Mismo patrón que AgenteSNMP (pkg/uploader/uploader.go,
// purgeOldFiles, TTL 3 días).
func Purge(dir string, maxAge time.Duration) {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return
	}

	cutoff := time.Now().Add(-maxAge)
	deleted := 0
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if os.Remove(f) == nil {
				deleted++
			}
		}
	}

	if deleted > 0 {
		log.Printf("[Queue] Limpieza: %d archivo(s) más viejos que %v eliminados", deleted, maxAge)
	}
}
