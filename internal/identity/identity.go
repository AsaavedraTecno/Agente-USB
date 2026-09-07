// Package identity resuelve la identidad física de ESTA máquina — distinta
// de la api_key, que es la credencial compartida por todas las instalaciones
// de un mismo cliente.
package identity

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const fileName = "machine_id"

// GetOrCreateMachineID devuelve el UUID v4 persistido en <dir>/machine_id,
// generándolo la primera vez que corre. A diferencia de un id derivado del
// hostname, no colisiona entre dos máquinas clonadas de la misma imagen y
// sobrevive a un cambio de nombre de host. Vive en un archivo propio, no
// dentro de agent.yaml: reinstalar o pisar la config no debe poder borrar
// la identidad por accidente.
//
// Reinstalar desde cero (archivo borrado) genera un id nuevo a propósito:
// una máquina reimageada es, para efectos de monitoreo, una máquina distinta.
func GetOrCreateMachineID(dir string) (string, error) {
	path := filepath.Join(dir, fileName)

	if data, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(data)); id != "" {
			return id, nil
		}
	}

	id, err := newUUIDv4()
	if err != nil {
		return "", fmt.Errorf("generar machine_id: %w", err)
	}

	if err := os.WriteFile(path, []byte(id), 0644); err != nil {
		return "", fmt.Errorf("guardar machine_id en %s: %w", path, err)
	}

	return id, nil
}

func newUUIDv4() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // versión 4
	b[8] = (b[8] & 0x3f) | 0x80 // variante RFC 4122
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
