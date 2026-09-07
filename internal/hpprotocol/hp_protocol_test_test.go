package hpprotocol_test

import (
	"encoding/binary"
	"testing"

	"usb-agent/internal/hpprotocol"
)

func TestBuildRequest(t *testing.T) {
	req := hpprotocol.NewRequestBuilder().
		WithSessionID(0x0F89).
		WithCommand(hpprotocol.CmdStatusQuery).
		Build()

	if len(req) != 32 {
		t.Fatalf("Tamaño esperado 32, obtuve %d", len(req))
	}

	if req[0] != 0x60 || req[1] != 0x6A {
		t.Errorf("Magic erróneo: %02X %02X", req[0], req[1])
	}

	protocolID := binary.BigEndian.Uint16(req[6:])
	if protocolID != 0x01E5 {
		t.Errorf("Protocol ID erróneo: %04X", protocolID)
	}

	sessionID := binary.BigEndian.Uint16(req[8:])
	if sessionID != 0x0F89 {
		t.Errorf("Session ID erróneo: %04X", sessionID)
	}

	cmdCode := binary.BigEndian.Uint16(req[0x18:])
	if cmdCode != 0x0208 {
		t.Errorf("CmdCode erróneo: %04X", cmdCode)
	}

	command := binary.BigEndian.Uint32(req[0x1C:])
	if command != hpprotocol.CmdStatusQuery {
		t.Errorf("CommandData erróneo: %08X", command)
	}
}

func TestParseStatusResponse(t *testing.T) {
	// Frame 16: 1b 23 00 00 5b d7 00 00
	data := []byte{0x1b, 0x23, 0x00, 0x00, 0x5b, 0xd7, 0x00, 0x00}

	info, err := hpprotocol.ParseStatusResponse(data)
	if err != nil {
		t.Fatalf("Error inesperado: %v", err)
	}

	// 0x5B = 91 -> Esperamos 92% (91 + 1)
	if info.TonerPct != 92 {
		t.Errorf("Toner esperado 92, obtuve %d", info.TonerPct)
	}

	// 0xD7 = 215 -> Esperamos 13760 (215 * 64)
	if info.ImagingPages != 13760 {
		t.Errorf("Imaging pages esperado 13760, obtuve %d", info.ImagingPages)
	}
}

func TestCRUMDateExtraction(t *testing.T) {
	y, m, d, err := hpprotocol.ExtractCRUMDate("CRUM-240112A8FA1")
	if err != nil {
		t.Fatalf("Error: %v", err)
	}
	if y != 2024 || m != 1 || d != 12 {
		t.Errorf("Fecha errónea, obtuve %d-%d-%d", y, m, d)
	}
}

func TestCommandDataVariations(t *testing.T) {
	cmds := []uint32{
		hpprotocol.CmdDeviceID,
		hpprotocol.CmdSoftReset,
		hpprotocol.CmdStatusQuery,
	}

	for _, cmd := range cmds {
		req := hpprotocol.NewRequestBuilder().WithCommand(cmd).Build()
		c := binary.BigEndian.Uint32(req[0x1C:])
		if c != cmd {
			t.Errorf("Variación fallida. Esperaba %08X, obtuve %08X", cmd, c)
		}
	}
}

func TestByteScalingHypothesis(t *testing.T) {
	// 91 = 92%
	data := make([]byte, 8)
	data[4] = 91
	info, _ := hpprotocol.ParseStatusResponse(data)
	if info.TonerPct != 92 {
		t.Errorf("Escalado toner falló. Obtenido: %d", info.TonerPct)
	}

	// 215 = 13760
	data[5] = 215
	info, _ = hpprotocol.ParseStatusResponse(data)
	if info.ImagingPages != 13760 {
		t.Errorf("Escalado imaging falló. Obtenido: %d", info.ImagingPages)
	}
}

func TestParseDeviceIDResponse(t *testing.T) {
	data := []byte{0x00, 0x10, 'M', 'F', 'G', ':', 'H', 'P', ';'}
	id, err := hpprotocol.ParseDeviceIDResponse(data)
	if err != nil {
		t.Fatalf("Error: %v", err)
	}
	if id != "MFG:HP;" {
		t.Errorf("Device ID erróneo, obtuve %s", id)
	}
}

func TestXMLFragmentReassembly(t *testing.T) {
	frags := [][]byte{
		[]byte("<?xml version=\"1.0\"?>"),
		[]byte("<status>ok</status>"),
	}
	full, _ := hpprotocol.ReassembleXML(frags)
	if full != "<?xml version=\"1.0\"?><status>ok</status>" {
		t.Errorf("Reassembly falló: %s", full)
	}
}
