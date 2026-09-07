// Package snmpusb detecta la IP virtual que el driver USB de la impresora crea en Windows
// y ejecuta extracción SNMP completa sobre ella, reutilizando el mismo protocolo que el
// agente de red existente.
package snmpusb

import (
	"fmt"
	"log"
	"math/big"
	"net"
	"strings"
	"time"

	"usb-agent/internal/payload"

	"github.com/gosnmp/gosnmp"
)

// OIDs del Printer MIB (RFC 3805) y MIB-II usados en la extracción
const (
	oidSysDescr  = ".1.3.6.1.2.1.1.1.0"
	oidSysName   = ".1.3.6.1.2.1.1.5.0"
	oidPrtModel  = ".1.3.6.1.2.1.43.5.1.1.16.1"
	oidPrtSerial = ".1.3.6.1.2.1.43.5.1.1.17.1"
	oidLifeCount = ".1.3.6.1.2.1.43.10.2.1.4.1.1"
	oidHrDescr   = ".1.3.6.1.2.1.25.3.2.1.3.1"
	oidIfMAC     = ".1.3.6.1.2.1.2.2.1.6.1"
	oidDisplay   = ".1.3.6.1.2.1.43.16.5.1.2.1.1"
)

// OIDs de insumos (supply slots 1..4)
var supplyOIDsPerSlot = []struct{ level, maxCap, desc string }{
	{".1.3.6.1.2.1.43.11.1.1.9.1.1", ".1.3.6.1.2.1.43.11.1.1.8.1.1", ".1.3.6.1.2.1.43.11.1.1.6.1.1"},
	{".1.3.6.1.2.1.43.11.1.1.9.1.2", ".1.3.6.1.2.1.43.11.1.1.8.1.2", ".1.3.6.1.2.1.43.11.1.1.6.1.2"},
	{".1.3.6.1.2.1.43.11.1.1.9.1.3", ".1.3.6.1.2.1.43.11.1.1.8.1.3", ".1.3.6.1.2.1.43.11.1.1.6.1.3"},
	{".1.3.6.1.2.1.43.11.1.1.9.1.4", ".1.3.6.1.2.1.43.11.1.1.8.1.4", ".1.3.6.1.2.1.43.11.1.1.6.1.4"},
}

// Result contiene los datos extraídos vía SNMP-USB.
type Result struct {
	IP         string
	SysDescr   string
	Model      string
	Brand      string
	Serial     string
	MAC        string
	Display    string
	TotalPages int64
	Supplies   []payload.Supply
	Alerts     []payload.Alert
}

// DetectVirtualIP escanea todas las interfaces locales buscando una impresora que responda SNMP.
// Retorna la primera IP que responda y true, o "" y false si no se encuentra ninguna.
func DetectVirtualIP(community string, timeoutMs int) (string, bool) {
	candidates := localNonLoopbackIPs()
	probe := time.Duration(timeoutMs/4) * time.Millisecond
	if probe < 300*time.Millisecond {
		probe = 300 * time.Millisecond
	}

	log.Printf("[SNMP-USB] Escaneando %d IPs locales (timeout por IP: %v)...", len(candidates), probe)

	for _, ip := range candidates {
		if isPrinterAt(ip, community, probe) {
			log.Printf("[SNMP-USB] Impresora encontrada en %s", ip)
			return ip, true
		}
	}

	return "", false
}

// Extract realiza la extracción SNMP completa desde la IP dada.
func Extract(ip, community string, timeoutMs int) (*Result, error) {
	timeout := time.Duration(timeoutMs) * time.Millisecond
	g := newClient(ip, community, timeout)

	if err := g.Connect(); err != nil {
		return nil, fmt.Errorf("conectar a %s:161: %w", ip, err)
	}
	defer g.Conn.Close()

	log.Printf("[SNMP-USB] Extrayendo datos de %s...", ip)

	// ── Consulta principal ─────────────────────────────────────────────────
	mainOIDs := []string{oidSysDescr, oidSysName, oidPrtModel, oidPrtSerial, oidLifeCount, oidHrDescr, oidIfMAC, oidDisplay}
	resp, err := g.Get(mainOIDs)
	if err != nil {
		return nil, fmt.Errorf("SNMP GET: %w", err)
	}

	res := &Result{IP: ip}

	// Mapear por posición (gosnmp preserva el orden de los OIDs solicitados)
	for i, v := range resp.Variables {
		if v.Type == gosnmp.NoSuchObject || v.Type == gosnmp.NoSuchInstance {
			continue
		}
		switch i {
		case 0:
			res.SysDescr = oidStr(v)
		case 1:
			if res.Model == "" {
				res.Model = oidStr(v)
			}
		case 2:
			if s := oidStr(v); s != "" {
				res.Model = s
			}
		case 3:
			res.Serial = oidStr(v)
		case 4:
			res.TotalPages = oidInt64(v)
		case 5:
			if res.Model == "" {
				res.Model = oidStr(v)
			}
		case 6:
			res.MAC = formatMAC(v)
		case 7:
			res.Display = oidStr(v)
		}
	}

	if res.Model == "" {
		res.Model = res.SysDescr
	}
	res.Brand = inferBrand(res.Model + " " + res.SysDescr)

	// ── Insumos ────────────────────────────────────────────────────────────
	res.Supplies = extractSupplies(g)

	return res, nil
}

