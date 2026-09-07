package hpprotocol

import (
	"encoding/binary"
	"errors"
	"strconv"
)

// Constantes de comandos del protocolo HP 408dn
const (
	CmdDeviceID    = 0xA1000000 // Device ID IEEE 1284
	CmdSoftReset   = 0xC1020000 // 8 bytes de status
	CmdStatusQuery = 0xC10D0600 // 8 bytes + XML streaming
)

// Info contiene la información parseada del estatus de la impresora
type Info struct {
	TonerPct     int
	ImagingPages int
}

// RequestBuilder implementa un patrón fluent para construir requests de 32 bytes
type RequestBuilder struct {
	sessionID uint16
	command   uint32
}

func NewRequestBuilder() *RequestBuilder {
	return &RequestBuilder{}
}

func (r *RequestBuilder) WithSessionID(sessionID uint16) *RequestBuilder {
	r.sessionID = sessionID
	return r
}

func (r *RequestBuilder) WithCommand(command uint32) *RequestBuilder {
	r.command = command
	return r
}

// Build construye el payload binario.
// Offsets documentados:
// 0x00: Magic (0x60, 0x6A)
// 0x06: Protocol ID (0x01E5)
// 0x08: Session ID
// 0x18: CmdCode (0x0208)
// 0x1C: CommandData (4 bytes)
func (r *RequestBuilder) Build() []byte {
	buf := make([]byte, 32)
	buf[0] = 0x60
	buf[1] = 0x6A

	// Protocol ID 0x01E5 (Big Endian)
	binary.BigEndian.PutUint16(buf[6:], 0x01E5)

	// Session ID
	binary.BigEndian.PutUint16(buf[8:], r.sessionID)

	// CmdCode siempre 0x0208
	binary.BigEndian.PutUint16(buf[0x18:], 0x0208)

	// CommandData en el offset 0x1C (28)
	binary.BigEndian.PutUint32(buf[0x1C:], r.command)

	return buf
}

// ParseStatusResponse extrae los valores de toner e imaging pages desde el frame 16 de 8 bytes
// Frame esperado (ejemplo): 1b 23 00 00 5b d7 00 00
func ParseStatusResponse(data []byte) (*Info, error) {
	if len(data) < 8 {
		return nil, errors.New("respuesta demasiado corta")
	}

	// Byte 4: Toner % (escala directa, 91 ≈ 92%)
	// Se le suma 1 al valor RAW para coincidir con el panel de HP según la hipótesis
	tonerPct := int(data[4]) + 1
	if tonerPct > 100 {
		tonerPct = 100
	}

	// Byte 5: Imaging pages. Escala: valor * 64
	imagingPages := int(data[5]) * 64

	return &Info{
		TonerPct:     tonerPct,
		ImagingPages: imagingPages,
	}, nil
}

// ExtractCRUMDate extrae la fecha de un string CRUM (ej: CRUM-240112A8FA1)
func ExtractCRUMDate(crum string) (year, month, day int, err error) {
	if len(crum) < 11 || crum[:5] != "CRUM-" {
		return 0, 0, 0, errors.New("formato CRUM inválido")
	}

	dateStr := crum[5:11]

	y, err := strconv.Atoi(dateStr[:2])
	if err != nil {
		return 0, 0, 0, err
	}

	m, err := strconv.Atoi(dateStr[2:4])
	if err != nil {
		return 0, 0, 0, err
	}

	d, err := strconv.Atoi(dateStr[4:6])
	if err != nil {
		return 0, 0, 0, err
	}

	// Asumimos año 2000+
	return 2000 + y, m, d, nil
}

// ParseDeviceIDResponse es un placeholder para extraer el Device ID del frame de respuesta
func ParseDeviceIDResponse(data []byte) (string, error) {
	// Eliminar cualquier header binario y devolver ASCII imprimible
	var ascii []byte
	for _, b := range data {
		if b >= 0x20 && b <= 0x7E {
			ascii = append(ascii, b)
		}
	}
	if len(ascii) == 0 {
		return "", errors.New("sin device id")
	}
	return string(ascii), nil
}

// ReassembleXML es un placeholder para ensamblar fragmentos
func ReassembleXML(fragments [][]byte) (string, error) {
	var full string
	for _, f := range fragments {
		full += string(f)
	}
	return full, nil
}
