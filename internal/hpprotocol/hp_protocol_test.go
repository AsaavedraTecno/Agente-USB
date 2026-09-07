package hpprotocol_test

import (
	"fmt"
	"usb-agent/internal/hpprotocol"
)

// Este archivo sirve como ejemplo de uso del paquete hpprotocol.
// No se ejecutará durante los tests regulares a menos que se defina ExampleMain().
// Muestra cómo construir un request y parsear una respuesta.

func Example() {
	// Construir requests
	req := hpprotocol.NewRequestBuilder().
		WithCommand(hpprotocol.CmdStatusQuery).
		WithSessionID(0x0F89).
		Build()

	fmt.Printf("Request payload size: %d bytes\n", len(req))

	// Supongamos que obtenemos estos 8 bytes de la impresora
	data := []byte{0x1b, 0x23, 0x00, 0x00, 0x5b, 0xd7, 0x00, 0x00}

	// Parsear respuestas
	info, err := hpprotocol.ParseStatusResponse(data)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}

	fmt.Println(info.TonerPct)     // 92
	fmt.Println(info.ImagingPages) // 13760

	// Extraer fechas CRUM
	year, mon, day, _ := hpprotocol.ExtractCRUMDate("CRUM-240112A8FA1")
	fmt.Printf("%d-%02d-%02d\n", year, mon, day)

	// Output:
	// Request payload size: 32 bytes
	// 92
	// 13760
	// 2024-01-12
}
