# Paquete `hpprotocol`

El paquete `hpprotocol` implementa la capa de comunicación y decodificación para el protocolo USB propietario de la impresora **HP LaserJet 408dn**.

## Arquitectura del Protocolo

El protocolo utiliza un header fijo de 32 bytes estructurado de la siguiente forma (offsets documentados):

| Offset | Tamaño | Descripción |
|---|---|---|
| `0x00` | 2 bytes | Magic Number (`0x60`, `0x6A`) |
| `0x06` | 2 bytes | Protocol ID (`0x01E5`) |
| `0x08` | 2 bytes | Session ID |
| `0x18` | 2 bytes | Command Code fijo (`0x0208`) |
| `0x1C` | 4 bytes | Command Data / Selector real del comando |

### Comandos Conocidos (`CommandData`):
- `0xA1000000` (`CmdDeviceID`): Obtiene el Device ID (IEEE 1284).
- `0xC1020000` (`CmdSoftReset`): Devuelve 8 bytes de estado de consumibles rápidos.
- `0xC10D0600` (`CmdStatusQuery`): Devuelve los 8 bytes de estado y luego transmite un flujo fragmentado de XML.

## Uso Básico

```go
package main

import (
    "fmt"
    "usb-agent/internal/hpprotocol"
)

func main() {
    // 1. Construir un request usando el Fluent Builder
    req := hpprotocol.NewRequestBuilder().
        WithSessionID(0x0F89).
        WithCommand(hpprotocol.CmdStatusQuery).
        Build()

    // Enviar 'req' por USB (vía gousb)...
    
    // 2. Parsear el Status Response (Frame 16: 8 bytes)
    data := []byte{0x1b, 0x23, 0x00, 0x00, 0x5b, 0xd7, 0x00, 0x00}
    info, err := hpprotocol.ParseStatusResponse(data)
    if err == nil {
        fmt.Printf("Toner: %d%%\n", info.TonerPct)
        fmt.Printf("Imaging Unit: %d páginas\n", info.ImagingPages)
    }
}
```

## Pruebas (Tests Unitarios)
Para ejecutar todas las validaciones de hipótesis de escalado, CRUM y builders, corre:
```bash
go test -v ./internal/hpprotocol
```
