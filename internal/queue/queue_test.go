package queue

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Purge es lo único que evita que un agente sin contacto con el servidor por
// mucho tiempo (notebook de baja, servidor caído) llene el disco: Save()
// nunca deja de escribir por su cuenta.
func TestPurge_BorraSoloLosArchivosViejos(t *testing.T) {
	dir := t.TempDir()

	viejo := filepath.Join(dir, "viejo.json")
	nuevo := filepath.Join(dir, "nuevo.json")
	if err := os.WriteFile(viejo, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nuevo, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	viejaFecha := time.Now().Add(-100 * time.Hour)
	if err := os.Chtimes(viejo, viejaFecha, viejaFecha); err != nil {
		t.Fatal(err)
	}

	Purge(dir, 72*time.Hour)

	if _, err := os.Stat(viejo); !os.IsNotExist(err) {
		t.Errorf("el archivo viejo debía borrarse, err=%v", err)
	}
	if _, err := os.Stat(nuevo); err != nil {
		t.Errorf("el archivo nuevo NO debía borrarse: %v", err)
	}
}
