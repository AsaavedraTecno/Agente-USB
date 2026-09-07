package pjl

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"usb-agent/internal/payload"
)

// OIDs estándar mapeados a PML
const (
	OidTonerLevel = "1.3.6.1.2.1.43.11.1.1.9.1.1"
	OidTotalPages = "1.3.6.1.2.1.43.10.2.1.4.1.1"
	OidSerial     = "1.3.6.1.2.1.43.5.1.1.17.1"
)

// BuildPMLQuery construye un comando PJL DMCMD ASCIIHEX para leer un OID vía PML.
func BuildPMLQuery(oid string) string {
	var buf bytes.Buffer
	// Header PML
	buf.WriteByte(0x04)
	buf.WriteByte(0x00)
	// Comando: GET (0x04)
	buf.WriteByte(0x04)

	parts := strings.Split(oid, ".")
	for _, p := range parts {
		if n, err := strconv.Atoi(p); err == nil {
			buf.WriteByte(byte(n))
		}
	}

	hexStr := strings.ToUpper(hex.EncodeToString(buf.Bytes()))
	return fmt.Sprintf("@PJL DMCMD ASCIIHEX=\"%s\"", hexStr)
}

// GetBruteForcePMLQueries retorna una lista de comandos PML intentando varios OIDs
// comunes y propietarios de HP para sacar supplies y contadores.
func GetBruteForcePMLQueries() string {
	var cmds []string

	// 1. OIDs SNMP estándar (Toner, Pages, Serial) envueltos en PML
	cmds = append(cmds, BuildPMLQuery(OidTonerLevel))
	cmds = append(cmds, BuildPMLQuery(OidTotalPages))
	cmds = append(cmds, BuildPMLQuery(OidSerial))

	// 2. OIDs HP PML nativos (basados en HPLIP)
	// PML usa OIDs de la rama 2.x.x.x
	// 2.4.3.1.2 = Total Pages
	cmds = append(cmds, `@PJL DMCMD ASCIIHEX="0400040204030102"`)
	// 2.4.3.4.1 = Toner Level
	cmds = append(cmds, `@PJL DMCMD ASCIIHEX="0400040204030401"`)
	// 2.4.3.4.2 = Max Toner Capacity
	cmds = append(cmds, `@PJL DMCMD ASCIIHEX="0400040204030402"`)
	// 2.1.1.2.1 = Model Name
	cmds = append(cmds, `@PJL DMCMD ASCIIHEX="0400040201010201"`)
	// 2.1.1.2.3 = Serial Number
	cmds = append(cmds, `@PJL DMCMD ASCIIHEX="0400040201010203"`)
	// 2.2.1.1.1 = Device Status
	cmds = append(cmds, `@PJL DMCMD ASCIIHEX="0400040202010101"`)
	// 1.1.3.1.4.1.6 = Otra posible página total o reset (exploratorio)
	cmds = append(cmds, `@PJL DMCMD ASCIIHEX="04000401010301040106"`)

	return strings.Join(cmds, "\r\n") + "\r\n"
}

// ExtractPML extrae de la respuesta cruda todos los datos PML devueltos y los mapea al Result.
func ExtractPML(raw []byte, res *Result) {
	lines := strings.Split(string(raw), "\n")
	pmlFound := false

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "@PJL DMCMD ASCIIHEX=") {
			idx := strings.Index(line, "\"")
			if idx != -1 {
				lastIdx := strings.LastIndex(line, "\"")
				if lastIdx > idx {
					hexStr := line[idx+1 : lastIdx]
					decoded, err := hex.DecodeString(hexStr)
					if err == nil && len(decoded) > 3 {
						pmlFound = true
						parsePMLPayload(decoded, res)
					}
				}
			}
		}
	}

	if pmlFound && res.Confidence == "pjl_basic" && (res.PageCount > 0 || len(res.Supplies) > 0) {
		res.Confidence = "pml_usb"
	}
}

// parsePMLPayload decodifica la trama PML (magic, cmd, oid, type, len, val).
func parsePMLPayload(data []byte, res *Result) {
	// Header: 04 00
	if len(data) < 4 || data[0] != 0x04 || data[1] != 0x00 {
		return
	}
	// Comando GET_RESPONSE = 0x05
	if data[2] != 0x05 {
		return
	}

	// Payload empieza en data[3:]
	// Extraer el valor heurísticamente sin importar cuál OID respondió exactamente
	val := extractPMLValue(data[3:])
	if val == nil {
		return
	}

	switch v := val.(type) {
	case int64:
		// Clasificación burda de enteros:
		// Si es <= 100, asumimos que es Toner Level
		// Si es > 100 y < 5.000.000, asumimos que es Total Pages (o max capacity)
		if v >= 0 && v <= 100 {
			if len(res.Supplies) == 0 {
				pct := int(v)
				res.Supplies = append(res.Supplies, payload.Supply{
					Name:       "Toner (PML)",
					Color:      "black",
					Type:       "toner",
					Category:   "toner",
					Percentage: payload.Float64Ptr(float64(pct)),
				})
			}
		} else if v > 100 {
			// Podría ser el Page Count o Toner Max Capacity.
			// Asignaremos al PageCount si está en 0.
			if res.PageCount == 0 {
				res.PageCount = v
			}
		}
	case string:
		// Si es un string largo alfanumérico sin espacios, podría ser MAC o Serial
		vClean := strings.TrimSpace(v)
		if len(vClean) >= 8 {
			if res.Serial == "" || res.Serial == "?" {
				res.Serial = vClean
			}
		}
	}
}

func extractPMLValue(data []byte) interface{} {
	if len(data) < 2 {
		return nil
	}
	dataType := data[0]
	// Omitimos error "No Such Object"
	if dataType == 0x00 || dataType == 0xFF {
		return nil
	}

	dataLen := int(data[1])
	if len(data) < 2+dataLen {
		return nil
	}

	valBytes := data[2 : 2+dataLen]

	// 0x04: Entero, 0x0A: String
	switch dataType {
	case 0x04, 0x01, 0x02: // Tipos numéricos
		var val int64
		for _, b := range valBytes {
			val = (val << 8) | int64(b)
		}
		return val
	case 0x0A, 0x0B: // Strings / Octet Strings
		return string(valBytes)
	default:
		// Fallback heurístico: Si son bytes ASCII, retornamos string
		isAscii := true
		for _, b := range valBytes {
			if b < 32 || b > 126 {
				isAscii = false
				break
			}
		}
		if isAscii && len(valBytes) > 0 {
			return string(valBytes)
		}

		// Fallback: tratar como entero
		var val int64
		for _, b := range valBytes {
			val = (val << 8) | int64(b)
		}
		return val
	}
}