// extractSupplies recorre los slots de insumos del Printer MIB.
func extractSupplies(g *gosnmp.GoSNMP) []payload.Supply {
	var supplies []payload.Supply

	for i, slot := range supplyOIDsPerSlot {
		resp, err := g.Get([]string{slot.level, slot.maxCap, slot.desc})
		if err != nil {
			break
		}
		if len(resp.Variables) == 0 {
			break
		}

		lv := resp.Variables[0]
		if lv.Type == gosnmp.NoSuchObject || lv.Type == gosnmp.NoSuchInstance {
			break
		}

		level := oidInt64(lv)
		if level < 0 { // -1 = unknown, -3 = other
			continue
		}

		maxCap := int64(0)
		if len(resp.Variables) > 1 {
			maxCap = oidInt64(resp.Variables[1])
		}

		desc := fmt.Sprintf("Insumo %d", i+1)
		if len(resp.Variables) > 2 {
			if s := oidStr(resp.Variables[2]); s != "" {
				desc = s
			}
		}

		var levelPct *int
		if maxCap > 0 {
			pct := int((level * 100) / maxCap)
			levelPct = &pct
		} else if level >= 0 && level <= 100 {
			pct := int(level)
			levelPct = &pct
		}

		status := "ok"
		if levelPct != nil {
			switch {
			case *levelPct <= 10:
				status = "Cr\u00edtico"
			case *levelPct <= 25:
				status = "Bajo"
			}
		}

		supplies = append(supplies, payload.Supply{
			Name:       desc,
			Type:       "toner",
			Category:   "toner",
			Color:      "black",
			Percentage: payload.Float64Ptr(float64(*levelPct)),
			Status:     status,
		})
	}

	return supplies
}

// ── Helpers ────────────────────────────────────────────────────────────────

func localNonLoopbackIPs() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}

	seen := map[string]bool{}
	var ips []string

	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() || ip.To4() == nil {
				continue
			}
			s := ip.String()
			if !seen[s] {
				ips = append(ips, s)
				seen[s] = true
			}
		}
	}

	return ips
}

func isPrinterAt(ip, community string, timeout time.Duration) bool {
	g := newClient(ip, community, timeout)
	if err := g.Connect(); err != nil {
		return false
	}
	defer g.Conn.Close()

	resp, err := g.Get([]string{oidSysDescr})
	if err != nil || len(resp.Variables) == 0 {
		return false
	}

	v := resp.Variables[0]
	if v.Type != gosnmp.OctetString {
		return false
	}

	desc := strings.ToLower(string(v.Value.([]byte)))
	keywords := []string{"print", "samsung", "hp", "xerox", "canon", "epson", "brother", "ricoh", "lexmark", "kyocera", "laser", "inkjet"}
	for _, kw := range keywords {
		if strings.Contains(desc, kw) {
			return true
		}
	}

	return false
}

func newClient(ip, community string, timeout time.Duration) *gosnmp.GoSNMP {
	return &gosnmp.GoSNMP{
		Target:    ip,
		Port:      161,
		Community: community,
		Version:   gosnmp.Version2c,
		Timeout:   timeout,
		Retries:   1,
		MaxOids:   gosnmp.MaxOids,
	}
}

func oidStr(v gosnmp.SnmpPDU) string {
	if v.Type == gosnmp.OctetString {
		return strings.TrimSpace(string(v.Value.([]byte)))
	}
	return ""
}

func oidInt64(v gosnmp.SnmpPDU) int64 {
	if v.Value == nil {
		return 0
	}
	if b, ok := v.Value.(*big.Int); ok {
		return b.Int64()
	}
	return gosnmp.ToBigInt(v.Value).Int64()
}

func formatMAC(v gosnmp.SnmpPDU) string {
	if v.Type != gosnmp.OctetString {
		return ""
	}
	b, ok := v.Value.([]byte)
	if !ok || len(b) != 6 {
		return ""
	}
	return fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X", b[0], b[1], b[2], b[3], b[4], b[5])
}

func inferBrand(desc string) string {
	d := strings.ToLower(desc)
	switch {
	case strings.Contains(d, "samsung"):
		return "Samsung"
	case strings.Contains(d, "hewlett") || strings.Contains(d, " hp ") || strings.HasPrefix(d, "hp"):
		return "HP"
	case strings.Contains(d, "xerox"):
		return "Xerox"
	case strings.Contains(d, "canon"):
		return "Canon"
	case strings.Contains(d, "epson"):
		return "Epson"
	case strings.Contains(d, "brother"):
		return "Brother"
	case strings.Contains(d, "ricoh"):
		return "Ricoh"
	case strings.Contains(d, "lexmark"):
		return "Lexmark"
	case strings.Contains(d, "kyocera"):
		return "Kyocera"
	case strings.Contains(d, "konica"):
		return "Konica Minolta"
	case strings.Contains(d, "oki"):
		return "OKI"
	default:
		return "Unknown"
	}
}
